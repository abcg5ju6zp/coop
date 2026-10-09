package cli

// A recoverable `coop update` generation change. The verified new binary is first STAGED as a
// candidate, the candidate binary itself builds and stamps the matching box image (so the image
// comes from that release's embedded box definition, not the running binary's), and only then is
// the candidate renamed over the active executable and the supervised boxes restarted onto the
// new image. Everything between stages is recorded in update-generation.json, so a process that
// dies at any point is either continued or safely rolled back by the next ordinary startup or
// update. The state split:
//
//   - Candidate: a downloaded, checksum-verified binary staged beside the executable, possibly
//     with its image already built. The active binary does not change until the rename — discarding
//     the candidate (file + record) is a complete rollback to the old binary+image combination.
//   - PendingRecycle: supervised boxes snapshotted at the switch, restarted by exact container id
//     after the rename. Only those recorded objects are ever removed, so a retry never kills a box
//     a new launch already started on the new image. An unsupervised box is never recorded here.
//
// The candidate drives the image build as a child process (`internal box-plan` /
// `internal box-build`): its own Main resolves its own base-image tag and embedded client lock.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

const updateGenerationFile = "update-generation.json"

type candidatePhase string

const (
	phaseStaged     candidatePhase = "staged"      // verified binary staged; its image not built yet
	phaseImageReady candidatePhase = "image-ready" // the candidate's own image build succeeded and was verified
)

// candidateState is one not-yet-active generation. Times are bookkeeping; the version and the
// binary hash are what a resume trusts.
type candidateState struct {
	Version   string         `json:"version"`              // normalized release version, e.g. "2.7.3"
	Tag       string         `json:"tag"`                  // the release tag, e.g. "v2.7.3"
	Exe       string         `json:"exe"`                  // active executable path the rename targets
	Staged    string         `json:"staged"`               // staged candidate executable
	SHA256    string         `json:"sha256"`               // sha256 of the staged binary bytes
	Phase     candidatePhase `json:"phase"`                // how far the generation got
	Repo      string         `json:"repo,omitempty"`       // repo the image was planned/built for
	BaseImage string         `json:"base_image,omitempty"` // the candidate's resolved managed base tag
	Image     string         `json:"image,omitempty"`      // image tag the candidate build produced
	Project   bool           `json:"project,omitempty"`    // the image is the project's own, not the shared base
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// pendingRecycle is the set of supervised boxes one committed generation still owes a restart.
// The version guards against an external downgrade: boxes recorded for a newer generation are not
// reaped while an older binary is active.
type pendingRecycle struct {
	Version   string    `json:"version"`
	IDs       []string  `json:"ids"`
	Error     string    `json:"error,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type generationState struct {
	Format    int             `json:"format"`
	Candidate *candidateState `json:"candidate,omitempty"`
	Pending   *pendingRecycle `json:"pending_recycle,omitempty"`
}

func generationStatePath(cfg *config.Config) string {
	return filepath.Join(cfg.BoxHome, updateGenerationFile)
}

func emptyGenerationState() *generationState { return &generationState{Format: 1} }

// loadGenerationState reads the persisted generation. A missing file is an empty state, not an
// error; a malformed file is quarantined (never deleted — it is evidence) and also reads as empty,
// so one bad write cannot wedge every later startup.
func loadGenerationState(cfg *config.Config) *generationState {
	path := generationStatePath(cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		return emptyGenerationState()
	}
	st := emptyGenerationState()
	if err := json.Unmarshal(data, st); err != nil || st.Format != 1 {
		bad := fmt.Sprintf("%s.bad-%d", path, time.Now().UnixNano())
		if renameErr := os.Rename(path, bad); renameErr != nil {
			_ = os.Remove(path)
		}
		return emptyGenerationState()
	}
	return st
}

// save writes the state atomically (temp + fsync + rename), or removes the file once it describes
// no generation at all.
func (st *generationState) save(cfg *config.Config) error {
	if st.Candidate == nil && st.Pending == nil {
		if err := os.Remove(generationStatePath(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if st.Format == 0 {
		st.Format = 1
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(generationStatePath(cfg), data)
}

// healUpdateGeneration is the silent startup reconciliation. It never touches the network, the
// runtime, or an active binary:
//
//   - candidate present, running binary IS the candidate: the rename landed but the process died
//     before clearing the record. Clear it (the staged path was renamed away) and KEEP the pending
//     recycle, which the running generation now owes.
//   - candidate present, running binary is older: the switch never happened, so the active
//     binary+image combination was never disturbed and there is nothing to undo. Leave the record
//     for the next `coop update` to resume (the stage is byte-verified) or to discard when a newer
//     release superseded it; an ordinary command must not delete a resumable generation.
//   - pending for a binary older than the recorded generation (an external downgrade): keep it and
//     do nothing — only its own generation may reap those boxes.
func healUpdateGeneration(cfg *config.Config) {
	st := loadGenerationState(cfg)
	c := st.Candidate
	if c == nil {
		return
	}
	running := resolveVersion()
	if !(releaseVersion(running) && normalizeVersion(running) == c.Version) {
		return // unswitched candidate: left for the next update to resume or supersede
	}
	_ = os.Remove(c.Staged) // the rename consumed the stage; a leftover at this path is stale
	st.Candidate = nil
	_ = st.save(cfg)
}

// candidateStagedPath is the stable stage location, beside the executable so the activating
// rename is same-filesystem atomic. A fixed name (not a temp suffix) makes a resume find the stage.
func candidateStagedPath(exe, version string) string {
	return filepath.Join(filepath.Dir(exe), ".coop-candidate-"+normalizeVersion(version))
}

// candidateIntact proves a staged candidate is still exactly the release it claims: present and
// byte-identical to the checksum recorded when it was downloaded.
func candidateIntact(c *candidateState) bool {
	if c == nil || c.SHA256 == "" {
		return false
	}
	f, err := os.Open(c.Staged)
	if err != nil {
		return false
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, io.LimitReader(f, maxReleaseBinaryBytes+1)); err != nil {
		return false
	}
	return hex.EncodeToString(sum.Sum(nil)) == c.SHA256
}

// discardCandidate abandons an uncommitted generation: staged binary removed, record cleared. A
// pending armed for this same candidate only exists when the rename did not land, so it goes too.
func (st *generationState) discardCandidate(cfg *config.Config) {
	if c := st.Candidate; c != nil {
		_ = os.Remove(c.Staged)
		if st.Pending != nil && st.Pending.Version == c.Version {
			st.Pending = nil
		}
		st.Candidate = nil
	}
	_ = st.save(cfg)
}

// stageOrResumeCandidate returns a verified candidate for tag, reusing an interrupted stage when
// it is still byte-identical to the release offered, instead of trusting or re-downloading it.
// The active executable is never touched; a failure here leaves the record without a candidate.
func (st *generationState) stageOrResumeCandidate(cfg *config.Config, exe, tag string) (*candidateState, error) {
	version := normalizeVersion(tag)
	if c := st.Candidate; c != nil {
		if c.Version == version && c.Exe == exe && candidateIntact(c) {
			return c, nil
		}
		st.discardCandidate(cfg)
	}
	staged := candidateStagedPath(exe, version)
	sum, err := stageReleaseBinary(staged, tag)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	c := &candidateState{
		Version: version, Tag: tag, Exe: exe, Staged: staged, SHA256: sum,
		Phase: phaseStaged, CreatedAt: now, UpdatedAt: now,
	}
	st.Candidate = c
	if err := st.save(cfg); err != nil {
		_ = os.Remove(staged)
		st.Candidate = nil
		return nil, &installFailure{reason: "Could not record the Coop update state: " + osCause(err) + ".", unchanged: true, err: err}
	}
	return c, nil
}

// markCandidateImageReady records the image the candidate built once the running process verified
// it exists, so a later resume does not rebuild it.
func (st *generationState) markCandidateImageReady(cfg *config.Config, c *candidateState, repo string, plan internalPlanResult) error {
	c.Phase = phaseImageReady
	c.Repo = repo
	c.BaseImage = plan.BaseImage
	c.Image = plan.Image
	c.Project = plan.Project
	c.UpdatedAt = time.Now().UTC()
	st.Candidate = c
	return st.save(cfg)
}

// armPending records the boxes to restart BEFORE the binary rename. If the rename fails (or the
// process dies in that window), cmdUpdate drops this arm on the failed rename and a later update
// discards it together with the unswitched candidate; startup heal leaves the record untouched for
// that resolution.
func (st *generationState) armPending(c *candidateState, ids []string) {
	st.Pending = &pendingRecycle{Version: c.Version, IDs: ids, UpdatedAt: time.Now().UTC()}
}

// pendingMatchesRunning reports whether the recorded boxes belong to the binary currently running —
// the only case in which removing them reconnects onto the recorded (new) image.
func (st *generationState) pendingMatchesRunning() bool {
	p := st.Pending
	if p == nil {
		return false
	}
	running := resolveVersion()
	return releaseVersion(running) && normalizeVersion(running) == p.Version
}

// --- candidate child process ---------------------------------------------------------------

// runCandidateInternal executes the staged candidate's hidden internal subcommand, streaming its
// stdio (so a docker build is interactive and visible exactly like an in-process one), and reads
// its machine result file. A var so tests can substitute the child without a real release binary.
var runCandidateInternal = func(cfg *config.Config, staged, subcommand string) ([]byte, error) {
	f, err := os.CreateTemp(cfg.BoxHome, ".coop-update-result-*")
	if err != nil {
		return nil, err
	}
	resultPath := f.Name()
	_ = f.Close()
	defer os.Remove(resultPath)
	cmd := exec.Command(staged, "internal", subcommand, "--result-file", resultPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ() // COOP_REPO, COOP_RUNTIME, config and auth all resolve in the child identically
	runErr := cmd.Run()
	data, readErr := os.ReadFile(resultPath)
	if runErr != nil {
		return data, fmt.Errorf("the staged Coop %s step failed: %w", subcommand, runErr)
	}
	if readErr != nil {
		return nil, errors.New("the staged Coop build reported no result")
	}
	return data, nil
}

// internalResult is the candidate child's one machine-readable answer, for both subcommands. On a
// failure the child streams the build diagnostics to stderr as usual and names the cause here.
type internalResult struct {
	Version                 string `json:"version,omitempty"`
	BaseImage               string `json:"base_image,omitempty"`
	Image                   string `json:"image,omitempty"`
	Project                 bool   `json:"project,omitempty"`
	Dockerfile              string `json:"dockerfile,omitempty"`
	CarriesQualifiedClients *bool  `json:"carries_qualified_clients,omitempty"`
	Error                   string `json:"error,omitempty"`
}

// internalPlanResult is what the parent needs from a plan: the image the candidate will build and
// the facts the post-update report names.
type internalPlanResult struct {
	Version                 string
	BaseImage               string
	Image                   string
	Project                 bool
	Dockerfile              string
	CarriesQualifiedClients bool
}

func runCandidatePlan(cfg *config.Config, staged string) (internalPlanResult, error) {
	return parseCandidatePlan(runCandidateInternal(cfg, staged, "box-plan"))
}

func parseCandidatePlan(data []byte, err error) (internalPlanResult, error) {
	if err != nil {
		return internalPlanResult{}, candidateChildError(data, err)
	}
	var res internalResult
	if jsonErr := json.Unmarshal(data, &res); jsonErr != nil {
		return internalPlanResult{}, errors.New("the staged Coop build reported an unreadable plan")
	}
	if res.Error != "" {
		return internalPlanResult{}, errors.New(res.Error)
	}
	if res.Image == "" {
		return internalPlanResult{}, errors.New("the staged Coop build reported an unreadable plan")
	}
	qualified := res.CarriesQualifiedClients != nil && *res.CarriesQualifiedClients
	return internalPlanResult{
		Version: res.Version, BaseImage: res.BaseImage, Image: res.Image,
		Project: res.Project, Dockerfile: res.Dockerfile, CarriesQualifiedClients: qualified,
	}, nil
}

// runCandidateBuild performs the fresh image build in the candidate process and verifies the
// answer names the image the parent planned.
func runCandidateBuild(cfg *config.Config, staged, wantImage string) error {
	data, err := runCandidateInternal(cfg, staged, "box-build")
	if err != nil {
		return candidateChildError(data, err)
	}
	var res internalResult
	if jsonErr := json.Unmarshal(data, &res); jsonErr != nil {
		return errors.New("the staged Coop build reported an unreadable result")
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	if res.Image == "" {
		return errors.New("the staged Coop build reported an unreadable result")
	}
	if wantImage != "" && res.Image != wantImage {
		return fmt.Errorf("the staged Coop built %s, but planned %s", res.Image, wantImage)
	}
	return nil
}

// candidateChildError prefers the child's own cause sentence; a signal-killed child writes none,
// in which case the exit error is all the parent has.
func candidateChildError(data []byte, err error) error {
	var res internalResult
	if jsonErr := json.Unmarshal(data, &res); jsonErr == nil && res.Error != "" {
		return errors.New(res.Error)
	}
	return err
}

// cmdInternal serves the hidden `coop internal <subcommand>` protocol a generation update uses to
// make the staged candidate perform its own image build. It is not in help or completions: nothing
// user-facing invokes it, and a Main running it skips generation healing (the candidate record
// belongs to the parent that launched this binary).
func (a *app) cmdInternal(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("coop internal requires a subcommand")
	}
	sub, rest := args[0], args[1:]
	resultPath := ""
	for i := 0; i < len(rest); {
		value, count, match, err := flagValue(rest, i, "--result-file")
		if !match {
			return 2, ui.UnexpectedArgument(rest[i], "coop internal", "coop internal <subcommand> --result-file <path>")
		}
		if err != nil {
			return 2, err
		}
		resultPath = value
		i += count
	}
	if resultPath == "" {
		return 2, errors.New("coop internal " + sub + " requires --result-file")
	}
	switch sub {
	case "box-plan":
		if err := a.internalBoxPlan(resultPath); err != nil {
			return 1, err
		}
	case "box-build":
		if err := a.internalBoxBuild(resultPath); err != nil {
			return 1, err
		}
	default:
		return 2, fmt.Errorf("unknown coop internal subcommand %q", sub)
	}
	return 0, nil
}

// internalFreshPlan resolves exactly the plan the candidate's own definitions produce for a fresh
// update build.
func (a *app) internalFreshPlan() (string, box.BuildPlan, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return "", box.BuildPlan{}, err
	}
	if err := a.ensureRuntime(); err != nil {
		return "", box.BuildPlan{}, err
	}
	plan, err := box.PlanBuild(a.rt, a.cfg, repo, true)
	return repo, plan, err
}

func (a *app) internalBoxPlan(resultPath string) error {
	res := internalResult{Version: normalizeVersion(resolveVersion())}
	_, plan, err := a.internalFreshPlan()
	if err != nil {
		res.Error = sentence(firstLine(err))
		return writeInternalResult(resultPath, res)
	}
	res.BaseImage = a.cfg.BaseImage
	res.Image = plan.Image
	res.Project = plan.Project
	res.Dockerfile = plan.Dockerfile
	qualified := plan.CarriesQualifiedClients()
	res.CarriesQualifiedClients = &qualified
	return writeInternalResult(resultPath, res)
}

func (a *app) internalBoxBuild(resultPath string) error {
	repo, plan, err := a.internalFreshPlan()
	if err != nil {
		return writeInternalResult(resultPath, internalResult{Error: sentence(firstLine(err))})
	}
	// The build's own diagnostics stream to stdio; on failure the cause sentence is the only thing
	// the parent renders into its block, so the child prints nothing of its own.
	if err := box.BuildPlanned(a.rt, a.cfg, repo, plan, true, resolveVersion(), os.Stdin, os.Stdout); err != nil {
		return writeInternalResult(resultPath, internalResult{Image: plan.Image, Error: sentence(firstLine(err))})
	}
	return writeInternalResult(resultPath, internalResult{Image: plan.Image, BaseImage: a.cfg.BaseImage})
}

func writeInternalResult(path string, res internalResult) error {
	data, err := json.Marshal(&res)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// --- pending recycle ------------------------------------------------------------------------

// resumePendingRecycle is the silent, bounded best-effort continuation any runtime-bearing
// startup runs: remove the recorded boxes the now-active generation owes, persisting survivors.
// Failures stay recorded for the next startup or `coop update`; this path never reports.
func (a *app) resumePendingRecycle() {
	st := loadGenerationState(a.cfg)
	if !st.pendingMatchesRunning() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recycleBoxesTimeout)
	defer cancel()
	_, _ = a.recyclePendingIDs(ctx, st)
}

// recyclePendingIDs removes ONLY the recorded container ids, one at a time, under ctx's deadline.
// Removed ids leave the record; the first runtime error or deadline stops the round and keeps the
// survivors (with the cause) for a retry. A box the runtime already reaped reads as gone.
func (a *app) recyclePendingIDs(ctx context.Context, st *generationState) (int, error) {
	p := st.Pending
	removed := 0
	for i, id := range p.IDs {
		if err := ctx.Err(); err != nil {
			p.Attempts++
			p.Error = "the restart deadline passed"
			p.IDs = p.IDs[i:]
			p.UpdatedAt = time.Now().UTC()
			_ = st.save(a.cfg)
			return removed, err
		}
		if err := a.rt.RemoveContainerContext(ctx, id); err != nil {
			p.IDs = append([]string{}, p.IDs[i:]...)
			p.Attempts++
			p.Error = sentence(firstLine(err))
			p.UpdatedAt = time.Now().UTC()
			_ = st.save(a.cfg)
			return removed, err
		}
		removed++
	}
	st.Pending = nil
	_ = st.save(a.cfg)
	return removed, nil
}

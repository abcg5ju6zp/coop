package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
)

// --- state persistence -----------------------------------------------------------------------

func testGenerationCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{BoxHome: t.TempDir(), RepoOverride: t.TempDir(), BaseImage: "coop-box:olddef"}
}

func TestGenerationStateSaveLoadAndClear(t *testing.T) {
	cfg := testGenerationCfg(t)
	path := generationStatePath(cfg)
	if st := loadGenerationState(cfg); st.Candidate != nil || st.Pending != nil {
		t.Fatalf("missing state loaded as %+v", st)
	}
	st := emptyGenerationState()
	st.Candidate = &candidateState{Version: "2.7.3", Staged: "/x/.coop-candidate-2.7.3", Phase: phaseStaged}
	if err := st.save(cfg); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "2.7.3") {
		t.Fatalf("state not persisted: %q err=%v", data, err)
	}
	got := loadGenerationState(cfg)
	if got.Candidate == nil || got.Candidate.Version != "2.7.3" || got.Candidate.Phase != phaseStaged {
		t.Fatalf("loaded candidate = %+v", got.Candidate)
	}
	// Empty state removes the file rather than writing an empty record.
	got.Pending = nil
	got.Candidate = nil
	if err := got.save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty state left a file: %v", err)
	}
}

func TestGenerationStateCorruptIsQuarantined(t *testing.T) {
	cfg := testGenerationCfg(t)
	path := generationStatePath(cfg)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := loadGenerationState(cfg); st.Candidate != nil || st.Pending != nil {
		t.Fatalf("corrupt state loaded as %+v", st)
	}
	matches, _ := filepath.Glob(path + ".bad-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt state was not quarantined: %v", matches)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt state file left in place: %v", err)
	}
}

// --- startup healing --------------------------------------------------------------------------

func writeCandidateState(t *testing.T, cfg *config.Config, c *candidateState, p *pendingRecycle) {
	t.Helper()
	st := emptyGenerationState()
	st.Candidate = c
	st.Pending = p
	if err := st.save(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestHealUpdateGeneration(t *testing.T) {
	t.Run("unswitched candidate is kept for resume", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		cfg := testGenerationCfg(t)
		staged := filepath.Join(t.TempDir(), ".coop-candidate-2.7.3")
		mustWrite(t, staged, "candidate")
		writeCandidateState(t, cfg, &candidateState{Version: "2.7.3", Staged: staged, Phase: phaseStaged}, nil)
		healUpdateGeneration(cfg)
		st := loadGenerationState(cfg)
		if st.Candidate == nil {
			t.Fatal("heal dropped an unswitched, resumable candidate")
		}
		if _, err := os.Stat(staged); err != nil {
			t.Errorf("heal removed an unswitched staged binary: %v", err)
		}
	})

	t.Run("switched candidate is cleared but its pending restart stays", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		cfg := testGenerationCfg(t)
		staged := filepath.Join(t.TempDir(), ".coop-candidate-2.7.3")
		mustWrite(t, staged, "candidate")
		writeCandidateState(t, cfg,
			&candidateState{Version: "2.7.3", Staged: staged, Phase: phaseImageReady},
			&pendingRecycle{Version: "2.7.3", IDs: []string{"a"}})
		healUpdateGeneration(cfg)
		st := loadGenerationState(cfg)
		if st.Candidate != nil {
			t.Fatalf("post-switch candidate not cleared: %+v", st.Candidate)
		}
		if st.Pending == nil || len(st.Pending.IDs) != 1 || st.Pending.IDs[0] != "a" {
			t.Fatalf("pending restart lost: %+v", st.Pending)
		}
		if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("leftover staged path not removed: %v", err)
		}
	})

	t.Run("pending for a downgraded binary is left untouched", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		cfg := testGenerationCfg(t)
		writeCandidateState(t, cfg, nil, &pendingRecycle{Version: "2.7.3", IDs: []string{"a"}})
		healUpdateGeneration(cfg)
		st := loadGenerationState(cfg)
		if st.Pending == nil || len(st.Pending.IDs) != 1 {
			t.Fatalf("downgraded-generation pending was altered: %+v", st.Pending)
		}
	})
}

// --- staging, resume, discard -----------------------------------------------------------------

func releaseFixture(t *testing.T) string {
	t.Helper()
	archive := testReleaseArchive(t, "new-binary", "coop")
	asset := fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sums := releaseChecksum(asset)(archive)
	srv := testReleaseServer(t, archive, sums, nil)
	t.Cleanup(srv.Close)
	t.Cleanup(stub(&releaseFileURLFor, func(tag, name string) string {
		return srv.URL + "/" + tag + "/" + name
	}))
	t.Cleanup(stub(&githubLatestURL, srv.URL))
	return "v2.7.3"
}

func TestStageOrResumeCandidate(t *testing.T) {
	defer stub(&Version, "2.7.2")()
	tag := releaseFixture(t)

	t.Run("stages the verified candidate and records it", func(t *testing.T) {
		cfg := testGenerationCfg(t)
		dir := t.TempDir()
		exe := filepath.Join(dir, "coop")
		mustWrite(t, exe, "old")
		st := loadGenerationState(cfg)
		c, err := st.stageOrResumeCandidate(cfg, exe, tag)
		if err != nil {
			t.Fatal(err)
		}
		if c.Phase != phaseStaged || c.SHA256 == "" {
			t.Fatalf("candidate = %+v", c)
		}
		if got, err := os.ReadFile(exe); err != nil || string(got) != "old" {
			t.Fatalf("staging changed the active executable: %q %v", got, err)
		}
		if got, err := os.ReadFile(c.Staged); err != nil || string(got) != "new-binary" {
			t.Fatalf("staged bytes = %q, err=%v", got, err)
		}
		if info, err := os.Stat(c.Staged); err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("staged mode = %v err=%v, want 755", info.Mode(), err)
		}
		reloaded := loadGenerationState(cfg)
		if reloaded.Candidate == nil || reloaded.Candidate.Version != "2.7.3" {
			t.Fatalf("candidate not recorded: %+v", reloaded.Candidate)
		}
	})

	t.Run("an intact stage is reused without re-downloading", func(t *testing.T) {
		cfg := testGenerationCfg(t)
		dir := t.TempDir()
		exe := filepath.Join(dir, "coop")
		mustWrite(t, exe, "old")
		st := loadGenerationState(cfg)
		first, err := st.stageOrResumeCandidate(cfg, exe, tag)
		if err != nil {
			t.Fatal(err)
		}
		// Corrupt the release endpoint: a resume must not download again.
		t.Cleanup(stub(&releaseFileURLFor, func(string, string) string {
			t.Error("an intact candidate must be reused without downloading")
			return "http://127.0.0.1:1/nope"
		}))
		again := loadGenerationState(cfg)
		second, err := again.stageOrResumeCandidate(cfg, exe, tag)
		if err != nil {
			t.Fatal(err)
		}
		if second.Staged != first.Staged || second.SHA256 != first.SHA256 || second.Phase != phaseStaged {
			t.Fatalf("resume did not reuse the stage: %+v vs %+v", first, second)
		}
	})

	t.Run("a tampered stage is discarded and re-downloaded", func(t *testing.T) {
		cfg := testGenerationCfg(t)
		dir := t.TempDir()
		exe := filepath.Join(dir, "coop")
		mustWrite(t, exe, "old")
		st := loadGenerationState(cfg)
		c, err := st.stageOrResumeCandidate(cfg, exe, tag)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.Staged, []byte("tampered"), 0o755); err != nil {
			t.Fatal(err)
		}
		again := loadGenerationState(cfg)
		restaged, err := again.stageOrResumeCandidate(cfg, exe, tag)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(restaged.Staged); err != nil || string(got) != "new-binary" {
			t.Fatalf("tampered stage not replaced: %q err=%v", got, err)
		}
	})

	t.Run("discard keeps another generation's pending restart", func(t *testing.T) {
		cfg := testGenerationCfg(t)
		staged := filepath.Join(t.TempDir(), ".coop-candidate-2.7.3")
		mustWrite(t, staged, "candidate")
		st := emptyGenerationState()
		st.Candidate = &candidateState{Version: "2.7.3", Staged: staged, Phase: phaseImageReady}
		st.Pending = &pendingRecycle{Version: "2.7.1", IDs: []string{"older"}}
		if err := st.save(cfg); err != nil {
			t.Fatal(err)
		}
		loaded := loadGenerationState(cfg)
		loaded.discardCandidate(cfg)
		after := loadGenerationState(cfg)
		if after.Candidate != nil {
			t.Fatal("candidate still present after discard")
		}
		if after.Pending == nil || len(after.Pending.IDs) != 1 || after.Pending.IDs[0] != "older" {
			t.Fatalf("another generation's pending was dropped: %+v", after.Pending)
		}
		if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("staged binary not removed by discard")
		}
	})
}

// --- candidate wire protocol ------------------------------------------------------------------

func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCandidatePlanBuildParsing(t *testing.T) {
	qualified := true
	plan, err := parseCandidatePlan(jsonBytes(t, internalResult{
		Version: "2.7.3", BaseImage: "coop-box:cand", Image: "coop-box:cand",
		Project: false, CarriesQualifiedClients: &qualified,
	}), nil)
	if err != nil || plan.Image != "coop-box:cand" || plan.Version != "2.7.3" || !plan.CarriesQualifiedClients {
		t.Fatalf("plan = %+v err=%v", plan, err)
	}

	// A failure carried in the result is the cause the parent renders; an exit error with an EMPTY
	// result body falls back to the exit error.
	wantErr := errors.New("the Dockerfile is broken")
	if _, err := parseCandidatePlan(jsonBytes(t, internalResult{Error: wantErr.Error()}), nil); err == nil ||
		err.Error() != wantErr.Error() {
		t.Fatalf("result error = %v, want %q", err, wantErr)
	}
	if _, err := parseCandidatePlan([]byte("{}"), wantErr); err == nil || err != wantErr {
		t.Fatalf("exit error = %v, want the passed exit error unchanged", err)
	}
	if err := runCandidateBuildBytes(t.TempDir(), jsonBytes(t, internalResult{Image: "img"}), nil, "img"); err != nil {
		t.Errorf("matching build result errored: %v", err)
	}
	if err := runCandidateBuildBytes(t.TempDir(), jsonBytes(t, internalResult{Image: "other"}), nil, "img"); err == nil {
		t.Error("an image name differing from the plan must fail")
	}
	if err := runCandidateBuildBytes(t.TempDir(), jsonBytes(t, internalResult{Error: "boom"}), nil, "img"); err == nil {
		t.Error("an error result must fail")
	}
}

// runCandidateBuildBytes exercises the post-child verification without spawning a process.
func runCandidateBuildBytes(boxHome string, data []byte, runErr error, wantImage string) error {
	// Parse through the same helper the child uses by temporarily stubbing the runner.
	restore := stub(&runCandidateInternal, func(_ *config.Config, _, _ string) ([]byte, error) {
		return data, runErr
	})
	defer restore()
	return runCandidateBuild(&config.Config{BoxHome: boxHome}, "/nonexistent/staged", wantImage)
}

// TestRunCandidateInternalProtocol drives the real child runner against a shell-script candidate
// that answers the hidden internal subcommands: argv shape and result-file handoff included.
func TestRunCandidateInternalProtocol(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "coop-candidate")
	trace := filepath.Join(dir, "trace")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + strconvQuote(trace) + `
result=""
for arg in "$@"; do
  case "$prev" in --result-file) result="$arg" ;; esac
  prev="$arg"
done
case "$1 $2" in
  "internal box-plan")
    printf '{"version":"2.7.3","base_image":"coop-box:cand","image":"coop-box:cand","carries_qualified_clients":true}' > "$result"
    ;;
  "internal box-build")
    echo "[candidate build output]"
    printf '{"image":"coop-box:cand"}' > "$result"
    ;;
  *) echo "unknown: $*" >&2; exit 9 ;;
esac
`
	if err := os.WriteFile(candidate, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{BoxHome: dir}

	plan, err := runCandidatePlan(cfg, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Image != "coop-box:cand" || plan.Version != "2.7.3" || !plan.CarriesQualifiedClients {
		t.Fatalf("plan = %+v", plan)
	}
	if err := runCandidateBuild(cfg, candidate, "coop-box:cand"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"internal box-plan --result-file", "internal box-build --result-file"} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("candidate argv missing %q:\n%s", want, calls)
		}
	}
	// No result file is left lying in BoxHome.
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".coop-update-result-*")); len(leftovers) != 0 {
		t.Errorf("result files leaked: %v", leftovers)
	}
}

// --- recorded recycle -------------------------------------------------------------------------

func generationRuntime(t *testing.T, mode string) (containerruntime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	shim := filepath.Join(dir, "docker")
	trace := filepath.Join(dir, "trace")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + strconvQuote(trace) + `
case "$1" in
  ps)
    case "$*" in
      *label=coop.supervised=1*) printf 'sa\nsb\n' ;;
      *label=coop=box*) printf 'sa\nsb\nother\n' ;;
    esac
    # The docker rm-fallback asks "does id=X still exist?" after an rm failure; say so only for the
    # box the test declared still present (matched by the id's seed prefix).
    if [ -n "$COOP_TEST_RM_STILL_THERE" ]; then
      case "$*" in
        *"id=$COOP_TEST_RM_STILL_THERE"*) printf '%s\n' "$COOP_TEST_RM_STILL_THERE" ;;
      esac
    fi
    exit 0
    ;;
  rm)
    id="$3"
    if [ -n "$COOP_TEST_RM_FAIL" ]; then
      case "$id" in
        "$COOP_TEST_RM_FAIL"*)
          echo "remove broke" >&2
          exit 41
          ;;
      esac
    fi
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_RM_FAIL", map[string]string{"partial": "sb", "gone": "09"}[mode])
	if mode == "partial" {
		t.Setenv("COOP_TEST_RM_STILL_THERE", "sb")
	}
	return containerruntime.Runtime{Name: shim}, trace
}

func TestRecyclePendingIDs(t *testing.T) {
	fullID := func(seed string) string { return seed + strings.Repeat("0", 64-len(seed)) }

	t.Run("removes only the recorded ids and clears the record", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		rt, trace := generationRuntime(t, "success")
		cfg := testGenerationCfg(t)
		a := &app{cfg: cfg, rt: rt, rtSet: true}
		st := emptyGenerationState()
		st.Pending = &pendingRecycle{Version: "2.7.3", IDs: []string{fullID("sa"), fullID("sb")}}
		ctx := context.Background()
		removed, err := a.recyclePendingIDs(ctx, st)
		if err != nil || removed != 2 {
			t.Fatalf("recycle = (%d, %v)", removed, err)
		}
		if st.Pending != nil {
			t.Fatalf("record not cleared: %+v", st.Pending)
		}
		if got := loadGenerationState(cfg).Pending; got != nil {
			t.Fatalf("pending persisted after full recycle: %+v", got)
		}
		calls, _ := os.ReadFile(trace)
		if strings.Contains(string(calls), "other") {
			t.Fatalf("an unsupervised box was touched:\n%s", calls)
		}
	})

	t.Run("a surviving id stays recorded with the cause", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		rt, _ := generationRuntime(t, "partial")
		cfg := testGenerationCfg(t)
		a := &app{cfg: cfg, rt: rt, rtSet: true}
		st := emptyGenerationState()
		st.Pending = &pendingRecycle{Version: "2.7.3", IDs: []string{fullID("sa"), fullID("sb")}}
		removed, err := a.recyclePendingIDs(context.Background(), st)
		if err == nil || removed != 1 {
			t.Fatalf("partial recycle = (%d, %v), want 1 removed + error", removed, err)
		}
		st2 := loadGenerationState(cfg)
		if st2.Pending == nil || len(st2.Pending.IDs) != 1 || st2.Pending.IDs[0] != fullID("sb") {
			t.Fatalf("survivors = %+v, want just sb", st2.Pending)
		}
		if st2.Pending.Error == "" || st2.Pending.Attempts != 1 {
			t.Fatalf("cause/attempts not recorded: %+v", st2.Pending)
		}
	})

	t.Run("a box already gone counts as removed", func(t *testing.T) {
		defer stub(&Version, "2.7.3")()
		rt, _ := generationRuntime(t, "gone") // rm fails, but the id query answers empty
		cfg := testGenerationCfg(t)
		a := &app{cfg: cfg, rt: rt, rtSet: true}
		st := emptyGenerationState()
		st.Pending = &pendingRecycle{Version: "2.7.3", IDs: []string{fullID("09")}}
		removed, err := a.recyclePendingIDs(context.Background(), st)
		if err != nil || removed != 1 {
			t.Fatalf("gone-id recycle = (%d, %v)", removed, err)
		}
	})

	t.Run("a running older binary never reaps a newer generation's record", func(t *testing.T) {
		defer stub(&Version, "2.7.2")()
		rt, trace := generationRuntime(t, "success")
		cfg := testGenerationCfg(t)
		a := &app{cfg: cfg, rt: rt, rtSet: true}
		st := emptyGenerationState()
		st.Pending = &pendingRecycle{Version: "2.7.3", IDs: []string{fullID("sa")}}
		if err := st.save(cfg); err != nil {
			t.Fatal(err)
		}
		a.resumePendingRecycle()
		if got := loadGenerationState(cfg).Pending; got == nil || len(got.IDs) != 1 {
			t.Fatalf("downgrade guard altered pending: %+v", got)
		}
		calls, _ := os.ReadFile(trace)
		if strings.Contains(string(calls), "rm") {
			t.Fatalf("the older binary removed a newer generation's box:\n%s", calls)
		}
	})
}

// strconvQuote produces a safely double-quoted string for embedding in a /bin/sh script.
func strconvQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// --- end-to-end generation update -------------------------------------------------------------

// updateShim is a docker-like runtime that lists supervised boxes and removes them by id.
func updateShim(t *testing.T, rmFail string, trace string) containerruntime.Runtime {
	t.Helper()
	dir := filepath.Dir(trace)
	shim := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + strconvQuote(trace) + `
case "$1" in
  info) [ "$2" = --format ] && echo linux/aarch64; exit 0 ;;
  image) exit 0 ;; # every image the candidate planned is present
  ps)
    case "$*" in
      *label=coop.supervised=1*) printf '%s\n' sa sb ;;
      *label=coop=box*) printf '%s\n' sa sb other ;;
    esac
    exit 0 ;;
  rm)
    [ "$3" = "` + rmFail + `" ] && { echo "remove broke" >&2; exit 43; }
    exit 0 ;;
  build) echo "[build output]"; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return containerruntime.Runtime{Name: shim}
}

func stubCandidateInternal(t *testing.T, failBuild bool, calls *[]string) {
	t.Helper()
	qualified := true
	t.Cleanup(stub(&runCandidateInternal, func(_ *config.Config, _, subcommand string) ([]byte, error) {
		*calls = append(*calls, subcommand)
		switch subcommand {
		case "box-plan":
			return jsonBytes(t, internalResult{
				Version: "2.7.3", BaseImage: "coop-box:cand", Image: "coop-box:cand",
				CarriesQualifiedClients: &qualified,
			}), nil
		case "box-build":
			if failBuild {
				return jsonBytes(t, internalResult{Error: "the build failed"}), nil
			}
			return jsonBytes(t, internalResult{Image: "coop-box:cand"}), nil
		}
		return nil, errors.New("unknown subcommand")
	}))
}

func setupGenerationUpdate(t *testing.T) (cfg *config.Config, exe string, api *httptest.Server) {
	t.Helper()
	t.Cleanup(stub(&Version, "2.7.2"))
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v2.7.3"}`)
	}))
	t.Cleanup(api.Close)
	t.Cleanup(stub(&githubLatestURL, api.URL))

	archive := testReleaseArchive(t, "new-binary", "coop")
	asset := fmt.Sprintf("coop_2.7.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	release := testReleaseServer(t, archive, releaseChecksum(asset)(archive), nil)
	t.Cleanup(release.Close)
	t.Cleanup(stub(&releaseFileURLFor, func(tag, name string) string {
		return release.URL + "/" + tag + "/" + name
	}))

	dir := t.TempDir()
	exe = filepath.Join(dir, "coop")
	mustWrite(t, exe, "old-binary")
	t.Cleanup(stub(&executablePath, func() (string, error) { return exe, nil }))

	cfg = &config.Config{BoxHome: t.TempDir(), RepoOverride: t.TempDir(), BaseImage: "coop-box:olddef"}
	return cfg, exe, api
}

func TestCmdUpdateGenerationSuccess(t *testing.T) {
	cfg, exe, _ := setupGenerationUpdate(t)
	trace := filepath.Join(t.TempDir(), "runtime-trace")
	rt := updateShim(t, "", trace)
	var calls []string
	stubCandidateInternal(t, false, &calls)
	a := &app{cfg: cfg, rt: rt, rtSet: true}

	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUpdate(nil) })
	if code != 0 {
		t.Fatalf("cmdUpdate = %d:\n%s", code, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new-binary" {
		t.Fatalf("active executable = %q, want the candidate", got)
	}
	if got := loadGenerationState(cfg); got.Candidate != nil || got.Pending != nil {
		t.Fatalf("state not finalized: %+v", got)
	}
	if fmt.Sprint(calls) != "[box-plan box-build]" {
		t.Fatalf("candidate subcommands = %v, want plan then build", calls)
	}
	runtimeCalls, _ := os.ReadFile(trace)
	for _, want := range []string{"rm -f sa", "rm -f sb"} {
		if !strings.Contains(string(runtimeCalls), want) {
			t.Errorf("runtime calls missing %q:\n%s", want, runtimeCalls)
		}
	}
	if strings.Contains(string(runtimeCalls), "rm -f other") {
		t.Error("an unsupervised box was force-removed")
	}
	for _, want := range []string{"Coop updated to v2.7.3", "Box image updated", "will reconnect with the new image"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCmdUpdateGenerationBuildFailureKeepsOldCombination(t *testing.T) {
	cfg, exe, _ := setupGenerationUpdate(t)
	trace := filepath.Join(t.TempDir(), "runtime-trace")
	rt := updateShim(t, "", trace)
	var calls []string
	stubCandidateInternal(t, true, &calls)
	a := &app{cfg: cfg, rt: rt, rtSet: true}

	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUpdate(nil) })
	if code != 1 {
		t.Fatalf("a failed candidate build exited %d, want 1:\n%s", code, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old-binary" {
		t.Fatalf("the active executable changed after a build failure: %q", got)
	}
	if got := loadGenerationState(cfg); got.Candidate != nil || got.Pending != nil {
		t.Fatalf("candidate not discarded: %+v", got)
	}
	runtimeCalls, _ := os.ReadFile(trace)
	if strings.Contains(string(runtimeCalls), "rm -f") {
		t.Fatalf("sessions were restarted despite the failed build:\n%s", runtimeCalls)
	}
	if !strings.Contains(out, "run coop update again") || !strings.Contains(out, "were not changed") {
		t.Fatalf("failure output missing the retry/unchanged guidance:\n%s", out)
	}
}

func TestCmdUpdateGenerationPartialRecycleIsRecordedAndResumed(t *testing.T) {
	cfg, exe, _ := setupGenerationUpdate(t)
	trace := filepath.Join(t.TempDir(), "runtime-trace")
	rt := updateShim(t, "sb", trace)
	var calls []string
	stubCandidateInternal(t, false, &calls)
	a := &app{cfg: cfg, rt: rt, rtSet: true}

	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUpdate(nil) })
	if code != 1 {
		t.Fatalf("partial recycle exited %d, want 1:\n%s", code, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new-binary" {
		t.Fatalf("the new binary must stay active: %q", got)
	}
	st := loadGenerationState(cfg)
	if st.Pending == nil || len(st.Pending.IDs) != 1 || st.Pending.IDs[0] != "sb" || st.Candidate != nil {
		t.Fatalf("surviving box not recorded: %+v", st.Pending)
	}
	if !strings.Contains(out, "Could not restart all supervised editor sessions") {
		t.Fatalf("recycle failure was not reported:\n%s", out)
	}

	// The new binary runs on the next startup and finishes the recorded restart, silently.
	defer stub(&Version, "2.7.3")()
	resume := updateShim(t, "", filepath.Join(t.TempDir(), "resume-trace"))
	b := &app{cfg: cfg, rt: resume, rtSet: true}
	b.resumePendingRecycle()
	if got := loadGenerationState(cfg).Pending; got != nil {
		t.Fatalf("pending restart not finished on resume: %+v", got)
	}
}

func TestCmdUpdateSelfOnlyBoundary(t *testing.T) {
	cfg, exe, _ := setupGenerationUpdate(t)
	var calls []string
	stubCandidateInternal(t, false, &calls)
	a := &app{cfg: cfg, rtSet: false}

	// A pending restart recorded by an older generation must survive a binary-only update.
	st := emptyGenerationState()
	st.Pending = &pendingRecycle{Version: "2.7.1", IDs: []string{"older-box"}}
	if err := st.save(cfg); err != nil {
		t.Fatal(err)
	}

	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUpdate([]string{"--self-only"}) })
	if code != 0 {
		t.Fatalf("--self-only = %d:\n%s", code, out)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new-binary" {
		t.Fatalf("exe = %q, want candidate", got)
	}
	if len(calls) != 0 {
		t.Fatalf("--self-only built an image: %v", calls)
	}
	got := loadGenerationState(cfg)
	if got.Candidate != nil {
		t.Fatalf("--self-only left a candidate record: %+v", got.Candidate)
	}
	if got.Pending == nil || len(got.Pending.IDs) != 1 || got.Pending.IDs[0] != "older-box" {
		t.Fatalf("--self-only broke the pending restart record: %+v", got.Pending)
	}
	if !strings.Contains(out, "Coop updated to v2.7.3") {
		t.Fatalf("--self-only output:\n%s", out)
	}
}

func TestCmdUpdateBoxOnlyDoesNotTouchCandidate(t *testing.T) {
	// A staged generation from a crashed full update survives --box-only: it builds the ACTIVE
	// binary's image but must not rename, delete, or advance the candidate.
	cfg := &config.Config{RepoOverride: t.TempDir(), BoxHome: t.TempDir(), BaseImage: "coop-box:5db4c3f8c69555471772326ae9c3e960"}
	staged := filepath.Join(t.TempDir(), ".coop-candidate-2.7.3")
	mustWrite(t, staged, "candidate-bytes")
	st := emptyGenerationState()
	st.Candidate = &candidateState{Version: "2.7.3", Tag: "v2.7.3", Exe: "/nonexistent/coop", Staged: staged,
		SHA256: "unused", Phase: phaseImageReady, Image: "coop-box:cand"}
	if err := st.save(cfg); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg, rt: buildShim{daemonUp: true}.build(t), rtSet: true}

	var code int
	out := captureTerminal(t, func() { code, _ = a.cmdUpdate([]string{"--box-only"}) })
	if code != 0 {
		t.Fatalf("--box-only = %d:\n%s", code, out)
	}
	got := loadGenerationState(cfg)
	if got.Candidate == nil || got.Candidate.Version != "2.7.3" || got.Candidate.Phase != phaseImageReady {
		t.Fatalf("--box-only altered the candidate record: %+v", got.Candidate)
	}
	if data, err := os.ReadFile(staged); err != nil || string(data) != "candidate-bytes" {
		t.Fatalf("--box-only touched the staged binary: %q err=%v", data, err)
	}
}

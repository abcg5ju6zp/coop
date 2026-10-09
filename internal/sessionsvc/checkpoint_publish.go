package sessionsvc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The post-verification publish of a restored checkpoint rewrites the live workspace
// before the session row and operation receipt commit. This file makes that publish one
// undoable, crash-decidable transaction:
//
//	workspace-checkpoints/<op>.publish/
//	  publish.json      durable phase journal
//	  result.json       marshaled receipt result (before phase=committed)
//	  original/files/  bytes of every original untracked and task-projection file
//	  candidate/       the verified stage moved here whole (repository/, members/)
//
// The tracked half of the original workspace needs no copies: its HEAD and tracked
// tree are pinned into the workspace object store at capture, materialization only adds
// objects, and submodule gitlinks are provably unchanged by the earlier verification.
const (
	checkpointPublishJournalVersion = 1
	checkpointPublishJournalMax     = 4 << 20
	checkpointPublishJournalName    = "publish.json"
	checkpointPublishResultName     = "result.json"
	checkpointPublishOriginalDir    = "original"
	checkpointPublishCandidateDir   = "candidate"
	checkpointPublishManifestEntry  = "manifest.json"
)

type checkpointPublishPhase string

const (
	// phasePrepared: original content is pinned and the switch intent is durable; the
	// live tree may be unwritten or partially rewritten (a crash mid-materialization lands here).
	checkpointPublishPrepared checkpointPublishPhase = "prepared"
	// phasePublished: the live tree holds the verified candidate; the session row has
	// not necessarily crossed its compare-and-swap yet.
	checkpointPublishPublished checkpointPublishPhase = "published"
	// phaseCommitted: the confirmation point — the session row owns the binding/base.
	// Only the operation receipt remains, and it must be backfilled, never re-published.
	checkpointPublishCommitted checkpointPublishPhase = "committed"
	// phaseRolledBack: the live tree is back at the pinned original; the candidate is
	// retained, so the same operation can publish again without re-streaming the body.
	checkpointPublishRolledBack checkpointPublishPhase = "rolled_back"
)

// errCheckpointCandidateMissing means the transaction's durable journal exists but its
// retained candidate does not; the caller falls back to the retained-body replay.
var errCheckpointCandidateMissing = errors.New("retained checkpoint candidate is missing")

type checkpointPublishJournal struct {
	Version   int    `json:"version"`
	Phase     string `json:"phase"`
	OpID      string `json:"operation_id"`
	SessionID string `json:"session_id"`

	Repository       string `json:"repository"`
	Workspace        string `json:"workspace"`
	ForkName         string `json:"fork_name"`
	ForkGeneration   string `json:"fork_generation"`
	Branch           string `json:"branch"`
	ExpectedRevision int64  `json:"expected_revision"`

	// Checkpoint is the canonical descriptor bytes, so a retry has to name the
	// exact checkpoint the transaction was opened for.
	Checkpoint        json.RawMessage `json:"checkpoint"`
	BaseRevision      string          `json:"base_revision"`
	CommittedRevision string          `json:"committed_revision"`

	Original checkpointPublishOriginal `json:"original"`
	// Binding and PublishedDigest are recorded with phase=published.
	Binding         *session.WorkspaceTaskBinding `json:"binding,omitempty"`
	PublishedDigest string                        `json:"published_digest,omitempty"`
}

type checkpointPublishOriginal struct {
	Head        string                                     `json:"head"`
	TrackedTree string                                     `json:"tracked_tree"`
	Untracked   []workerproto.WorkspaceCheckpointFileEntry `json:"untracked"`
	TaskFiles   []workerproto.WorkspaceCheckpointFileEntry `json:"task_files"`
}

type checkpointPublish struct {
	s   *Service
	op  session.Operation
	dir string
	j   checkpointPublishJournal
}

func (s *Service) checkpointPublishDir(operationID string) (string, error) {
	if !validSessionPathComponent(operationID) {
		return "", errors.New("checkpoint operation identity is invalid")
	}
	return filepath.Join(s.stateRoot, "workspace-checkpoints", operationID+".publish"), nil
}

// openCheckpointPublish returns the durable transaction for an operation, or nil when
// no journal was ever committed. A malformed journal is an error: it names a
// workspace whose rollback/forward direction nobody is allowed to guess.
func (s *Service) openCheckpointPublish(operationID string) (*checkpointPublish, error) {
	dir, err := s.checkpointPublishDir(operationID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, checkpointPublishJournalName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint publish journal: %w", err)
	}
	var journal checkpointPublishJournal
	if err := json.Unmarshal(data, &journal); err != nil || journal.Version != checkpointPublishJournalVersion ||
		len(data) > checkpointPublishJournalMax || journal.OpID != operationID ||
		!validSessionPathComponent(journal.SessionID) {
		return nil, errors.New("checkpoint publish journal is invalid")
	}
	switch checkpointPublishPhase(journal.Phase) {
	case checkpointPublishPrepared, checkpointPublishPublished, checkpointPublishCommitted, checkpointPublishRolledBack:
	default:
		return nil, errors.New("checkpoint publish journal phase is invalid")
	}
	return &checkpointPublish{s: s, op: session.Operation{ID: operationID}, dir: dir, j: journal}, nil
}

func writeCheckpointPublishJournal(dir string, journal *checkpointPublishJournal) error {
	journal.Version = checkpointPublishJournalVersion
	data, err := json.Marshal(journal)
	if err != nil || len(data) > checkpointPublishJournalMax {
		return errors.New("checkpoint publish journal exceeds its bound")
	}
	tmp, err := os.CreateTemp(dir, ".journal-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := errors.Join(tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, checkpointPublishJournalName)); err != nil {
		return err
	}
	return syncReviewPath(dir)
}

// prepareCheckpointPublish pins the original workspace state and durably records the
// switch intent. The verified stage is moved later (run), so a crash before this
// function returns leaves only the legacy retained-body replay path.
func (s *Service) prepareCheckpointPublish(
	ctx context.Context,
	op session.Operation,
	sess session.Session,
	checkpoint workerproto.WorkspaceCheckpoint,
	manifest workerproto.WorkspaceCheckpointBundleManifest,
) (*checkpointPublish, bool, error) {
	if txn, err := s.openCheckpointPublish(op.ID); txn != nil || err != nil {
		return txn, true, err
	}
	dir, err := s.checkpointPublishDir(op.ID)
	if err != nil {
		return nil, false, err
	}
	// A crash between the first snapshot byte and the journal write leaves a
	// journal-less directory: its O_EXCL copies could never be retaken. Start over.
	if _, err := os.Stat(filepath.Join(dir, checkpointPublishOriginalDir)); err == nil {
		if err := os.RemoveAll(dir); err != nil {
			return nil, false, err
		}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil || len(manifestBytes) > workerproto.MaxWorkspaceCheckpointManifest {
		return nil, false, errors.New("checkpoint manifest exceeds its bound")
	}
	original, err := captureCheckpointOriginal(ctx, sess.Workspace)
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Join(dir, checkpointPublishOriginalDir, "files"), 0o700); err != nil {
		return nil, false, err
	}
	for _, group := range [][]workerproto.WorkspaceCheckpointFileEntry{original.Untracked, original.TaskFiles} {
		for _, entry := range group {
			if err := copyOriginalFile(sess.Workspace, dir, entry); err != nil {
				return nil, false, err
			}
		}
	}
	// The bundle reader consumes manifest.json itself, so the extracted stage
	// never carries it; retain it with the journal for candidate resume.
	if err := os.WriteFile(filepath.Join(dir, checkpointPublishManifestEntry), manifestBytes, 0o600); err != nil {
		return nil, false, err
	}
	descriptor, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, false, err
	}

	journal := checkpointPublishJournal{
		Phase: string(checkpointPublishPrepared), OpID: op.ID, SessionID: sess.ID,
		Repository: sess.Repository, Workspace: sess.Workspace,
		ForkName: sess.ForkName, ForkGeneration: sess.ForkGeneration, Branch: sess.ForkName,
		ExpectedRevision: sess.Revision,
		Checkpoint:       descriptor,
		BaseRevision:     checkpoint.BaseRevision, CommittedRevision: checkpoint.CommittedRevision,
		Original: original,
	}
	// The journal must be durable before run() is allowed to move the candidate or
	// touch the live tree.
	if err := writeCheckpointPublishJournal(dir, &journal); err != nil {
		return nil, false, err
	}
	if err := syncReviewPath(filepath.Dir(dir)); err != nil {
		return nil, false, err
	}
	txn, err := s.openCheckpointPublish(op.ID)
	return txn, false, err
}

func copyOriginalFile(workspace, dir string, entry workerproto.WorkspaceCheckpointFileEntry) error {
	prefix := ""
	switch {
	case strings.HasPrefix(entry.Entry, "untracked/"):
		prefix = "untracked/"
	case strings.HasPrefix(entry.Entry, "task/"):
		prefix = "task/"
	default:
		return errors.New("invalid original file entry")
	}
	name := entry.PathBytes
	if name == nil {
		decoded, err := base64.StdEncoding.DecodeString(entry.PathB64)
		if err != nil {
			return errors.New("invalid original file path")
		}
		name = decoded
	}
	input, err := os.Open(filepath.Join(workspace, filepath.FromSlash(string(name))))
	if err != nil {
		return err
	}
	defer input.Close()
	destinationDir := filepath.Join(dir, checkpointPublishOriginalDir, "files", filepath.Clean(prefix))
	leaf := filepath.Base(entry.Entry)
	if leaf == "." || leaf == ".." || leaf == "" {
		return errors.New("invalid original file entry")
	}
	destination := filepath.Join(destinationDir, leaf)
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, reviewScanReader{context.Background(), input})
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil || syncErr != nil || written != entry.ByteSize {
		return errors.Join(copyErr, syncErr, closeErr, errors.New("original workspace file changed during capture"))
	}
	return nil
}

// captureCheckpointOriginal pins the exact restorable identity of the workspace before
// anything is rewritten: HEAD, the tracked tree (committed+staged+unstaged, exactly the
// checkpoint fidelity model), every portable untracked file, and the task projection.
func captureCheckpointOriginal(ctx context.Context, workspace string) (checkpointPublishOriginal, error) {
	var original checkpointPublishOriginal
	head, err := sessionWorkspaceCommitContext(ctx, workspace, "HEAD")
	if err != nil {
		return original, err
	}
	err = withSessionTrackedTreeGitDir(ctx, workspace, func(tree, gitDir string, _ []string) error {
		// write-tree stores into the private index repository, which is removed when the
		// callback returns. Pin the tree and its reachable blobs into the real object
		// store now, or rollback could not resolve it.
		if err := forkspace.GitFetchPinnedContext(ctx, gitDir, workspace, tree); err != nil {
			return err
		}
		original.TrackedTree = tree
		return nil
	})
	if err != nil {
		return original, fmt.Errorf("pin original tracked tree: %w", err)
	}
	untracked, err := checkpointUntrackedFiles(ctx, workspace)
	if err != nil {
		return original, err
	}
	taskFiles, err := checkpointWorkspaceTaskFiles(ctx, workspace)
	if err != nil {
		return original, err
	}
	original.Head = head
	original.Untracked = checkpointEntries(untracked)
	original.TaskFiles = checkpointEntries(taskFiles)
	return original, nil
}

// checkpointWorkspaceTaskFiles snapshots the regular files under .agent/tasks the same
// way capture does, excluding the disposable tmp/ scratch directory. The restore wipes
// this git-ignored tree with `git clean -x`, so rollback needs its bytes too.
func checkpointWorkspaceTaskFiles(ctx context.Context, workspace string) ([]checkpointBlob, error) {
	queue := filepath.Join(workspace, tasks.TasksRoot)
	info, err := os.Lstat(queue)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace task projection is not a directory")
	}
	var paths [][]byte
	if _, err := os.Lstat(filepath.Join(queue, tasks.QueueIdentityFile)); err == nil {
		paths = append(paths, []byte(tasks.QueueIdentityFile))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	err = filepath.WalkDir(queue, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == queue {
			return nil
		}
		rel, err := filepath.Rel(queue, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("workspace task projection contains a non-regular file")
		}
		paths = append(paths, []byte(filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(paths, func(i, j int) bool { return bytes.Compare(paths[i], paths[j]) < 0 })
	if len(paths) > workerproto.MaxWorkspaceCheckpointTaskFiles {
		return nil, errors.New("workspace task file count exceeds its bound")
	}
	if len(paths) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(queue)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return checkpointFilesFromPaths(ctx, root, paths, "task", workerproto.MaxWorkspaceCheckpointStreamBytes)
}

func checkpointStateDigest(head, tree string, untracked, taskFiles []workerproto.WorkspaceCheckpointFileEntry) string {
	return checkpointSHA256(mustCheckpointJSON(struct {
		Head      string                                     `json:"head"`
		Tree      string                                     `json:"tree"`
		Untracked []workerproto.WorkspaceCheckpointFileEntry `json:"untracked"`
		TaskFiles []workerproto.WorkspaceCheckpointFileEntry `json:"task_files"`
	}{Head: head, Tree: tree, Untracked: untracked, TaskFiles: taskFiles}))
}

// checkpointWorkspaceStateDigest hashes everything the publish rewrites: HEAD, the
// tracked tree, untracked content and the task projection. A user write between
// publication and the final switch changes it and blocks the switch.
func checkpointWorkspaceStateDigest(ctx context.Context, workspace string) (string, error) {
	head, err := sessionWorkspaceCommitContext(ctx, workspace, "HEAD")
	if err != nil {
		return "", err
	}
	var tree string
	if err := withSessionTrackedTree(ctx, workspace, func(captured string, _ []string) error {
		tree = captured
		return nil
	}); err != nil {
		return "", err
	}
	untracked, err := checkpointUntrackedFiles(ctx, workspace)
	if err != nil {
		return "", err
	}
	taskFiles, err := checkpointWorkspaceTaskFiles(ctx, workspace)
	if err != nil {
		return "", err
	}
	return checkpointStateDigest(head, tree, checkpointEntries(untracked), checkpointEntries(taskFiles)), nil
}

func (j *checkpointPublishJournal) originalDigest() string {
	return checkpointStateDigest(j.Original.Head, j.Original.TrackedTree, j.Original.Untracked, j.Original.TaskFiles)
}

func (p *checkpointPublish) checkpointDescriptor() (workerproto.WorkspaceCheckpoint, error) {
	var checkpoint workerproto.WorkspaceCheckpoint
	if err := json.Unmarshal(p.j.Checkpoint, &checkpoint); err != nil {
		return checkpoint, errors.New("retained checkpoint descriptor is invalid")
	}
	return checkpoint, nil
}

// ensureCandidate retains the verified stage under the transaction directory. The
// stage is the one this operation created in .staging; anything else is refused.
func (p *checkpointPublish) ensureCandidate(stage string) error {
	candidate := filepath.Join(p.dir, checkpointPublishCandidateDir)
	if info, err := os.Lstat(candidate); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("retained checkpoint candidate is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if stage == "" {
		stage = p.findStage()
	}
	if stage == "" {
		return errCheckpointCandidateMissing
	}
	if filepath.Dir(stage) != filepath.Join(filepath.Dir(p.dir), ".staging") ||
		!strings.HasPrefix(filepath.Base(stage), p.op.ID+"-") {
		return errors.New("checkpoint stage identity is invalid")
	}
	if err := os.Rename(stage, candidate); err != nil {
		return err
	}
	return syncReviewPath(filepath.Dir(p.dir))
}

func (p *checkpointPublish) findStage() string {
	staging := filepath.Join(filepath.Dir(p.dir), ".staging")
	entries, err := os.ReadDir(staging)
	if err != nil {
		return ""
	}
	var match string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), p.op.ID+"-") {
			if match != "" {
				return ""
			}
			match = filepath.Join(staging, entry.Name())
		}
	}
	return match
}

func (p *checkpointPublish) loadCandidate() (workerproto.WorkspaceCheckpointBundleManifest, map[string]string, error) {
	candidate := filepath.Join(p.dir, checkpointPublishCandidateDir)
	manifestBytes, err := os.ReadFile(filepath.Join(p.dir, checkpointPublishManifestEntry))
	if err != nil {
		return workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	manifest, err := workerproto.DecodeWorkspaceCheckpointBundleManifest(manifestBytes)
	if err != nil {
		return workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	members := make(map[string]string)
	add := func(entry string) { members[entry] = filepath.Join(candidate, "members", filepath.FromSlash(entry)) }
	add(manifest.Repository.Entry)
	for _, entry := range manifest.UntrackedFiles {
		add(entry.Entry)
	}
	for _, entry := range manifest.TaskProjection.Files {
		add(entry.Entry)
	}
	return manifest, members, nil
}

// run drives one publish transaction to a terminal receipt from whatever phase the
// journal records. stage names the verified stage when the live call just built it;
// recovery and retries pass "". freshStart means this call just journaled
// phase=prepared against an untouched tree, so the idempotent pre-publish rollback
// is skipped (materialization resets the tree anyway).
func (p *checkpointPublish) run(ctx context.Context, stage string, freshStart bool) (session.Session, error) {
	if err := p.ensureCandidate(stage); err != nil {
		return session.Session{}, err
	}
	checkpoint, err := p.checkpointDescriptor()
	if err != nil {
		return session.Session{}, err
	}
	// Confirmation point already crossed: the only remaining work is backfilling
	// the receipt from the committed session row — the tree must not be touched again.
	if p.j.Phase == string(checkpointPublishCommitted) {
		return p.completeReceipt(ctx)
	}
	fresh, err := p.s.store.GetSession(ctx, p.j.SessionID)
	if err != nil {
		return session.Session{}, err
	}
	if p.sessionOwnsCandidate(fresh) {
		return p.completeReceipt(ctx)
	}
	if p.j.Phase == string(checkpointPublishPublished) {
		// The switch was published to the tree but not committed. A concurrent
		// turn, another restore, a user write or an authority change BLOCK it:
		// roll back to the original and leave the operation retryable. The
		// caller's next attempt re-publishes the retained candidate.
		if err := p.fence(ctx, fresh); err != nil {
			if rollbackErr := p.rollback(ctx, fresh); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
			return session.Session{}, err
		}
	} else {
		// prepared (possibly a partial materialization) or already rolled back.
		// A fresh in-process start has an untouched tree; materialization
		// rewrites it wholesale. Anything resumed after a crash may be half-new.
		if !freshStart {
			if err := p.rollback(ctx, fresh); err != nil {
				return session.Session{}, fmt.Errorf("restore checkpoint could not restore the workspace: %w", err)
			}
		}
		if err := p.publishTree(ctx, fresh, checkpoint); err != nil {
			return session.Session{}, err
		}
	}
	return p.commitSwitch(ctx)
}

func (p *checkpointPublish) sessionOwnsCandidate(sess session.Session) bool {
	return sess.WorkspaceTask != nil && p.j.Binding != nil &&
		*sess.WorkspaceTask == *p.j.Binding && sess.BaseCommit == p.j.BaseRevision
}

// fence is the last check before the session row switches. The runtime lock already
// excludes turns and checkpoints, but the durable facts are re-read here: the control
// plane must not have moved, no other restore may be pending, the fork authority is
// still this session's generation, and the tree is exactly the published one —
// a user write in the publish window changes the digest and blocks the switch.
func (p *checkpointPublish) fence(ctx context.Context, sess session.Session) error {
	if sess.WorkspaceTask != nil {
		return errors.New("session became bound before the checkpoint switch")
	}
	if sess.Revision != p.j.ExpectedRevision {
		return &session.Error{Code: session.CodeRevisionConflict, Detail: "session revision changed before the checkpoint switch"}
	}
	if sess.State != session.SessionOpen || sess.RepositoryReadOnly || sess.Activity != session.ActivityParked ||
		sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 || sess.TurnsUsed != 0 {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: "session changed before the checkpoint switch"}
	}
	if pending, err := p.s.store.HasPendingWorkspaceRestore(ctx, sess.ID, p.op.ID); err != nil {
		return err
	} else if pending {
		return errors.New("another checkpoint restore is pending for this session")
	}
	branch, err := sessionWorkspaceGitTextContext(ctx, sess.Workspace, 257, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != p.j.ForkName {
		return errors.New("replacement workspace branch authority changed")
	}
	if err := p.s.validateSessionForkAuthority(ctx, sess); err != nil {
		return err
	}
	if p.j.Phase == string(checkpointPublishPublished) {
		digest, err := checkpointWorkspaceStateDigest(ctx, sess.Workspace)
		if err != nil || digest != p.j.PublishedDigest {
			return errors.New("workspace changed before the checkpoint switch")
		}
	}
	return nil
}

// publishTree materializes the verified candidate into the live workspace, verifies it
// there, and durably publishes the digest. Any failure restores the original first:
// a failed publish leaves the workspace exactly as it was, candidate retained.
func (p *checkpointPublish) publishTree(
	ctx context.Context,
	sess session.Session,
	checkpoint workerproto.WorkspaceCheckpoint,
) (returnErr error) {
	manifest, members, err := p.loadCandidate()
	if err != nil {
		return err
	}
	candidate := filepath.Join(p.dir, checkpointPublishCandidateDir)
	rolledBack := false
	defer func() {
		if returnErr != nil && !rolledBack {
			returnErr = errors.Join(returnErr, p.rollback(context.Background(), sess))
		}
	}()
	if err := materializeCheckpoint(ctx, sess.Repository, filepath.Join(candidate, "repository"), sess.Workspace,
		checkpoint, manifest, members); err != nil {
		return err
	}
	if err := p.s.validateSessionForkAuthority(ctx, sess); err != nil {
		return err
	}
	binding, err := verifyStreamedCheckpoint(ctx, sess, checkpoint, manifest)
	if err != nil {
		return err
	}
	digest, err := checkpointWorkspaceStateDigest(ctx, sess.Workspace)
	if err != nil {
		return err
	}
	p.j.Binding = &binding
	p.j.PublishedDigest = digest
	p.j.Phase = string(checkpointPublishPublished)
	if err := writeCheckpointPublishJournal(p.dir, &p.j); err != nil {
		return err
	}
	rolledBack = true // publication reached its durable phase; nothing to roll back from here
	return nil
}

// commitSwitch is the confirmation point. Its compare-and-swap is the one moment the
// control plane adopts the new workspace; a failure rolls the files back and keeps
// the verified candidate for retry.
func (p *checkpointPublish) commitSwitch(ctx context.Context) (session.Session, error) {
	fresh, err := p.s.store.GetSession(ctx, p.j.SessionID)
	if err != nil {
		return session.Session{}, err
	}
	if !p.sessionOwnsCandidate(fresh) {
		// Test seam: simulate the process dying right after the publication
		// was journaled — no rollback runs, exactly like a kill -9.
		if p.s.testCrashAfterRestorePublished != nil && p.s.testCrashAfterRestorePublished() {
			return session.Session{}, errors.New("simulated process exit after checkpoint publication")
		}
		if p.s.testAfterRestorePublished != nil {
			p.s.testAfterRestorePublished()
		}
		if err := p.fence(ctx, fresh); err != nil {
			rollbackErr := p.rollback(ctx, fresh)
			return session.Session{}, errors.Join(err, rollbackErr)
		}
		bound, err := p.s.store.RestoreWorkspaceTask(ctx, fresh.ID, p.j.ExpectedRevision, p.j.BaseRevision, *p.j.Binding)
		if err != nil {
			rollbackErr := p.rollback(ctx, fresh)
			return session.Session{}, errors.Join(err, rollbackErr)
		}
		fresh = bound
	}
	result, err := json.Marshal(fresh)
	if err != nil {
		return session.Session{}, err
	}
	if err := writeCheckpointPublishFile(p.dir, checkpointPublishResultName, result); err != nil {
		return session.Session{}, err
	}
	p.j.Phase = string(checkpointPublishCommitted)
	if err := writeCheckpointPublishJournal(p.dir, &p.j); err != nil {
		return session.Session{}, err
	}
	return p.completeReceipt(ctx)
}

func writeCheckpointPublishFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".result-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := errors.Join(tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncReviewPath(dir)
}

// completeReceipt backfills the operation receipt from the already-committed
// session row. Session revision, worktree HEAD and the same operation prove which result
// is existing; the candidate tree is never materialized again.
func (p *checkpointPublish) completeReceipt(ctx context.Context) (session.Session, error) {
	op, err := p.s.store.GetOperationByID(ctx, p.op.ID)
	if err != nil {
		return session.Session{}, err
	}
	if op.State == session.OperationSucceeded {
		var bound session.Session
		if err := json.Unmarshal(op.Result, &bound); err != nil || bound.ID != p.j.SessionID {
			return session.Session{}, errors.New("stored restore result is invalid")
		}
		return bound, nil
	}
	if op.State != session.OperationRunning && op.State != session.OperationUncertain {
		return session.Session{}, errors.New("checkpoint publish operation is neither running nor complete")
	}
	result, err := os.ReadFile(filepath.Join(p.dir, checkpointPublishResultName))
	if err != nil {
		return session.Session{}, err
	}
	var bound session.Session
	if err := json.Unmarshal(result, &bound); err != nil || bound.ID != p.j.SessionID {
		return session.Session{}, errors.New("checkpoint publish result is invalid")
	}
	fresh, err := p.s.store.GetSession(ctx, p.j.SessionID)
	if err != nil {
		return session.Session{}, err
	}
	if !p.sessionOwnsCandidate(fresh) || fresh.Revision != bound.Revision {
		return session.Session{}, errors.New("session revision does not match the pending checkpoint result")
	}
	head, err := sessionWorkspaceCommitContext(ctx, fresh.Workspace, "HEAD")
	if err != nil || head != p.j.CommittedRevision {
		return session.Session{}, errors.New("workspace identity does not match the pending checkpoint result")
	}
	if err := p.s.validateSessionForkAuthority(ctx, fresh); err != nil {
		return session.Session{}, err
	}
	if err := p.s.store.CompleteOperation(ctx, p.op.ID, "session", bound.ID, result); err != nil {
		return session.Session{}, err
	}
	// Success: the journal, retained candidate and original snapshot, and the
	// retained body are all redundant now. The receipt is durable, so a cleanup
	// failure must not turn the completed restore back into an error; the
	// leftover custody is removed on discard and counted in storage meanwhile.
	if err := os.RemoveAll(p.dir); err != nil {
		p.s.log.Warn("checkpoint publish custody remained after receipt",
			"operation_id", p.op.ID, "error", p.s.operationalErrorDetail(err))
	}
	_ = os.Remove(filepath.Join(p.s.stateRoot, "workspace-checkpoints", p.op.ID+".checkpoint"))
	return bound, nil
}

// rollback restores the pinned original workspace content and proves the restoration.
// It is idempotent: reset/read-tree reach the tracked state from any intermediate one,
// clean removes the candidate's files, then the original untracked and task bytes are
// rewritten. Git objects and LFS payloads were never deleted, and submodule gitlinks
// are unchanged.
func (p *checkpointPublish) rollback(ctx context.Context, sess session.Session) error {
	original := p.j.Original
	workspace := p.j.Workspace
	for _, args := range [][]string{
		{"reset", "--hard", original.Head},
		{"read-tree", "--reset", "-u", original.TrackedTree},
	} {
		if _, _, err := runSessionCompanionGitContext(ctx, workspace, sessionWorkspaceGitOutputLimit, args...); err != nil {
			return err
		}
	}
	if err := materializeSessionSubmodules(ctx, p.j.Repository, workspace, original.Head); err != nil {
		return err
	}
	if _, _, err := runSessionCompanionGitContext(ctx, workspace, sessionWorkspaceGitOutputLimit,
		"clean", "-qfdx", "-e", "/"+forkspace.GenerationMarkerName); err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	restore := func(entries []workerproto.WorkspaceCheckpointFileEntry, task bool) error {
		for _, entry := range entries {
			path := entry.PathBytes
			if path == nil {
				decoded, decodeErr := base64.StdEncoding.DecodeString(entry.PathB64)
				if decodeErr != nil {
					return decodeErr
				}
				path = decoded
			}
			if task {
				path = append([]byte(tasks.TasksRoot+"/"), path...)
			}
			file, err := os.Open(filepath.Join(p.dir, checkpointPublishOriginalDir, "files", filepath.FromSlash(entry.Entry)))
			if err != nil {
				return err
			}
			writeErr := writeRestoredCheckpointStream(root, path, entry.Mode, reviewScanReader{ctx, file})
			if closeErr := file.Close(); writeErr != nil {
				return errors.Join(writeErr, closeErr)
			} else if closeErr != nil {
				return closeErr
			}
		}
		return nil
	}
	if err := restore(original.Untracked, false); err != nil {
		return err
	}
	if err := restore(original.TaskFiles, true); err != nil {
		return err
	}
	digest, err := checkpointWorkspaceStateDigest(ctx, workspace)
	if err != nil {
		return err
	}
	if digest != p.j.originalDigest() {
		return errors.New("rolled-back workspace does not match its pinned original")
	}
	if err := p.s.validateSessionForkAuthority(ctx, sess); err != nil {
		return err
	}
	if p.j.Phase != string(checkpointPublishRolledBack) {
		p.j.Phase = string(checkpointPublishRolledBack)
		if err := writeCheckpointPublishJournal(p.dir, &p.j); err != nil {
			return err
		}
	}
	return nil
}

// removeCheckpointPublish cleans a terminal operation's transaction directory. It is
// only called on session discard after the operation was made terminal.
func (s *Service) removeCheckpointPublish(operationID string) error {
	dir, err := s.checkpointPublishDir(operationID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncReviewPath(filepath.Dir(dir))
}

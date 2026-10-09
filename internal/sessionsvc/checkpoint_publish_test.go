package sessionsvc

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

type restorePublishFixture struct {
	t          *testing.T
	service    *sessionFixture
	source     session.Session
	checkpoint workerproto.WorkspaceCheckpoint
	bundle     []byte
	offer      string
}

func newRestorePublishFixture(t *testing.T, empty bool) *restorePublishFixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	sessionWorkspaceWrite(t, filepath.Join(repo, ".gitignore"), ".agent/tasks/\n")
	sessionWorkspaceWrite(t, filepath.Join(repo, "tracked.txt"), "base\n")
	git("add", ".")
	git("commit", "-qm", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	t.Cleanup(func() {
		service.testAfterRestorePublished = nil
		service.testCrashAfterRestorePublished = nil
		_ = service.Stop()
	})
	ctx := context.Background()
	offer := "record:task_offer:publish"
	source, err := service.CreateRemoteSession(ctx, "publish-source-create", service.request(t, offer))
	if err != nil {
		t.Fatal(err)
	}
	source, err = service.EnsureWorkspaceTask(ctx, "publish-source-ensure", EnsureWorkspaceTaskRequest{
		SessionID: source.ID, ExpectedRevision: source.Revision,
		Task: tasks.ControllerTaskDraft{
			OfferRef: offer, Title: "Transactionally restore",
			Prompt: "The publish must be undoable.", SuccessChecks: []string{"rolled back"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		sessionWorkspaceWrite(t, filepath.Join(source.Workspace, "committed.txt"), "committed addition\n")
		sessionWorkspaceGit(t, source.Workspace, "add", "committed.txt")
		sessionWorkspaceGit(t, source.Workspace, "commit", "-qm", "candidate work")
		if err := os.WriteFile(filepath.Join(source.Workspace, "helper.sh"),
			[]byte("#!/bin/sh\necho restored\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	captured, err := service.CheckpointWorkspace(ctx, "publish-source-checkpoint", CheckpointWorkspaceRequest{
		SessionID: source.ID, SessionRef: "publish-source", ExpectedRevision: source.Revision,
		PlacementGeneration: 1, RepositoryRef: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := readCheckpointBundle(t, service.Service, captured.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return &restorePublishFixture{
		t: t, service: service, source: source,
		checkpoint: captured.Checkpoint, bundle: bundle, offer: offer,
	}
}

func (f *restorePublishFixture) createTarget(key string) session.Session {
	f.t.Helper()
	target, err := f.service.CreateRemoteSession(context.Background(), key, f.service.request(f.t, f.offer))
	if err != nil {
		f.t.Fatal(err)
	}
	sessionWorkspaceWrite(f.t, filepath.Join(target.Workspace, "original-canary.txt"), "original canary\n")
	return target
}

func (f *restorePublishFixture) request(target session.Session, stream bool) RestoreWorkspaceCheckpointRequest {
	req := RestoreWorkspaceCheckpointRequest{
		SessionID: target.ID, ExpectedRevision: target.Revision, Checkpoint: f.checkpoint,
	}
	if stream {
		req.Stream = bytes.NewReader(f.bundle)
	}
	return req
}

func (f *restorePublishFixture) publishDir(operationID string) string {
	dir, err := f.service.checkpointPublishDir(operationID)
	if err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func assertOriginalWorkspace(t *testing.T, target session.Session) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(target.Workspace, "original-canary.txt")); err != nil || string(got) != "original canary\n" {
		t.Fatalf("original canary = %q, %v", got, err)
	}
	for _, candidate := range []string{"committed.txt", "helper.sh"} {
		if _, err := os.Stat(filepath.Join(target.Workspace, candidate)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rolled-back workspace retained candidate file %s: %v", candidate, err)
		}
	}
}

func assertCandidateWorkspace(t *testing.T, target session.Session) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(target.Workspace, "committed.txt")); err != nil || string(got) != "committed addition\n" {
		t.Fatalf("candidate committed.txt = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target.Workspace, "helper.sh")); err != nil || string(got) != "#!/bin/sh\necho restored\n" {
		t.Fatalf("candidate helper.sh = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target.Workspace, "tracked.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("candidate tracked.txt = %q, %v", got, err)
	}
}

// A user write in the publish window blocks the switch; the workspace rolls back to
// its pinned original, the operation stays recoverable, and the same key completes
// from the retained candidate without re-streaming the body.
func TestCheckpointPublishUserWriteBlocksSwitchAndRetriesWithoutStream(t *testing.T) {
	ctx := context.Background()
	f := newRestorePublishFixture(t, false)
	target := f.createTarget("publish-blocked-create")
	f.service.testAfterRestorePublished = func() {
		sessionWorkspaceWrite(t, filepath.Join(target.Workspace, "user-added.txt"), "user write during switch\n")
	}
	if _, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-blocked-once", f.request(target, true)); err == nil ||
		!strings.Contains(err.Error(), "workspace changed before the checkpoint switch") {
		t.Fatalf("user write during the switch = %v; want a blocked publish", err)
	}
	assertOriginalWorkspace(t, target)
	if _, err := os.Stat(filepath.Join(target.Workspace, "user-added.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the blocking user write survived the rollback")
	}
	op, err := f.service.Store().GetOperation(ctx, "publish-blocked-once")
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("blocked publish terminalized the operation: %+v, %v", op, err)
	}
	if info, err := os.Stat(f.publishDir(op.ID)); err != nil || !info.IsDir() {
		t.Fatalf("blocked publish dropped its retained candidate: %v", err)
	}
	current := mustSession(t, f.service, target.ID)
	if current.Revision != target.Revision || current.WorkspaceTask != nil {
		t.Fatalf("blocked publish changed the session row: %+v", current)
	}
	// Same operation, no body stream: the retained verified candidate drives the retry.
	f.service.testAfterRestorePublished = nil
	restored, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-blocked-once", f.request(target, false))
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateWorkspace(t, restored)
	if _, err := os.Stat(filepath.Join(restored.Workspace, "original-canary.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the completed publish kept the original untracked file")
	}
	if _, err := os.Stat(f.publishDir(op.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed publish retained its transaction directory")
	}
}

// A session-row compare-and-swap failure rolls the files back and keeps the
// candidate; restart recovery then finishes from the journal.
func TestCheckpointPublishSessionCommitFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	f := newRestorePublishFixture(t, false)
	target := f.createTarget("publish-cas-create")
	db, err := sql.Open("sqlite", filepath.Join(f.service.Store().Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_restore_commit BEFORE UPDATE ON sessions
		WHEN NEW.workspace_task IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'injected switch loss'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-cas-once", f.request(target, true)); err == nil ||
		!strings.Contains(err.Error(), "injected switch loss") {
		t.Fatalf("missing injected switch failure: %v", err)
	}
	assertOriginalWorkspace(t, target)
	current := mustSession(t, f.service, target.ID)
	if current.Revision != target.Revision || current.WorkspaceTask != nil {
		t.Fatalf("failed switch changed the session row: %+v", current)
	}
	op, err := f.service.Store().GetOperation(ctx, "publish-cas-once")
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("failed switch terminalized the operation: %+v, %v", op, err)
	}
	if _, err := db.Exec("DROP TRIGGER fail_restore_commit"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcileInterruptedOperations(ctx, true); err != nil {
		t.Fatal(err)
	}
	op, err = f.service.Store().GetOperation(ctx, "publish-cas-once")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("recovery did not finish the restore: %+v, %v", op, err)
	}
	restored := mustSession(t, f.service, target.ID)
	if restored.WorkspaceTask == nil || restored.BaseCommit != f.checkpoint.BaseRevision {
		t.Fatalf("recovery did not bind the restored task: %+v", restored)
	}
	assertCandidateWorkspace(t, restored)
	if _, err := os.Stat(f.publishDir(op.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed recovery retained its transaction directory")
	}
}

// A crash between the published phase and the switch resumes at restart:
// the tree already matches and the operation only needs its CAS + receipt.
func TestCheckpointPublishRestartContinuesPublishedTree(t *testing.T) {
	ctx := context.Background()
	f := newRestorePublishFixture(t, false)
	target := f.createTarget("publish-crash-create")
	f.service.testCrashAfterRestorePublished = func() bool { return true }
	if _, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-crash-once", f.request(target, true)); err == nil ||
		!strings.Contains(err.Error(), "simulated process exit") {
		t.Fatalf("missing simulated crash: %v", err)
	}
	// No rollback ran: the candidate tree is live and uncommitted.
	assertCandidateWorkspace(t, mustSession(t, f.service, target.ID))
	if _, err := os.Stat(filepath.Join(target.Workspace, "original-canary.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published tree still held the original canary")
	}
	txn, err := f.service.openCheckpointPublish(func() string {
		op, _ := f.service.Store().GetOperation(ctx, "publish-crash-once")
		return op.ID
	}())
	if err != nil || txn == nil || txn.j.Phase != string(checkpointPublishPublished) {
		t.Fatalf("journal did not survive the crash in published phase: %+v, %v", txn, err)
	}
	f.service.testCrashAfterRestorePublished = nil
	if err := f.service.reconcileInterruptedOperations(ctx, true); err != nil {
		t.Fatal(err)
	}
	op, err := f.service.Store().GetOperation(ctx, "publish-crash-once")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("restart recovery did not complete: %+v, %v", op, err)
	}
	assertCandidateWorkspace(t, mustSession(t, f.service, target.ID))
}

// A crash that leaves a user write on the published tree is rolled back at
// restart and completed on the following pass; the candidate is retained
// throughout, so no body stream is needed.
func TestCheckpointPublishRestartRollsBackDivergedPublishedTree(t *testing.T) {
	ctx := context.Background()
	f := newRestorePublishFixture(t, false)
	target := f.createTarget("publish-diverge-create")
	f.service.testCrashAfterRestorePublished = func() bool {
		sessionWorkspaceWrite(t, filepath.Join(target.Workspace, "downtime-write.txt"), "written while the daemon was dead\n")
		return true
	}
	if _, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-diverge-once", f.request(target, true)); err == nil {
		t.Fatal("simulated crash returned no error")
	}
	f.service.testCrashAfterRestorePublished = nil
	// First restart pass fences the write, rolls back, and stays running.
	if err := f.service.reconcileInterruptedOperations(ctx, true); err != nil {
		t.Fatal(err)
	}
	op, err := f.service.Store().GetOperation(ctx, "publish-diverge-once")
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("diverged publish was not rolled back to a retryable state: %+v, %v", op, err)
	}
	assertOriginalWorkspace(t, target)
	if _, err := os.Stat(filepath.Join(target.Workspace, "downtime-write.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("downtime write survived the restart rollback")
	}
	// Second pass re-publishes the retained candidate and finishes.
	if err := f.service.reconcileInterruptedOperations(ctx, true); err != nil {
		t.Fatal(err)
	}
	op, err = f.service.Store().GetOperation(ctx, "publish-diverge-once")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("second restart pass did not complete: %+v, %v", op, err)
	}
	assertCandidateWorkspace(t, mustSession(t, f.service, target.ID))
}

// The fork branch authority changing in the publish window blocks the
// switch and rolls back; restoring it lets the retained candidate finish.
func TestCheckpointPublishAuthorityChangeBlocksSwitch(t *testing.T) {
	ctx := context.Background()
	f := newRestorePublishFixture(t, false)
	target := f.createTarget("publish-authority-create")
	const stolen = "publish-authority-stolen"
	f.service.testAfterRestorePublished = func() {
		sessionWorkspaceGit(t, target.Workspace, "branch", "-m", stolen)
	}
	if _, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-authority-once", f.request(target, true)); err == nil ||
		!strings.Contains(err.Error(), "branch authority changed") {
		t.Fatalf("branch authority change = %v; want a blocked switch", err)
	}
	assertOriginalWorkspace(t, target)
	if err := f.service.validateSessionForkAuthority(ctx, mustSession(t, f.service, target.ID)); err != nil {
		t.Fatalf("rollback left fork authority broken: %v", err)
	}
	sessionWorkspaceGit(t, target.Workspace, "branch", "-m", target.ForkName)
	f.service.testAfterRestorePublished = nil
	restored, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-authority-once", f.request(target, false))
	if err != nil {
		t.Fatal(err)
	}
	assertCandidateWorkspace(t, restored)
}

// A checkpoint carrying no changes at all publishes and cleans up like any other.
func TestCheckpointPublishEmptyCheckpoint(t *testing.T) {
	ctx := context.Background()
	f := newRestorePublishFixture(t, true)
	if f.checkpoint.CommittedRevision != f.checkpoint.BaseRevision {
		t.Fatal("empty fixture unexpectedly carries committed changes")
	}
	target := f.createTarget("publish-empty-create")
	restored, err := f.service.RestoreWorkspaceCheckpoint(ctx, "publish-empty-once", f.request(target, true))
	if err != nil {
		t.Fatal(err)
	}
	if restored.WorkspaceTask == nil || restored.BaseCommit != f.checkpoint.BaseRevision {
		t.Fatalf("empty restore did not bind: %+v", restored)
	}
	if got, err := sessionWorkspaceCommit(restored.Workspace, "HEAD"); err != nil || got != f.checkpoint.BaseRevision {
		t.Fatalf("empty restore moved HEAD: %s %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(restored.Workspace, "committed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty restore invented candidate files")
	}
	if _, err := os.Stat(filepath.Join(restored.Workspace, "original-canary.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty restore kept the original untracked file")
	}
	item, ok := mustCurrentTask(t, filepath.Join(restored.Workspace, tasks.TasksRoot), restored.WorkspaceTask.ID)
	if !ok || item.ID != restored.WorkspaceTask.ID {
		t.Fatalf("empty restore lost the task projection: %+v", item)
	}
	op, err := f.service.Store().GetOperation(ctx, "publish-empty-once")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("empty restore did not complete: %+v, %v", op, err)
	}
}

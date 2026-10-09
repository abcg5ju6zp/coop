---
name: checkpoint-restore-publish
description: RestoreWorkspaceCheckpoint publishes the verified tree through a journaled undoable transaction (prepared/published/committed/rolled_back); the session-row CAS is the only confirmation point
subsystem: sessionsvc
sources: [internal/sessionsvc/checkpoint_publish.go, internal/sessionsvc/checkpoint_restore.go, internal/sessionsvc/operation_recovery.go, internal/sessionsvc/tracked_snapshot.go]
updated: 2026-10-09
---

The restore's destructive half is one journaled transaction under
`workspace-checkpoints/<op>.publish/` (`internal/sessionsvc/checkpoint_publish.go`):

- Isolated verification in `.staging` is unchanged; after it passes, `prepareCheckpointPublish`
  pins the ORIGINAL workspace — HEAD, the tracked tree (pinned into the real object db with
  `GitFetchPinnedContext` out of the private index repo, which is deleted immediately after
  `withSessionTrackedTree` returns), untracked non-ignored bytes, and the whole `.agent/tasks`
  tree minus `tmp/` — then durably writes `publish.json` (phase `prepared`) and only `run()`
  moves the stage to `candidate/`.
- Phases are the crash decision: `published` = tree holds the candidate + digest but session
  row uncommitted; `committed` = `RestoreWorkspaceTask` CAS landed, receipt may be missing;
  `rolled_back` = original restored, candidate retained.
- The final fence (revision/parked/unused, pending-restore SQL, branch, fork authority,
  workspace digest) runs BEFORE the CAS. Any miss rolls back and the op STAYS RUNNING
  (retryable, `CodeOperationUncertain` envelope) — it is never failed after the journal exists.
- Post-`committed`, recovery only backfills the receipt from session revision + HEAD + same op
  and must NEVER materialize again; writes landing after the confirmation point are the owner's.
- Restart (`resumeInterruptedRestore`) routes journal ops through the state machine without a
  body stream; only ops with no journal replay the retained `.checkpoint` body. A missing
  candidate directory drops the journal and falls back to that replay.

Fidelity is intentionally the checkpoint format's own model: ignored files outside
`.agent/tasks` are wiped by `git clean -fdx` in both directions and are not rolled back.

## Changelog
- 2026-10-09 — created with the undoable publish transaction; verified against the
  `TestCheckpointPublish*` suite and the retained-body interrupted-completion test.

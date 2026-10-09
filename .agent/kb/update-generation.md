---
name: update-generation
description: coop update is a recoverable binary+image generation switch — candidate staging, candidate-built image, one atomic rename, and a persisted pending-session restart; what each failure stage keeps
subsystem: cli
sources: [internal/cli/update_generation.go, internal/cli/build_cmd.go, internal/cli/selfupdate.go, internal/cli/cli.go, internal/box/image.go]
updated: 2026-10-09
---

`coop update` no longer replaces the binary first and builds from the old process. The full update is
one persisted generation (`BoxHome/update-generation.json`):

1. **Stage** — the newer archive is downloaded and checksum-verified exactly as before
   (`fetchVerifiedReleaseBinary`), but written to `.coop-candidate-<version>` BESIDE the executable
   (`stageReleaseBinary`, 0755+fsync) instead of renamed over it. The exe is untouched.
2. **Build from the candidate** — the parent execs `<staged> internal box-plan` / `internal
   box-build` (hidden `coop internal` subcommand; env inherited, stdio passed through, result JSON in
   a `--result-file`). The CHILD's Main runs `box.ResolveBaseImage`, so the image built is
   `coop-box:<the candidate's baseDefHash>` from the candidate's embedded client lock — never the
   running binary's definition. The parent verifies the planned image actually exists afterwards.
3. **Switch** — supervised box ids are snapshotted and written to `pending_recycle` BEFORE the
   rename; `commitStagedBinary` is one same-dir atomic rename (the single point of activation);
   only then is the candidate record cleared.
4. **Recycle** — recorded ids are removed individually (`rm -f`), not by a broad label sweep, so a
   retry can never kill a box a new launch already started. Unsupervised boxes are counted, never
   killed.

Why candidate-exec: an ACP supervisor re-execs the disk binary per box, so after the rename
reconnects run the new code, and because bases are per-definition tags ([[box-base-image-tags]])
the pre-built candidate tag is what those relaunches resolve. Building with the old binary's
definition rebuilt the OLD tag: the new CLI's first runs still had to repair-build theirs.

Failure/exit semantics (the contract in the user-facing help):
- download/checksum/stage or candidate BUILD failure → candidate record + staged file discarded,
  old binary+image remain the one active combination.
- list/rename failure before the rename → candidate kept (`image-ready` is resumable); the armed
  pending is dropped because the rename did not land.
- recycle failure AFTER the rename → new combination stays active; surviving ids + cause persist in
  `pending_recycle` and are retried silently once per process right after runtime detection
  (`ensureRuntimeContext` → `resumePendingRecycle`), bounded by `recycleBoxesTimeout`, or loudly by
  the next `coop update`.
- startup heal (`healUpdateGeneration` in Main, skipped for `coop internal`): running version ==
  candidate version means the rename landed post-crash → clear candidate, KEEP pending; running
  older means an unswitched stage, which is left for the next `coop update` to byte-verify and
  resume (`candidateIntact`, sha256 of staged bytes) or discard when a newer release superseded it.
  Ordinary commands never delete a resumable generation. A pending record newer than the running
  binary is never reaped (external downgrade guard).

Boundaries: `--self-only` stages+renames and stops (no image, no recycle, but preserves an
unrelated pending); `--box-only` builds the ACTIVE binary's image, never touches the candidate, and
its label-wide recycle clears only a pending matching the running binary. Same-version re-runs are
binary no-ops while still fresh-rebuilding the image. The `internal` child is excluded from
update-check notices and generation heal, or it would fight the parent over the state file.
`coop update --check` and the daily notice are unchanged.

## Changelog
- 2026-10-09 — created with the recoverable generation switch; verified against
  internal/cli/update_generation.go and the pinned update help/--check transcripts.

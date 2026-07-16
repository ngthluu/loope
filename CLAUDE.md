# CLAUDE.md

Project instructions for the `loope` issue-processing daemon.

## Lessons learned

### On failure, continue from existing state — don't delete and restart from zero

When a step fails (a git worktree add, a pipeline run, a resume), prefer
**recovering and continuing from whatever already exists** over deleting it and
starting from scratch. Blowing away state on every failure throws out partial
progress and, worse, can create infinite retry loops when the failure recurs
each cycle.

This is a general working principle, not just a failure-recovery rule: **never
remove and start from zero — always try to build on what already exists.**
Reusing existing state saves tokens and effort; recreating it from scratch
wastes both. Reach for existing artifacts, partial results, and prior work
before regenerating anything.

Concretely:

- If a worktree still exists on the path, **reuse it** and continue working on
  it. Only reclaim (delete) a leftover when it is truly unusable — e.g. a bare
  orphaned branch whose worktree is gone.
- Reclaim/force-delete only as a last resort, gated on the specific condition
  that makes reuse impossible, not as the default reaction to any error.
- Keep the happy path untouched: recovery logic belongs in the error branch, so
  a normal run never pays for it.

See `Worktree.Create` in `worktree.go` for the pattern: try the operation; on
failure, reuse the existing worktree if present, otherwise reclaim the bare
leftover and retry once.

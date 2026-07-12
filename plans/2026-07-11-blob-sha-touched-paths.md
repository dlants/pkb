# Objective and Context

## User request (verbatim)

> I want to revisit the design around how this gets triggered. Basing it on commits is feeling really awkward. Especially with this change, reindexing is actually quite fast, and I want to make it possible to do it as a precommit hook.
>
> I think we can still leverage git for this maybe? As we're about to commit a file, we can observe the diff, which can help us identify which files need reindexing. Can we see what commit hash the file *will* have at this moment, so we can index the file content hash as a way of verifying which file needs to be indexed vs not?
>
> this seems like a good direction. Let's write a plan

## What we're building and why

Today PKB decides *which files need re-embedding* by diffing the current target
commit against the last-indexed commit stored in `pkb-state.toml`
(`touchedPaths` uses `DiffNameStatus` / `IsAncestor` / `MergeBase`). This
commit-anchored model is awkward: it breaks on amends, rebases, force-pushes,
missing objects, and same-commit model swaps (the reason for the recent
`State.Model` patch), and it makes a pre-commit hook impossible because there is
no commit yet.

The key realization: **reuse is already keyed on the git blob SHA**, not the
commit. `indexedEntry.sha` (from the `files` table) records each file's blob
sha, and the reindex loop already skips a file when
`prevEntry.sha == blobSha && same model && same mode`. The commit only serves to
*narrow* the candidate set — an optimization that is redundant because `LsTree`
enumerates the whole tree on every run anyway and the per-file skip is free.

So we replace commit-diffing with a direct **blob-sha comparison** between the
target tree and the stored per-file blob shas. This:

- makes the touched set correct regardless of commit history shape;
- lets us index a **staged tree** (via `git write-tree`) with zero commit, which
  is exactly what a pre-commit hook needs — the blob shas of staged content are
  already final and match what the commit will contain;
- subsumes the `State.Model` model-swap fix (a new model has an empty `indexed`
  map, so every tree file differs and is re-embedded naturally);
- removes the divergence/ancestor/merge-base machinery entirely.

## Key entities

- `git.Repo` (`internal/git/git.go`) — plumbing wrapper. Already has `LsTree(ref)`
  returning `[]RepoFile{Path, BlobSha}`. Needs a way to materialize the staged
  index as a tree (`git write-tree`) so `LsTree` can list it.
- `index.Options.touchedPaths` (`internal/index/manager.go`) — computes the set
  of paths needing work. This is the core of the change.
- `index.State` (`internal/index/manager.go`) — persisted marker
  (`pkb-state.toml`): `Commit`, `FileCount`, `ChunkCount`, `Model`. After this
  change `Commit`/`Model` are informational only; correctness comes from the
  blob shas already in `pkb.db`.
- `store.IndexedFiles(model)` — returns `path -> {Sha, MinorSpec}`; already the
  source of truth for stored blob shas.
- `index.Reindex` / `index.Estimate` / `index.Healthcheck` — the three callers
  that resolve a target ref, build `treeMap`, and (for the first two) call
  `touchedPaths`.
- `main.go` `runReindex` — CLI entry; gains a `--staged` flag.

## Relevant files

- `internal/git/git.go` — add `WriteTree()`; the diff/ancestor/merge-base helpers
  become dead once `touchedPaths` is rewritten.
- `internal/index/manager.go` — rewrite `touchedPaths`, adjust `Reindex`,
  `Estimate`, `Healthcheck`, and `State`.
- `internal/index/manager_test.go` — retarget existing commit-diff tests to the
  blob-diff behavior; add staged-tree coverage.
- `main.go` — `--staged` flag on `reindex`.
- `README.md` — document the pre-commit trigger and the staged flow.
- sample pre-commit hook (new file under a `hooks/` or docs location).

# Design

## Target resolution

Introduce the notion of a **target tree** rather than a target commit:

- Normal (CI / manual): target tree = the tree of `HEAD` (as today, via
  `LsTree("HEAD")`).
- Staged (pre-commit): target tree = `git write-tree`, which serializes the
  current index (staging area) into a tree object and returns its sha, with no
  commit and no mutation of the working tree or index. Feed that sha to
  `LsTree(treeSha)` — the existing parser works unchanged because `ls-tree -r`
  accepts any tree-ish.

`write-tree` writes tree/blob objects into the object DB as a side effect; this
is harmless (git gc handles unreferenced trees) and blob shas for staged content
already exist from `git add`.

## Blob-sha touched-path computation

Rewrite `touchedPaths` to depend only on `treeMap` (target: `path -> blobSha`)
and `indexed` (stored: `path -> {model, sha, minorSpec}`). A path is touched
when any of:

1. **New/modified**: `path` is a candidate in `treeMap` and either not in
   `indexed`, or `indexed[path].sha != treeMap[path]`.
2. **Deleted**: `path` is in `indexed` but not in `treeMap` (the reindex loop's
   existing `else` branch deletes it).
3. **Mode flip**: `path` is an indexed candidate whose stored auto-chunk mode
   (`minorSpec == autoChunkMinorSpec`) disagrees with `o.isContextual(path)` —
   the existing `addModeFlips` logic, folded into the same pass.

Everything else (unchanged blob, same model, same mode) is untouched. This makes
the touched set *exactly* the work set; the loop's existing sha/model/mode skip
check becomes a redundant backstop rather than the primary filter.

`prev *State` and `targetSha` drop out of the `touchedPaths` signature. The
`full` concept survives only inside `Estimate` (project a from-scratch run by
treating every tree candidate as touched and disabling reuse) — that path
already bypasses `touchedPaths`, so it is unaffected.

## State marker

`State.Commit` and `State.Model` are no longer read for correctness. Keep
recording them for human-facing output (`stats`, reindex summary) — record the
resolved target sha (HEAD commit for normal runs; for staged runs record the
parent `HEAD` so `stats` stays meaningful, or leave blank). `FileCount` /
`ChunkCount` stay as-is. The `State.Model` field and its `touchedPaths`
mismatch check added earlier are removed, since blob-diffing handles model swaps.

Healthcheck currently warns when `state.Commit != HEAD`. With staged/pre-commit
flows this equality is no longer meaningful; replace that check with the
blob-coverage check it effectively already performs (every HEAD tree candidate
has a matching stored blob sha under an active model). The file/chunk count
checks stay.

## Pre-commit operational flow

A sample hook runs `pkb reindex --staged`, then `git add pkb.db pkb-state.toml`
so the refreshed index rides along in the same commit. Because we indexed the
staged tree, the blob shas recorded equal the blob shas the commit will contain,
so the very next (post-commit) `pkb reindex` is a no-op. Document the Git LFS
interaction: the hook must not fight the LFS pre-push hook, and `pkb.db` is LFS-
tracked, so `git add` of the pointer is fine.

## Alternatives considered

- **Keep commit-diff, add a staged special-case**: rejected — still carries all
  the commit-history edge cases and the `State.Model` patch; more code, not less.
- **`git ls-files -s` instead of `write-tree`**: viable for staged blob shas, but
  `write-tree` reuses the existing `LsTree` path verbatim and yields one tree-ish
  that both `LsTree` and future tooling can consume uniformly.

## Invariants

- Reuse identity is `(path, model, blobSha, autoChunk-mode)`; a file is embedded
  iff that tuple is absent from the store. Must hold before and after.
- The touched set must be a superset of every file whose committed content will
  differ from what is stored — otherwise stale vectors survive. Blob-sha
  inequality guarantees this.
- Deletions must remain detected (stored-but-not-in-tree), or orphan rows leak.
- `write-tree` must not alter the working tree or staging area.
- Indexing staged content means `pkb.db` reflects not-yet-committed blobs; those
  blob shas must equal the post-commit blob shas (true: blob sha is pure content
  hash), so a post-commit reindex is a no-op.
- Model swap with no content change must re-embed all files (empty `indexed` map
  for the new model ⇒ all tree files touched).

# Stages

## Stage 1 — `WriteTree` plumbing

**Status: DONE.** Added `Repo.WriteTree()` in `internal/git/git.go` (runs `git write-tree`, trims output). Added `internal/git/git_test.go` with `TestWriteTreeListsStagedFiles` verifying a staged-but-uncommitted file lists via `LsTree(treeSha)` and that `git status --porcelain` is unchanged before/after. Full `go build`/`go vet`/`go test ./...` pass.

- Goal: `git.Repo` can materialize the staging area into a tree sha usable by
  `LsTree`.
- Verification:
  - Behavior: `WriteTree` returns a tree-ish that lists the staged files.
  - Setup: temp repo, write + `git add` a file (do not commit).
  - Actions: call `WriteTree`, then `LsTree(treeSha)`.
  - Expected outcome: the staged file appears with its blob sha; working tree and
    index are unchanged.
- Before moving on: `go build ./...`, `go test ./internal/git/...` (add if none),
  `go vet ./...` pass.

## Stage 2 — Blob-sha `touchedPaths`

**Status: DONE.** Rewrote `touchedPaths` in `internal/index/manager.go` to compute the work set purely from `treeMap` vs stored `indexed` blob shas: new/modified (absent or differing blob sha), deleted (indexed but not in tree), and auto-chunk mode flips folded into the same pass. Dropped the `prev *State` and `targetSha` inputs and the error return (no git calls left). Removed the commit-diff machinery inside the function (`full`/`ObjectExists`/`IsAncestor`/`MergeBase`/`DiffNameStatus` usage, `addModeFlips` closure) — note the git helpers themselves and `State.Model` removal are Stage 3. Rewired `Reindex` and `Estimate` to the new signature and dropped their now-unused `targetSha`/`repoRoot`/`readState` locals in the incremental path. Existing tests already exercise the new behavior and pass unchanged: `TestIncrementalAddModifyDelete`, `TestDivergenceViaMergeBase` (abandoned-branch file deletion via blob-diff), `TestTotalRecoveryWhenCommitGone` (bogus commit sha now simply ignored), and `TestModelChangeForcesFullReembedSameCommit` (empty `indexed` for the swapped-in model ⇒ full re-embed). Full `go build`/`go vet`/`go test ./...` green.

- Goal: `touchedPaths` computes the work set purely from `treeMap` vs `indexed`,
  with no commit inputs; `Reindex` and `Estimate` use it.
- Verification (unit, in `manager_test.go`, mock embed model):
  - Behavior: unchanged blob ⇒ no re-embed. Setup: index once, reindex with no
    changes. Expected: zero embed calls / files.
  - Behavior: modified blob ⇒ only that file re-embeds. Setup: edit one file,
    commit, reindex. Expected: one file touched.
  - Behavior: deleted file ⇒ rows removed. Setup: delete a file, reindex.
    Expected: its rows gone from the store.
  - Behavior: model swap at same commit ⇒ full re-embed (replaces the
    `State.Model` test). Setup: reindex, swap model, reindex without touching
    state. Expected: all files re-embedded under new model, old model orphaned
    rows cleaned.
  - Behavior: contextualize mode flip ⇒ text-only re-embed (retain existing
    coverage).
  - Retarget/remove `TestDivergenceViaMergeBase`,
    `TestTotalRecoveryWhenCommitGone`, `TestIncrementalAddModifyDelete`,
    `TestModelChangeForcesFullReembedSameCommit` to the new model.
- Before moving on: full `go test ./...`, `go vet ./...` pass.

**Status: DONE.** Removed `State.Model` (field + its write in `Reindex`); the model-swap correctness now rides entirely on blob-diffing (empty `indexed` map for the swapped-in model). Deleted the now-dead git helpers `DiffNameStatus`, `IsAncestor`, `MergeBase`, `ObjectExists`, and the `Change` struct from `internal/git/git.go`, and updated the package doc comment. Rewrote `Healthcheck`'s stale check: dropped the `state.Commit != HEAD` equality warning (meaningless under amend/rebase/staged flows) and rely on the pre-existing blob-coverage checks (missing file / stale blob / orphaned file) plus the file/chunk count checks; `StateCommit` is still recorded for human-facing output. Updated the `index` package doc comment to describe blob-sha diffing. Retargeted `TestHealthcheckHealthyAndStale` to assert the stale-blob and missing-file issues (removed the obsolete "does not match HEAD" assertion). Full `go build`/`go vet`/`go test ./...` green.

## Stage 3 — Retire commit machinery & simplify State

- Goal: remove `State.Model` + its check; remove now-dead `DiffNameStatus`,
  `IsAncestor`, `MergeBase`, and (if unused) `ObjectExists`; update `Healthcheck`
  to a blob-coverage check instead of commit equality.
- Verification:
  - Behavior: healthcheck passes when every HEAD candidate has a stored blob and
    counts match; flags a stale/missing file otherwise. Setup: index, then mutate
    the store or tree. Actions: `Healthcheck`. Expected: appropriate issue(s).
  - `go vet` / build confirm no dead references remain.
- Before moving on: full `go test ./...`, `go vet ./...` pass.

## Stage 4 — `--staged` CLI + pre-commit hook + docs

- Goal: `pkb reindex --staged` indexes the staged tree; a sample pre-commit hook
  exists; README documents the pre-commit trigger alongside the CI trigger.
- Verification:
  - Behavior: `--staged` indexes staged-but-uncommitted content. Setup: temp
    repo, stage a new file. Actions: run reindex with staged target. Expected:
    the staged file is indexed; a subsequent post-commit normal reindex is a
    no-op.
  - Manual/doc: hook script stages `pkb.db`/`pkb-state.toml`; README explains the
    LFS caveat and that the index lands in the same commit.
- Before moving on: full `go test ./...`, `go vet ./...` pass.

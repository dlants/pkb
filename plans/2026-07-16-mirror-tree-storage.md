# Objective and Context

The user's request, captured verbatim across the conversation:

> I'm rethinking the lfs / sqlite storage. It's not really ideal for this pre-commit use case. The pkb.db file grows to be several mb in size, and then even tiny updates (the chunks for one file) causes the developer to have to re-upload, and re-download this whole file for every commit.
>
> I'm wondering if there are any more git-friendly storage options for this? Maybe something that would store the embeddings per-file, or in some other more fragmented way, so that small changes to embeddings would correspond to small upload/downloads?
>
> We need to balance this against speed - that is, running a query (comparing a query against every embedding) should still be really quick. So we'll need to load all the embeddings from disk into memory to do that. We're already having to do that for sqlite, but it's just a single file read.

> hm... I'm kinda finding the idea of storing things per-file (so each file gets a mirror .vec and .meta file in the pkb dir) appealing from a storage perspective. We can keep a local, cached / gitignored sqlite representation to make loading time faster. We can easily tell which files changed since the last query, and so we only ever need to load just the changed files once per commit

> we only need the full *vectors* to do the search, and then we can read only the metadata for the N closest vectors right? So we should keep embeddings in separate files from the metadata

## What we're building and why

Today the entire index lives in a single monolithic `pkb.db` SQLite file at the
repo root, tracked in git via LFS. That file is rewritten wholesale on every
reindex, so a one-file code change forces the developer to commit, push, and
pull a multi-megabyte blob. For magenta.nvim (402 files, 3,827 chunks) `pkb.db`
is 14.4 MB; a typical commit touching a few files should move kilobytes, not
megabytes.

We are replacing the single committed DB with a **git-tracked mirror tree** of
small per-source-file index artifacts, and demoting SQLite to a **gitignored,
locally-rebuildable cache** used only to make queries fast. The mirror tree is
the source of truth; the cache is derived and disposable.

Consequences of the split:
- A change to one source file rewrites exactly one small mirror artifact, so
  git stores a small delta and LFS is no longer needed (total index size is
  repo-scale: vectors are smaller than the text they embed, worst case ~2x repo).
- Queries load all vectors into memory (as today) but only ever re-read the
  mirror artifacts that changed since the cache was last synced, so steady-state
  query latency is unchanged.

## Key entities

- **Mirror artifact** — the committed per-source-file index record, split across
  **two sibling files**: for source file `src/foo.ts`, a metadata file
  `.pkb/index/src/foo.ts.meta` and a vector file `.pkb/index/src/foo.ts.vec`.
  The `.meta` file records the file's `blob_sha`, `model_name`, and every chunk's
  `(text, contextualized_text, heading_context, start/end line/col)`; the `.vec`
  file holds only the packed embeddings. Keeping vectors in their own file
  isolates high-entropy binary churn from the diffable text metadata; see Design.
- **Cache** — a gitignored SQLite db (existing schema, existing `sqlite-vec`
  vec table) at `.pkb/cache.db`. Rebuilt/synced from the mirror tree. This is
  the only thing the query path reads.
- **`ChunkKey(headingContext, text)`** — existing per-chunk reuse key
  (store.go). Reuse now resolves against the per-file mirror artifact rather
  than the SQLite cache.

## Relevant files

- `internal/store/store.go` — SQLite + sqlite-vec wrapper. Becomes the cache
  engine; `PutFile`/`IndexedFiles`/`ChunkEmbeddings`/`Search`/`Stats` operate on
  the cache. Needs a bulk "sync these paths from mirror data" entry point.
- `internal/index/manager.go` — the reindex flow. Today it calls `Store.PutFile`
  as the terminal write and reads reuse via `Store.ChunkEmbeddings`. It must
  instead write mirror artifacts and read reuse from them, then refresh the cache.
- `internal/paths/paths.go` — path types; add a source-path <-> mirror-path map.
- `main.go` — opens the store at `pkb.db`; must open the cache at `.pkb/cache.db`
  and ensure read commands sync the cache from the mirror tree first.
- `hooks/pre-commit`, `.gitattributes`, `.gitignore`, `README.md`, `context.md`,
  `pkb-state.toml` — configuration and docs that reference `pkb.db`/LFS.

# Design

## Storage layout

Source of truth is a mirror tree rooted at `.pkb/index/`, mirroring the repo's
directory structure. Each indexed source file gets **two sibling files** named
after the source path:

- **`<path>.vec`** — a tightly packed array of embeddings in chunk-ordinal order
  with a fixed float encoding, and nothing else. This is the hot, fully-scanned
  data, and the only file whose bytes are high-entropy/binary. Isolating it
  keeps vector churn out of the metadata diff and lets a query load vectors
  without touching text.
- **`<path>.meta`** — per-chunk `text`, `contextualized_text`, `heading_context`,
  and line/col spans, in the same chunk-ordinal order, plus the file-level
  `blob_sha` and `model_name`. This is cold (only the top-N winners' metadata is
  needed at query time) and diffs cleanly as text.

Chunk identity within a file is **positional** (ordinal), not content-addressed:
the vector row N corresponds to metadata record N. Vectors and metadata for a
file are always written together and always change together, so positional
identity is sufficient and keeps the format compact. (Content-addressing by
`ChunkKey` sha would only earn its keep for cross-file dedup, which per-file
mirroring does not need.)

The encoding must be **deterministic**: stable chunk ordering, fixed-width
little-endian float32 vectors, and canonical metadata serialization, so that
re-embedding a file whose chunks are unchanged produces byte-identical
artifacts. This pairs with existing per-chunk reuse so unchanged chunks never
cause a git diff.

## Cache

`.pkb/cache.db` is the existing SQLite/sqlite-vec schema, gitignored. It is
purely derived from the mirror tree. Nothing ever commits it, so LFS, merge
conflicts, and history bloat for the DB all disappear.

The cache records, per mirror artifact, a **fingerprint** used for staleness
detection — the artifact's `blob_sha` (already stored in the `files` row) is a
natural choice, matched against the mirror artifact's recorded `blob_sha`.

## Data flow

Reindex (writer):
1. Determine touched paths exactly as today (compare tree blob shas against
   recorded blob shas — but "recorded" now means the mirror artifacts, read via
   a mirror-tree scan instead of `Store.IndexedFiles`).
2. For each touched file, resolve per-chunk reuse by reading that file's
   existing mirror artifact (replacing `Store.ChunkEmbeddings`), embed the
   misses, and write the new mirror artifact atomically (temp file + rename).
   Deleted source files delete their mirror artifact.
3. After the mirror tree is updated, refresh the cache by running the same sync
   routine the query path uses (below). This keeps a single load path.

Query / stats / healthcheck (readers):
1. **Sync the cache from the mirror tree.** List `.pkb/index`; for each artifact
   whose fingerprint differs from the cache (new or changed), parse it and
   upsert its file+chunks+vectors into the cache; for each cache file row whose
   artifact is gone, delete it. Unchanged artifacts are skipped. On a fresh
   clone (no cache) this is a full build (~4 MB / 3,827 chunks for magenta —
   a few ms of reads plus insert time, once).
2. Run the existing `Store.Search` / `Store.Stats` against the cache.

## Alternatives considered and rejected

- **Different embedded DB (DuckDB, LanceDB, raw index blob).** Any monolithic
  single-file DB has the identical rewrite-the-whole-blob problem; the fix is
  the on-disk *layout*, not the query engine. Lance's fragment/manifest layout
  is git-friendlier but adds a heavier Rust-first dependency and compaction
  rewrites; not worth it when brute-force cosine over ~4k chunks is milliseconds.
- **256 hash buckets instead of per-file.** Bounded file count, but coarser git
  deltas (a commit dirties whole buckets) and loses the trivial source-file ->
  artifact mapping and the "only load changed files" property the user wants.
  Per-file is the better fit; revisit bucketing only if file count becomes a
  git-tree problem on very large repos.
- **Keep committing SQLite, drop LFS.** High-entropy vector bytes barely delta-
  compress, so committing the raw DB bloats history fast; the mirror tree +
  gitignored cache avoids committing any binary DB at all.

## Invariants

- The mirror tree is the sole source of truth. The cache must be fully
  reconstructible from it, and a stale/missing/corrupt cache must never change
  query *results* — only latency (it is rebuilt on demand).
- Cache sync must never return stale entries: an artifact whose fingerprint
  differs from the cache is always re-parsed, and an artifact absent from the
  tree is always evicted.
- Reindex remains crash-safe at file granularity: a file's `.vec` and `.meta`
  are each written atomically (temp + rename), so a crash leaves previously
  written artifacts intact and the file is simply reindexed next run. A torn pair
  (one file newer than the other) is detected via the `.meta` `blob_sha` and
  reindexed, so the two never silently drift.
- Deterministic encoding: re-embedding a file with unchanged chunks yields a
  byte-identical artifact (no spurious git diff).
- Vectors and metadata for a chunk stay aligned by ordinal; the compaction that
  drops empty chunks (`compactPrepared`) must renumber both consistently.
- `blob_sha` remains the correctness key (not the recorded commit), preserving
  the existing healthcheck / staged-index semantics.

# Stages

## Stage 1: Mirror artifact format (encode/decode)

**Status: DONE.** Implemented in `internal/mirror/mirror.go` (+ `mirror_test.go`).
Decisions:
- `.meta` uses canonical `json.MarshalIndent` (deterministic field order, trailing newline) for a diffable text format; `.vec` is a self-describing binary (`PKBV` magic + format-version/dims/count header, then row-major little-endian float32 matching `store.deserializeFloat32`).
- Chunk identity is positional; `Decode` errors when vector count != metadata count (torn-pair detection).
- `DecodeVec` loads vectors with zero dependence on `.meta`.
- Tests cover round-trip fidelity (incl. empty heading_context + multi-line text), byte determinism, standalone vector read, empty artifact, and torn-pair detection. Full suite/vet/build green.

- Goal: a self-contained package that serializes a file's chunks + vectors to
  the deterministic `.meta` + `.vec` sibling files and parses them back
  independently (vectors loadable without the metadata). No wiring into reindex yet.
- Key decisions to nail here: the exact byte layout, float encoding
  (little-endian float32, matching `deserializeFloat32`), and canonical ordering.
- Verification:
  - Behavior: round-trip fidelity — encode a set of chunks+embeddings, decode,
    get identical data back.
    - Setup: a handful of `chunk.ChunkInfo` + `embed.Embedding` fixtures,
      including a chunk with empty `heading_context` and multi-line text.
    - Actions: encode then decode.
    - Expected: decoded chunks/vectors equal the inputs field-for-field.
  - Behavior: determinism — encoding the same input twice yields byte-identical
    output.
    - Setup: one fixture set.
    - Actions: encode twice, compare bytes.
    - Expected: equal byte slices.
  - Behavior: vectors are readable without decoding metadata.
    - Setup: an artifact with several chunks.
    - Actions: read just the vectors section.
    - Expected: all embeddings recovered, no dependence on the metadata section.
- Before moving on: confirm `go build ./...`, `go vet ./...`, `go test ./...` pass.

**Status: DONE.** Implemented across `internal/mirror/tree.go` (new on-disk tree
layer) and `internal/index/manager.go`.
Decisions/deviations:
- Added `mirror.Tree` (rooted at `.pkb/index/`) with `List` (enumerate artifacts
  via `.meta` only, carrying blobSha/modelName/chunk-count), `TryRead` (full
  artifact; missing or torn/corrupt reads as "absent" so the file is simply
  reindexed), `Write` (atomic temp+rename per sibling), and `Delete`.
- `manager.go` now sources the "indexed" set from `treeIndexed(models)`
  (replacing `Store.IndexedFiles`) in reindex/estimate/healthcheck, resolves
  per-chunk reuse from `reuseMap` (replacing `Store.ChunkEmbeddings`), and
  `writeFile`/deletions go through the tree. The root `pkb.db` is never written.
- Key subtlety: `treeIndexed` filters to *active* models, so a same-content
  embedding-model swap reads the old artifact as absent and re-embeds (this
  restored the model-swap behavior that previously fell out of the per-model
  `Store.IndexedFiles` read).
- The SQLite store is now a derived cache: after the tree is updated, reindex
  calls `syncCache(models)` (the shared sync routine Stage 3 will also use from
  the read path) to reconcile the cache — upsert changed/new artifacts by
  blob-sha fingerprint, evict artifacts gone from the tree — so `Store.Stats`
  and the state marker stay correct. `main.setup()` still opens the store at
  `pkb.db` for now; pointing it at `.pkb/cache.db` and syncing on read commands
  is Stage 3.
- Tests added in `manager_test.go`: `TestReindexWritesMirrorTree` (artifacts
  written, both siblings present + decodable, no root `pkb.db`),
  `TestReindexReusesChunkKeepsArtifactBytes` (unchanged chunk keeps its vector,
  only the edited chunk re-embeds), `TestReindexDeleteRemovesArtifact`. All
  existing manager/e2e/store tests still pass against the derived cache.

## Stage 2: Reindex reads/writes the mirror tree

- Goal: `pkb reindex` writes mirror artifacts as the source of truth and resolves
  reuse from them, with the SQLite cache updated as a derived step. `pkb.db` at
  the repo root is no longer written.
- Work:
  - Add a mirror-tree reader: enumerate artifacts and their recorded blob shas
    (replacing `Store.IndexedFiles` in the reindex/estimate/healthcheck flows),
    and a per-path reuse map from an artifact (replacing `Store.ChunkEmbeddings`).
  - Replace `writeFile`'s `Store.PutFile` call with an atomic mirror-artifact
    write; deletions remove the artifact.
  - After the tree is updated, sync the cache (Stage 3 routine) so stats/state
    counts stay correct.
- Verification:
  - Behavior: reindex produces one artifact per indexed file with correct
    content.
    - Setup: a small temp git repo fixture (mirror `internal/smoke` / index
      tests) with a couple of code/markdown files and the mock embedding model.
    - Actions: run reindex.
    - Expected: `.pkb/index` contains the expected artifacts; decoding one yields
      the file's chunks + vectors; no `pkb.db` at repo root.
  - Behavior: per-chunk reuse still holds — an unchanged chunk is not re-embedded
    and its artifact bytes for that chunk are unchanged.
    - Setup: index a file, then modify one chunk and reindex.
    - Actions: compare embed-call counts (mock model counter) and artifact bytes.
    - Expected: only the changed chunk re-embeds; unchanged chunks reuse vectors.
  - Behavior: deleting a source file removes its artifact.
    - Setup: index two files, delete one, reindex.
    - Actions: inspect `.pkb/index`.
    - Expected: the deleted file's artifact is gone.
- Before moving on: confirm tests, vet, and build pass; update `manager_test.go`
  / `store_test.go` / `e2e_test.go` expectations.

## Stage 3: Cache sync + query path

**Status: DONE.** The query path now opens the gitignored `.pkb/cache.db` and
syncs it from the mirror tree before searching.
Decisions/deviations:
- The sync routine already existed as `Options.syncCache` (added in Stage 2 for
  the writer). Stage 3 exposes it to readers via a new exported
  `index.SyncCache(o)` that ensures each active model's vec table then runs the
  same fingerprint-diff reconcile (upsert changed/new by blob-sha+model, evict
  artifacts gone from the tree). One load path, shared by writer and readers.
- `index.Search` calls `SyncCache` first, so a cold or stale cache is rebuilt on
  demand and never changes results — only latency.
- `stats` and `healthcheck` deliberately do **not** touch the cache: `stats`
  reads the `pkb-state.toml` marker and `healthcheck` reads the mirror tree
  directly (`mirrorTree().List()`), so neither depends on the derived cache.
  Only `search` queries the store, so only it syncs.
- `main.setup()` now opens the store at `.pkb/cache.db` (const `cacheRelPath`),
  `MkdirAll`-ing `.pkb` first, and the usage text describes the mirror tree +
  local cache instead of `pkb.db`. Gitignoring the cache and retiring the
  committed `pkb.db`/LFS remain Stage 4.
- Tests added in `manager_test.go`: `TestSearchColdCacheMatchesWarm` (empty cache
  rebuilds and reproduces warm results exactly), `TestSearchEvictsRemovedArtifact`
  (read-path eviction of an artifact deleted from the tree while the cache still
  holds its rows), `TestSearchIncrementalReflectsEditedArtifact` (edited artifact
  is re-parsed and reflected in results). Full suite/vet/build green.

- Goal: read commands (`search`, `stats`, `healthcheck`) open the gitignored
  `.pkb/cache.db`, sync it incrementally from the mirror tree, then query it.
  Deleting the cache and re-running yields identical results.
- Work:
  - Implement the sync routine: diff mirror artifacts' fingerprints against cache
    `files` rows; upsert changed artifacts (reuse `PutFile` internals to write
    chunks+vectors into the cache), evict missing ones.
  - Point `main.setup()` at `.pkb/cache.db`; call sync before read operations.
- Verification:
  - Behavior: cold cache builds correct results.
    - Setup: a reindexed mirror tree, no `cache.db`.
    - Actions: run a search.
    - Expected: `cache.db` is created and results match a known-good ordering.
  - Behavior: incremental sync only touches changed files.
    - Setup: warm cache, then change one source file and reindex the mirror tree.
    - Actions: run search; observe which artifacts the sync parses.
    - Expected: only the changed artifact is re-parsed; results reflect the change.
  - Behavior: stale/deleted artifact is evicted from cache results.
    - Setup: warm cache, remove a source file + its artifact.
    - Actions: search.
    - Expected: no hits from the removed file.
- Before moving on: confirm tests, vet, and build pass.

## Stage 4: Retire pkb.db / LFS and update config + docs

- Goal: the repo no longer tracks a binary DB; hooks, gitignore, gitattributes,
  and docs reflect the mirror-tree model.
- Work:
  - Gitignore `.pkb/cache.db` (and any cache sidecar); remove the `pkb.db` LFS
    rule from `.gitattributes`; stop writing/committing `pkb.db`.
  - Update `hooks/pre-commit` to `git add` the `.pkb/index` tree (and
    `pkb-state.toml`) instead of `pkb.db`; drop the LFS caveats.
  - Migrate this repo's own index: run a full reindex to materialize `.pkb/index`,
    `git rm` the LFS `pkb.db`.
  - Update `README.md`, `context.md`, and `main.go` usage text (remove LFS
    setup, describe the mirror tree + local cache).
- Verification:
  - Behavior: a fresh clone can search with no committed DB.
    - Setup: clone-like checkout with `.pkb/index` present but no `cache.db`.
    - Actions: `pkb search`.
    - Expected: cache is built on demand; search works.
  - Behavior: pre-commit hook stages only small artifacts.
    - Setup: change one source file, run the hook.
    - Actions: inspect staged paths/sizes.
    - Expected: only the affected `.pkb/index` artifact(s) + state marker staged;
      no multi-MB blob.
- Before moving on: confirm the full suite, vet, build, and a manual
  reindex+search on this repo all pass.

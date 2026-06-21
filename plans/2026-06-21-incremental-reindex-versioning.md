# Objective and Context

## User request (verbatim)

> next up. Reindexing, and especially context augmentation, is very expensive for large repos. I want to make sure that partial completion is saved, so if we run reindex -> it fails -> we re-start it, progress is persisted and we don't waste money.
>
> As much as possible, we should only reindex any file *once*.
>
> For this, I'd like to add a "pkb version" to the hash we use to disambiguate the stored embedding. This should be added to the model specification. So we can tell that the embedding is compatible:
>
> - chunking algo, how handle breadcrumbs, treesitter parsers and scm / pkb logic
> - embedding model
> - embedding dimension (256 default)
>
> Things we should track as a "minor version". It's ok if these change as the embedding is still compatible
>
> - was context augmentation enabled?
> - context augmetnation inference model
> - context augmentation prompt
>
> Since context augmentation just participates in embedding as additional text, changing this text doesn't invalidate our existing embeddings.
>
> When we "reindex", we should make sure to keep partial progress persisted. So as soon as a chunk is embedded, we should store it in the db, so future embeddings/retries will not re-embed it again.
>
> Let's also make sure we store the augmentation text separately in the db. So we can see when augmentation for a chunk has happened, and we can debug / inspect it later if needed.
>
> Come up with a plan.

> ok, I also want to be cowardly about spending money. When we trigger a reindex, we should first estimate the cost of the reindexing operation - see how many files are affected, total up the changed characters, roughly chunk it, and estimate how many tokens / embedding calls it will be. Then project that into costs by using haiku/voyage embedding costs.
>
> pkb.toml gains a "budget" option (default $5). If the budget is exceeded by the estimate, we just throw an error. In this case the person will just have to manually reindex locally

## What we are building, in our own words

Reindexing a large repo is expensive, dominated by per-chunk LLM augmentation
(inference) calls and, secondarily, embedding calls. Today a failed run can
throw away a lot of paid work, and an augmentation-config change re-augments and
re-embeds whole files needlessly. We want:

1. A clear **compatibility identity** ("major version") for a stored embedding,
   so we know when a vector is still usable. It covers pkb chunking/breadcrumb
   logic + tree-sitter/scm, the embedding model, and the embedding dimension.
2. A **minor version** describing the augmentation spec (enabled?, inference
   model, prompt). It is recorded for inspection but does **not** invalidate
   embeddings: an embedding produced with a different augmentation blurb is still
   considered compatible and is never re-embedded just because the minor spec
   changed.
3. **Per-chunk persistence** so a crashed/aborted reindex keeps every chunk it
   already embedded and augmented; a retry re-pays for neither.
4. The **augmentation blurb stored separately** on each chunk so we can see
   whether/when a chunk was augmented and inspect the text.

## Key entities

- `internal/store/store.go` — sqlite + sqlite-vec wrapper. `files` table keyed
  by `(path, model_name, embedding_version)`, `chunks` table, one `vec0` table
  per `(model, version)`. Owns `EmbeddingVersion` (currently `3`), `PutFile`
  (atomic per-file delete+insert), `ChunkEmbeddings` (per-chunk reuse map),
  `ChunkKey(heading, text)` (deterministic reuse key), `Search`, `IndexedFiles`.
- `internal/index/manager.go` — the reindex flow: `Reindex`, `touchedPaths`,
  `indexFile`, `reuseEmbeddings`, `augmentPrompt`, `inferenceName`. Decides reuse
  vs re-index per file, drives chunking, augmentation, embedding, persistence,
  and the `pkb-state.toml` marker.
- `internal/embed` — `EmbeddingModel` whose `ModelName()` already encodes
  `model@dims` for every real provider, so the embedding-model + dimension half
  of the major identity is already present.
- `internal/infer` — `InferenceModel.ModelName()` and the augmentation
  `Complete()` call.

## Relevant facts established during research

- `EmbeddingVersion = 3` already isolates vec tables and is part of the `files`
  unique key and every reuse query — it is, in effect, today's "pkb major
  version". Bumping it discards old vectors.
- Every real embedding provider's `ModelName()` is `fmt.Sprintf("%s@%d", modelID,
  dims)`, so the embedding model and dimension are already folded into
  `model_name`. The major identity = `(EmbeddingVersion, model_name)` and is
  already what keys the vec table.
- `PutFile` is atomic per file (delete-all-then-insert in one tx). Because the
  state marker advances only on full success, file-level resume already works: a
  retry rescans and skips files whose `blob_sha` already matches. The gap is
  *intra-file*: a crash mid-file loses that file's in-progress work, and
  augmentation is never persisted in a reusable form at all.
- Today text files re-augment + re-embed all-or-nothing whenever the file or the
  inference-model identity changes (`prevEntry.inference == o.inferenceName()`
  short-circuit). Code files already reuse per-chunk via `ChunkEmbeddings`.

# Design

The unifying idea: make the **chunk** the unit of reuse and persistence for
*all* file types, key reuse on the **major identity + `ChunkKey`** only, store
the augmentation blurb on the chunk row, and make per-file writes **incremental
and generation-guarded** so a crash never destroys completed chunks.

## Major / minor version

- **Major identity** (invalidates embeddings): `(MajorVersion, model_name)`.
  - `MajorVersion` is the existing `EmbeddingVersion` constant, renamed and
    re-documented to make explicit that it covers chunking algorithm, breadcrumb
    handling, tree-sitter grammars, and tags.scm/pkb logic. Bump it whenever any
    of those change.
  - `model_name` already encodes embedding model + dimension (`model@dims`).
  - This pair already keys the vec table and the `files` row; no new column is
    needed for the major identity. The work is to *document* it as the
    compatibility hash and ensure nothing else leaks into it.
- **Minor spec** (recorded, never invalidates): a small serialized string
  `minorSpec = augEnabled | inferenceModel | promptVersion`. `promptVersion` is a
  short constant bumped by hand when `augmentPrompt` changes (a hash of the
  template is acceptable but a constant is simpler to reason about). Stored on
  the `files` row (`minor_spec`) and on each chunk (`aug_spec`) for inspection.
  It is deliberately **excluded** from reuse/compatibility decisions.

## Per-chunk reuse, generalized to all files

`ChunkEmbeddings` becomes the single reuse path for code *and* text. Because the
minor spec never invalidates an embedding, reuse keyed on `ChunkKey(heading,
rawText)` is valid for text too: an unchanged chunk keeps its vector and its
stored blurb even when the file changed elsewhere or the augmentation spec
changed. The reuse map carries both the embedding and the augmentation blurb.

Consequences:
- The per-file `inference == inferenceName()` short-circuit in `Reindex` is
  dropped. A file is skipped only when `complete = 1`, `blob_sha` matches, and
  the model matches. An augmentation-spec change alone no longer triggers any
  re-embedding (the cost-saving the user asked for).
- Augmentation (`o.Inference.Complete`) runs **only for chunks that miss reuse**
  (genuinely new/changed text). This is the dominant cost saving.

## Generation-guarded incremental persistence

To persist per chunk without ever showing duplicates in search and without
losing the last good copy on a crash, tag chunks with a per-file generation:

- `files`: add `complete INTEGER NOT NULL DEFAULT 1`, `indexed_gen INTEGER NOT
  NULL DEFAULT 0`, `minor_spec TEXT NOT NULL DEFAULT ''`.
- `chunks`: add `gen INTEGER NOT NULL DEFAULT 0`, `augmentation TEXT NOT NULL
  DEFAULT ''`, `aug_spec TEXT NOT NULL DEFAULT ''`.

`indexFile` becomes:

1. Load the reuse map from rows of the **current** generation
   (`c.gen = f.indexed_gen`): `ChunkKey -> { embedding, augmentation }`.
2. Clear any stale half-written rows from a prior crashed attempt
   (`DELETE FROM chunks WHERE file_id = ? AND gen <> indexed_gen`), set
   `complete = 0`, record target `blob_sha` and `minor_spec`. `newGen =
   indexed_gen + 1`.
3. For each chunk, in its own small transaction, insert the chunk row (with
   `gen = newGen`, plus `augmentation`/`aug_spec`) and its vec row:
   - reuse hit → carry the stored vector and blurb, no API calls;
   - miss → run augmentation if enabled (text only), then embed; persist
     immediately.
4. Finalize in one transaction: set `indexed_gen = newGen`, `complete = 1`, then
   `DELETE FROM chunks WHERE file_id = ? AND gen <> newGen` (drops the old
   generation).

`Search` and `ChunkEmbeddings` join with `c.gen = f.indexed_gen` so only the
completed generation is ever visible or reused. The new generation is invisible
until finalize.

Why generations rather than delete-up-front: the old generation stays intact
until the new one is fully committed, so a crash at any point leaves the last
good generation fully reusable — **no embedding and no augmentation is ever paid
for twice**. This is what makes "only reindex a file once" hold even across
crashes, and it removes the need for a separate augmentation cache table because
the blurb lives on (and is reused from) the chunk row.

Reclaiming space: dropping the old generation at finalize leaves free pages
behind, so the existing end-of-run `VACUUM` in `Reindex` must run *after* all
files have finalized (and thus after every old-generation drop). Keep the
`Vacuum` call at the tail of `Reindex`; do not move it earlier in the loop.

Embedding may still be issued as one batched `EmbedChunks` call for the miss set
(cheaper and fewer round-trips); the per-chunk transactions persist each result
as it is written. Batching the *writes* in small groups is a later performance
tuning, not a correctness concern.

Invariants:
- A vector is reused iff `(MajorVersion, model_name)` match and `ChunkKey`
  matches; the minor spec is never consulted for reuse.
- Search and reuse only ever observe rows with `gen = files.indexed_gen`.
- `complete = 1` ⟺ every chunk of the file's current generation is fully
  persisted; a `complete = 0` file is always reprocessed and reprocessing is
  idempotent and cheap (reuse hits).
- Bumping `MajorVersion` or changing `model_name` lands in a fresh vec table and
  forces full recompute; changing only the minor spec recomputes nothing.
- Augmentation `Complete` is called only for reuse-miss chunks of text files.

Edge cases:
- Duplicate `ChunkKey`s within a file collapse in the reuse map (identical
  deterministic input → identical vector), but each chunk still gets its own
  row, as today.
- A file that flips augmentation on/off keeps its existing vectors (minor
  change); newly embedded/changed chunks pick up the new spec. Mixed `aug_spec`
  within a file is allowed and visible for inspection.
- Migration: new columns are added with safe defaults via `ALTER TABLE`
  (mirroring the existing `heading_context`/`inference_model` migrations).
  Existing rows get `gen = 0 = indexed_gen`, `complete = 1`, empty
  `augmentation`. No re-embed of the existing index is required.

## Cost estimation + budget gate

Before doing any paid work, `Reindex` estimates the dollar cost of the run and aborts if it exceeds the configured budget. The estimate reuses the same `touchedPaths` + reuse-map machinery so it counts only work that will actually be paid for.

`Config` gains `Budget float64 toml:"budget"` (default `5.0`, USD). A non-positive budget disables the gate.

Estimation algorithm (no API calls; chunking is local and cheap):
- Resolve the touched set exactly as the real run does.
- For each touched file that is a candidate and not fully reused (not `complete=1` with matching blob/model), read its content and run the real chunker. Load its current-gen reuse map (`ChunkEmbeddings`) and split chunks into reuse hits vs misses by `ChunkKey`.
- Embedding cost: sum the characters of the contextualized text of embed-miss chunks, convert to tokens (`chars/charsPerToken`, the existing `charsPerToken = 3`), multiply by the embedding model's price-per-token.
- Augmentation (inference) cost: for each augment-miss chunk of a text file (only when inference is enabled), input tokens ≈ `(documentChars + chunkChars)/charsPerToken` (the prompt embeds the whole document), output tokens ≈ a small constant cap (`augmentMaxTokens`). Multiply by the inference model's input/output price-per-token. This term dominates for large documents.
- Sum embedding + inference; compare against `Budget`.

Pricing lives in a small table keyed by model id (e.g. `voyage-code-3`, `claude-haiku-4-5`) holding `$/1M` input (and, for inference, output) tokens, with a conservative fallback for unknown models. Keep it in one place (a new `internal/cost` helper, or constants beside the providers) so prices are easy to update; the major/minor versioning does not depend on it.

On overflow, `Reindex` returns an error before indexing anything, reporting the estimated cost, the budget, and the file/chunk/token counts, and telling the user to reindex locally. The estimate is printed (to stderr) even when under budget so a run's projected cost is visible.

Invariants:
- The estimate is computed from the same touched set and reuse maps as the real run, so it never under-counts already-reused work and never charges for reuse hits.
- Estimation performs no network/API calls and does not mutate the DB.
- A non-positive `Budget` skips the gate entirely.

# Stages

## Stage 1 — Major/minor versioning made explicit (no behavior change)

**Status: DONE.** `store.EmbeddingVersion` renamed to `store.MajorVersion`
(value unchanged at `3`) with a doc comment spelling out that it covers
chunking, breadcrumbs, tree-sitter grammars, and tags.scm/pkb logic, plus the
embedding model_name (`model@dims`). Added `promptVersion = "1"` constant and an
`Options.minorSpec()` helper in `internal/index/manager.go` serializing
`(augEnabled, inferenceModel, promptVersion)` as `off||` (disabled) or
`on|<model>|<promptVersion>`. Nothing in reuse consults the minor spec yet.
Test: `TestMinorSpec` in `internal/index/manager_test.go`. Full suite, vet, and
build pass.

- Goal: `EmbeddingVersion` is renamed/documented as `MajorVersion` covering
  chunking + tree-sitter/scm + embedding model + dimension; a `minorSpec` helper
  in `manager.go` serializes `(augEnabled, inferenceModel, promptVersion)` with a
  `promptVersion` constant. Nothing in reuse consults the minor spec yet.
- Verification:
  - Behavior: `minorSpec` reflects enabled/model/prompt; major identity unchanged
    for an unchanged config.
  - Setup: unit test in `internal/index` with mock embed/infer models.
  - Actions: build options with and without inference; compute the spec string.
  - Expected outcome: deterministic, distinct strings; disabled augmentation
    yields the empty-augmentation form.
- Before moving on: confirm tests, `go vet ./...`, and build all pass.

## Stage 2 — Schema + storage primitives for generations and augmentation

**Status: DONE.** `store` gained the new columns (`files.complete`,
`files.indexed_gen`, `files.minor_spec`, `chunks.gen`, `chunks.augmentation`,
`chunks.aug_spec`) via idempotent `ALTER TABLE` migrations (table-driven
`addColumn` loop). New incremental API: `StartFile` (ensures file row, records
blob/minor spec, marks incomplete, clears any prior crashed attempt's
non-committed-gen chunks, returns `indexed_gen+1`), `InsertChunk` (persists one
chunk + vec row under a gen in its own transaction, storing
`augmentation`/`aug_spec`), and `FinalizeFile` (advances `indexed_gen`, sets
`complete=1`, drops superseded generations via `deleteChunksOtherGenTx`).
`Search`, `Stats`, and `ChunkEmbeddings` filter `c.gen = f.indexed_gen` so only
the committed generation is visible/reused. `ChunkEmbeddings` returns
`map[string]ReuseChunk` (embedding + stored blurb); `FileMeta` exposes
`Complete`/`MinorSpec`. Decisions: kept `PutFile` unchanged (defaults keep it
valid; Stage 3 replaces its usage). Tests in `internal/store/store_test.go`
(`TestGenerationLifecycle`, `TestPartialGenerationInvisible`,
`TestStartFileClearsCrashedAttempt`,
`TestIndexedFilesExposesCompleteAndMinorSpec`, `TestMigrationFromOldSchema`).
Full suite, vet, build pass.

- Goal: `store` gains the new columns (`files.complete`, `files.indexed_gen`,
  `files.minor_spec`, `chunks.gen`, `chunks.augmentation`, `chunks.aug_spec`)
  with `ALTER TABLE` migrations, plus the incremental API: start-file (clear
  stale gen, mark incomplete), insert-one-chunk (with gen + augmentation),
  finalize-file (advance `indexed_gen`, set complete, drop old gen). `Search` and
  `ChunkEmbeddings` filter `c.gen = f.indexed_gen`; `ChunkEmbeddings`/reuse map
  also returns the stored blurb. `IndexedFiles`/`FileMeta` expose `complete`.
- Verification:
  - Behavior: a finalized file is searchable and reusable; an unfinalized file's
    new-gen rows are invisible to search and reuse; finalize drops the old gen.
  - Setup: store-level unit tests with a mock embedding (small dims), inserting
    two generations for one path.
  - Actions: write gen N, assert visible; start gen N+1 partially, assert search
    still returns gen N; finalize, assert only gen N+1 visible and old rows gone.
  - Expected outcome: counts and returned chunk IDs match the expected
    generation at each step; reuse map keys on `ChunkKey` and carries blurbs.
  - Behavior: migration on a pre-existing DB.
  - Setup: open an old-schema DB fixture (or a DB built without the new columns).
  - Actions: `Open`.
  - Expected outcome: columns added, existing rows default to `gen=0`,
    `indexed_gen=0`, `complete=1`, and remain searchable.
- Before moving on: confirm tests, vet, build pass.

## Stage 3 — `indexFile` drives incremental, generation-guarded persistence

**Status: DONE.** `indexFile` now loads the committed-generation reuse map
(`ChunkEmbeddings`, keyed on `ChunkKey`) for *all* file types, resolves per chunk
whether it is a reuse hit (carrying the stored embedding + augmentation blurb +
`aug_spec`), runs augmentation (`Inference.Complete`) only for reuse-miss chunks
of text files, builds the embedded text via a new `contextualize` helper (heading
context inner, augmentation blurb outermost), embeds only misses in one batched
call, then persists incrementally: `StartFile` (fresh generation) →
`InsertChunk` per chunk (in its own tx, storing `augmentation`/`aug_spec`) →
`FinalizeFile` (atomic generation swap, drops the superseded generation). The old
all-or-nothing `reuseEmbeddings` helper was deleted. Decision: `store.ReuseChunk`
gained an `AugSpec` field (and `ChunkEmbeddings` now selects `c.aug_spec`) so
reuse hits carry the original spec that produced their blurb. Crash resilience is
provided by the committed generation: a crash mid-file leaves the previous
generation intact, and the retry reuses every unchanged chunk by `ChunkKey`.
Tests: `TestTextFilePerChunkReuseOnChange`,
`TestTextFileInferenceIdentityChangeReusesVectors` (replaces the old
whole-file/identity re-embed tests), and
`TestCrashMidFileReusesCommittedGeneration`. The Reindex skip short-circuit
(`prevEntry.inference == inferenceName()`) is intentionally left for Stage 4.
Full suite, vet, build pass.

## Stage 3 — `indexFile` drives incremental, generation-guarded persistence

- Goal: `indexFile` uses the Stage-2 API: load current-gen reuse map, write each
  chunk under `newGen` in its own transaction, run augmentation only on reuse
  misses (text only), store the blurb on the chunk, finalize at the end.
  `reuseEmbeddings` is generalized to all file types and folded into this loop.
- Verification:
  - Behavior: reuse hits skip both embedding and inference.
  - Setup: index a text file with a mock inference counting calls; reindex with
    one chunk's text changed.
  - Actions: run reindex twice.
  - Expected outcome: second run calls inference only for the changed chunk;
    unchanged chunks keep their original vector and blurb.
  - Behavior: crash resilience.
  - Setup: index a multi-chunk file, then simulate failure after some chunks via
    an injected error (mock embed/infer that errors on the Nth chunk); rerun.
  - Actions: first run aborts mid-file; second run completes.
  - Expected outcome: the second run does not re-embed or re-augment the chunks
    persisted by the first run (assert via call counts); final index is correct
    and contains exactly one generation.
- Before moving on: confirm tests, vet, build pass.

## Stage 4 — Drop the augmentation short-circuit; minor changes don't re-embed

**Status: DONE.** `Reindex`'s skip decision now keys on `complete=1` + blob +
model match only (`wasIndexed && prevEntry.complete && prevEntry.model == ... &&
prevEntry.sha == blobSha`); the `prevEntry.inference == o.inferenceName()`
requirement and the now-dead `Options.inferenceName()` helper and
`indexedEntry.inference` field were removed. `indexedEntry` gained `complete`,
populated from `FileMeta.Complete`. `minor_spec` is already recorded on write via
`StartFile` (Stage 3). The existing tail `VACUUM` runs after every file
finalizes (after old-gen drops) and was left in place. An augmentation-spec-only
change (different inference model/prompt) now re-embeds and re-augments nothing:
reuse-by-`ChunkKey` carries the original vector, blurb, and `aug_spec`. Test
`TestTextFileInferenceIdentityChangeReusesVectors` was strengthened to assert
zero embedding and zero inference calls on the inference-model switch. Full
suite, vet, build pass.

## Stage 4 — Drop the augmentation short-circuit; minor changes don't re-embed

- Goal: remove the `prevEntry.inference == inferenceName()` requirement from
  `Reindex`'s skip decision (skip on `complete=1` + blob + model match only).
  Record `minor_spec` on write for inspection. Confirm an augmentation-spec
  change alone triggers no embedding/inference work.
- Verification:
  - Behavior: changing only the inference model/prompt re-embeds nothing.
  - Setup: index a repo, then reindex with a different mock inference model.
  - Actions: run reindex twice.
  - Expected outcome: zero embedding and inference calls on the second run;
    vectors unchanged; `minor_spec`/`aug_spec` reflect the original run.
- Before moving on: confirm tests, vet, build, and the full `go test ./...`
  suite pass.
  - Behavior: space is reclaimed after old generations are dropped.
  - Setup: index a repo, then reindex with changed content so old-gen chunks are
    dropped.
  - Actions: run reindex to completion.
  - Expected outcome: `VACUUM` runs at the tail of `Reindex` after every file has
    finalized (after old-gen drops); no orphaned vec/chunk rows remain.

## Stage 5 — Cost estimation + budget gate

**Status: DONE.** `Config.Budget float64 toml:"budget"` (default `5.0`) was
added with a non-positive value disabling the gate; it threads through
`index.Options.Budget` (wired in `cmd/pkb/main.go`). A new `internal/cost`
package holds the pricing table (`$/1M`-token embedding + inference rates,
matched as substrings of the model name so `voyage-code-3@256` and
`us.cohere.embed-v4:0` resolve, with conservative fallbacks) plus `CharsPerToken`
and `AugmentMaxTokens` constants and `EmbeddingPricePerToken`/
`InferencePricePerToken`. The chunking logic in `indexFile` was extracted into a
shared `Options.chunkFile` helper so the estimator chunks identically. New
`Options.estimate` mirrors `Reindex`'s skip decision and per-chunk
`ChunkEmbeddings` reuse: skipped/reused chunks are never charged; embedding
tokens come from the (un-augmented) contextualized text of miss chunks, and
inference cost is charged only for augment-miss chunks of text files
(`(documentChars+chunkChars)/charsPerToken` input, `AugmentMaxTokens` output). It
performs no API calls and no DB writes. `Reindex` runs the estimate right after
`touchedPaths`, prints the projected cost to stderr on every run, and returns a
budget error before any paid work when `Budget > 0 && est > Budget`. The tail
`VACUUM` was left in place (runs after all finalizes). Decisions: estimate uses
un-augmented contextual length for embed tokens (aug blurb length is unknown
pre-run; the inference term dominates); pricing matched by substring for
robustness to `@dims`/profile prefixes. Tests: `internal/cost/cost_test.go`
(`TestEmbeddingPricePerToken`, `TestInferencePricePerToken`) and
`internal/index/manager_test.go` (`TestBudgetGateAbortsOverBudget` — over-budget
aborts with zero embed/infer calls and an unchanged index;
`TestBudgetGateDoesNotChargeReuse` — a forced full revisit of an unchanged repo
under a tiny budget proceeds because reuse hits cost $0). README config reference
documents `budget`. Full suite, vet, build pass.

## Stage 5 — Cost estimation + budget gate

- Goal: `Config.Budget` (default `$5`) is parsed; `Reindex` estimates the run's cost from the touched set + reuse maps and aborts before any paid work when the estimate exceeds the budget. The projected cost is logged on every run.
- Verification:
  - Behavior: an over-budget estimate aborts with no API calls and no DB mutation.
  - Setup: a fixture repo with enough changed content to exceed a tiny budget; mock embed/infer models that count calls.
  - Actions: run `Reindex` with `Budget` set below the estimate.
  - Expected outcome: returns a budget error mentioning estimate vs budget; zero embedding/inference calls; index unchanged.
  - Behavior: reuse hits are not charged.
  - Setup: index a repo fully, then reindex unchanged.
  - Actions: run `Reindex` again.
  - Expected outcome: estimated cost is ~zero; run proceeds.
  - Behavior: estimate accuracy for a known mix.
  - Setup: a small repo with one code file and one text file, known sizes, with pricing constants.
  - Actions: compute the estimate.
  - Expected outcome: matches a hand-computed value within rounding.
- Before moving on: confirm tests, vet, build, and the full `go test ./...` suite pass.

# Out of scope / follow-ups

- Optional explicit `reindex --refresh-augmentation` to recompute blurbs when the
  minor spec changes (default remains: keep existing vectors).
- Batching the per-chunk write transactions for throughput.
- GC of orphaned generations is handled inline at finalize; a periodic sweep is
  unnecessary given the invariant.

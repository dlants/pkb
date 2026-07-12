# Objective and Context

## User request (verbatim)

> I think I actually just want to dramatically simplify this plugin - let's drop all the context augmentation stuff (config, docs, etc...) We will always run with contextualizeText for text, and use the current AST based embedding model with the same embedding model for code. Keep the embedding compatibility with other providers, but drop all the inference stuff

> I think we should only support contextualized embedders for text - so in effect, we're currently requiring voyage. Let's drop the remaining providers too. Maybe in the future other providers will add this functionality, but we can just add it back when needed.

## What we're building and why

Two coupled simplifications:

1. **Drop LLM augmentation / inference entirely.** The optional inference pass
   (the contextual-retrieval "blurb" prepended to text chunks) and its whole
   subsystem — `internal/infer`, the `[inference]` config, `MaxParallelism`,
   per-chunk `augmentation`/`aug_spec` storage, the `minorSpec` reuse dimension,
   and inference cost pricing — are removed.

2. **Voyage-only embeddings.** Only providers that support contextualized
   whole-document embeddings are supported for text, which today means **Voyage**
   (`voyage-context-*`). The other embedding providers (`bedrock`/Cohere,
   `openai`/`openai-compatible`, `gemini`) are removed. A deterministic `mock`
   model is retained for tests. Support can be re-added later if another provider
   ships contextual embeddings.

Retained behavior:

- **Text files**: always embedded via Voyage's contextualized whole-document
  endpoint (`EmbedDocument`). No config toggle — it is the only text path.
- **Code files**: AST chunking + breadcrumb heading context, embedded with the
  same Voyage model via `EmbedChunks` (single-chunk document groups). Unchanged.
- **Requirement**: the configured embedding model must implement
  `embed.ContextualEmbeddingModel`; `setup()` fails fast otherwise (a
  non-contextual model cannot handle text). `mock` and `voyage-context-*`
  qualify.

## Key entities

- `internal/infer/*` — **deleted entirely.**
- `internal/embed` — keep `voyage.go`, `mock.go`, `embed.go`
  (`EmbeddingModel` + `ContextualEmbeddingModel` interfaces). **Delete**
  `bedrock.go`, `openai.go`, `gemini.go` (+ their tests). `factory.go` `Build`
  collapses to voyage/mock (or is inlined); the AWS SDK deps drop from `go.mod`.
- `index.Options` — drop `Inference`, `MaxParallelism`, `ContextualizeText`.
- `index` routing — `isContextual(path)` = `route(path) == Text` (the model is
  always contextual by the setup-time guarantee). `contextualModel()` collapses
  to the model asserted to `ContextualEmbeddingModel`. Augmentation loop,
  `augmentPrompt`, `Contextualize`'s blurb portion, `stripThinking`,
  `promptVersion`, and `minorSpec`'s inference bits are removed.
- `minorSpec` / `autoChunkMinorSpec` mode-flip machinery — dead once the model is
  always contextual (for a fixed model name, `isContextual(path)` is constant and
  a model swap already forces a full re-embed via blob-diffing). Removed from
  `touchedPaths` and the store; `store.MajorVersion` is bumped to force one clean
  re-key.
- `config.Config` / `ModelConfig` — drop `Inference`, `ContextualizeText`,
  `Region`, `Profile` (Bedrock-only). Keep `Provider`, `Model`, `Dimensions`,
  `BaseURL`, `APIKeyEnv` (Voyage uses base URL + `VOYAGE_API_KEY`). Default
  provider stays `voyage`.
- `internal/cost` — keep Voyage embedding prices; drop Cohere/OpenAI embedding
  prices and all inference pricing (`inferencePricesPerM`,
  `InferencePricePerToken`, `AugmentMaxTokens`).
- `internal/store/store.go` — drop `augmentation`/`aug_spec` chunk columns and
  the `minor_spec` file column (+ the `MinorSpec`/`Augmentation`/`AugSpec` fields
  threaded through `IndexedFiles`/`PutFile`/`ChunkEmbeddings`); bump
  `MajorVersion`.
- `main.go` — build only the embedding model, assert contextual capability, drop
  inference wiring + inference cost output.
- `README.md` — rewrite provider/config docs to Voyage-only, no
  augmentation/inference. `LOCAL.md` (local non-Voyage setup guide) — **delete**.
- `pkb.toml` — drop `[inference]` and `contextualizeText`.

## Relevant files

- delete: `internal/infer/`, `internal/embed/{bedrock,openai,gemini}.go` (+tests),
  `internal/index/augment_bench_test.go`, `LOCAL.md`.
- edit: `internal/embed/factory.go`, `internal/index/manager.go` (+test),
  `internal/config/config.go` (+test), `internal/cost/cost.go` (+test),
  `internal/store/store.go` (+test), `internal/smoke/e2e_test.go`, `main.go`,
  `README.md`, `pkb.toml`, `go.mod`/`go.sum`.

# Design

## Routing after the change

`route(path)` (Code vs Text) is unchanged. Then:

- `isContextual(path)` = `route(path) == Text`.
- Text → `prepareContextualFile` → `EmbedDocument` (Voyage contextualized).
- Code → `prepareFile` → AST chunks → `EmbedChunks` with contextualized text =
  raw chunk + breadcrumb heading context only (no blurb). No inference, no
  `MaxParallelism`.

The setup-time guarantee (model implements `ContextualEmbeddingModel`) lets the
indexer treat the contextual model as always present, removing the
`(model, ok)` capability-probe branches.

## Reuse identity simplification

Reuse rides on `(path, model, blobSha)` blob-diffing (already in place) plus, on
the code path, `ChunkKey(headingContext, text)`. With augmentation gone the
embedded text is a pure function of `(headingContext, text)`, so `aug_spec` /
`augmentation` / `minor_spec` are unnecessary. Bumping `store.MajorVersion`
re-keys the vec tables so no stale augmented vectors survive the upgrade.

## Cost

`estimate()` keeps only embedding tokens (whole-file for text, per-chunk for
code); all inference accounting and the `CostEstimate` inference fields are
removed. The `MaxReindexCost` gate stays (embedding-only). Cost table keeps only
Voyage models plus the conservative unknown-model fallback.

## Alternatives considered

- **Keep a non-contextual per-chunk text fallback** (previous plan revision):
  rejected per the user — we now require a contextual embedder and drop the other
  providers rather than maintain a degraded text path.
- **Keep `minor_spec`/`aug_spec` as always-empty cruft**: rejected — a
  `MajorVersion` bump makes clean schema removal safe.

## Invariants

- `setup()` must reject a non-contextual embedding model with a clear error
  before any indexing.
- A re-index after upgrade must produce vectors identical to a from-scratch index
  (guaranteed by the `MajorVersion` bump forcing full recompute).
- Code-file indexing behavior is unchanged (AST chunks + breadcrumbs).
- Text-file behavior equals today's `contextualizeText = true` on Voyage.
- No remaining references to `internal/infer`, `Inference`,
  `ContextualizeText`, `MaxParallelism`, augmentation, inference pricing, or the
  removed embedding providers / AWS SDK (`go build`/`go vet` clean;
  `rg` for those symbols is empty).

# Stages

## Stage 1 — Remove augmentation from the indexer core

> **Status: DONE.** `internal/index` no longer performs any inference. Text
> always routes to the contextual (`EmbedDocument`) path; code to the per-chunk
> AST path with no blurb. `Options` lost `Inference`/`MaxParallelism`/
> `ContextualizeText`; `contextualModel()` now just asserts the model to
> `ContextualEmbeddingModel` (no toggle) and `isContextual` = text file. The
> store API is unchanged this stage: `minorSpec()` is the constant `"off||"` and
> empty augmentation/aug-spec slices are still threaded through. Removed
> `augmentPrompt`/`stripThinking`/`augmentNoneSentinel`/`promptVersion` and the
> augmentation loop. `main.go`/`internal/smoke` had their `Inference` wiring
> trimmed just enough to compile (full `internal/infer` removal is Stage 2).
> Tests: deleted `augment_bench_test.go` and the `TestAugmentation*`,
> `TestTextFileInferenceIdentityChangeReusesVectors`,
> `TestTextFilePerChunkReuseOnChange` (per-chunk text reuse no longer exists),
> `TestReindexOnHeadingChangeMarkdown` (breadcrumb reuse is code-only now),
> `TestContextualizeTextOffIsNoOp`, `TestContextualizeTextModeFlipReembedsTextOnly`,
> `TestEstimateContextualTextDropsInferenceKeepsEmbedding`, and `TestStripThinking`.
> Retargeted the `TestContextualizeText*` survivors to always-on and switched
> text-file chunk-count assertions to `DocumentCalls`. Converted the crash-safety
> and partial-run tests to a `.go` fixture (per-chunk reuse is code-only). Full
> `go build`/`go vet`/`go test ./...` pass.

- Goal: `internal/index` indexes with no inference. Text always routes to the
  contextual path; code to the per-chunk AST path (no blurb). `Options` loses
  `Inference`/`MaxParallelism`/`ContextualizeText`; `isContextual` = text.
  Temporarily keep the `store` API by passing empty augmentation slices / a
  constant `minorSpec` so this stage lands independently.
- Verification (unit, contextual mock model):
  - Behavior: text routes through `EmbedDocument`. Setup: mock contextual model.
    Actions: reindex a `.md`. Expected: document path used; one file indexed.
  - Behavior: code embeds per-chunk with breadcrumbs, no blurb. Setup: `.go`
    file. Expected: contextualized text = heading + raw text only.
  - Remove `TestAugmentation*`, `augment_bench_test.go`; retarget
    `TestContextualizeText*` to always-on (drop "off" variants).
- Before moving on: `go build ./...`, `go vet ./...`, `go test ./internal/index/...` pass.

## Stage 2 — Delete `internal/infer` and its wiring

> **Status: DONE.** Deleted `internal/infer/` entirely. `main.go`'s
> `printEstimate` no longer prints inference input/output tokens (embedding
> only); the `CostEstimate` inference fields remain for Stage 4. `internal/smoke`
> dropped the `infer` import and the `infer.Build` rejection assertion. `cost.go`
> still contains inference pricing/comments — that removal is Stage 4. Full
> `go build`/`go vet`/`go test ./...` pass; `rg -l internal/infer` is empty.

- Goal: `internal/infer` gone; `main.go` builds no inference model and prints no
  inference cost; `internal/smoke` drops the import.
- Verification: `pkb reindex`/`estimate` run end-to-end with a mock embedder;
  `rg -l internal/infer` is empty.
- Before moving on: full `go build`/`go vet`/`go test ./...` pass.

## Stage 3 — Voyage-only embeddings + contextual requirement

> **Status: DONE.** Deleted `internal/embed/{bedrock,openai,gemini}.go` and
> their tests. `Build` collapsed to `voyage`/`mock` (empty provider defaults to
> voyage) and its signature dropped the Bedrock-only `region`/`profile` args
> (config still carries those fields until Stage 4; they are simply no longer
> passed). `setup()` in `main.go` now asserts the built model implements
> `embed.ContextualEmbeddingModel` and fails fast with a clear error otherwise.
> AWS SDK deps removed from `go.mod`/`go.sum` via `go mod tidy`. Updated the
> `internal/smoke` `embed.Build` calls to the new signature and added
> `internal/embed/factory_test.go` covering that every provider is contextual and
> that unknown providers error. Full `go build`/`go vet`/`go test ./...` pass;
> `rg -in "bedrock|cohere|gemini|openai|aws" internal/embed go.mod` is empty.

- Goal: delete `internal/embed/{bedrock,openai,gemini}.go` (+tests); collapse
  `Build` to voyage/mock; `setup()` asserts the model implements
  `ContextualEmbeddingModel` and errors otherwise; drop AWS SDK from
  `go.mod`/`go.sum` (`go mod tidy`).
- Verification:
  - Behavior: a `voyage` config builds and indexes (mock-substituted in tests).
  - Behavior: a hypothetical non-contextual model errors at setup. Setup: a
    non-contextual test model. Actions: `setup()`/build. Expected: clear error.
  - `rg -in "bedrock|cohere|gemini|openai" internal/embed` is empty; build has no
    AWS imports.
- Before moving on: full `go build`/`go vet`/`go test ./...` pass.

## Stage 4 — Drop inference + non-Voyage config and cost

> **Status: DONE.** `ModelConfig` dropped `Region`/`Profile`/`ContextualizeText`;
> `Config` dropped the `Inference` block and `MaxParallelism`. Default embedding
> model is now `voyage-context-3` (the old `voyage-code-3` default is not
> contextual and would fail the Stage-3 setup guard). `internal/cost` dropped
> `AugmentMaxTokens`, the `Pricing` type, `inferencePricesPerM`,
> `fallbackInferencePerM`, `InferencePricePerToken`, and all non-Voyage embedding
> prices (Cohere/OpenAI) — leaving only `voyage-context-4`/`voyage-context-3`
> plus the unknown-model fallback. `CostEstimate`/`costEstimate` lost their
> inference/`EmbedDollars`/`InferDollars` fields and the reindex-cost log/error
> lines dropped the inference-token columns. Config/cost tests retargeted to
> Voyage-only, added `TestStrayInferenceKeysAreTolerated` covering that stray
> `[inference]`/`contextualizeText` keys are ignored. Note: `minor_spec`/
> `autoChunkMinorSpec` store machinery remains (Stage 5). Full
> `go build`/`go vet`/`go test ./...` pass.

- Goal: `ModelConfig` loses `Inference`, `ContextualizeText`, `Region`,
  `Profile`; `internal/cost` keeps only Voyage embedding prices and drops all
  inference pricing / `AugmentMaxTokens`; `CostEstimate` inference fields removed.
- Verification:
  - Behavior: loading a `pkb.toml` with stray `[inference]`/`contextualizeText`
    keys is tolerated (unknown keys ignored); defaults have no inference.
  - `cost_test.go` keeps Voyage-price coverage; inference/Cohere/OpenAI tests
    removed.
- Before moving on: full `go test ./...`, `go vet ./...` pass.

## Stage 5 — Simplify the store schema

> **Status: DONE.** Dropped the `augmentation`/`aug_spec` chunk columns and the
> `minor_spec` file column from the store schema + migrations; bumped
> `store.MajorVersion` 4 → 5 to re-key the vec tables. `FileMeta` lost
> `MinorSpec`; `PutFile` dropped its `minorSpec`/`augmentations`/`augSpecs`
> params; `ChunkEmbeddings` now returns `map[string]embed.Embedding` (the
> `ReuseChunk` struct is gone). In `internal/index`: removed `Options.minorSpec`,
> `autoChunkMinorSpec`, the `indexedEntry.minorSpec` field, and all
> auto-chunk-mode-flip logic in the reindex skip check, `touchedPaths`, and
> `estimate` (for a fixed model name `isContextual(path)` is constant, so stored
> mode is unnecessary). `preparedFile` dropped `minorSpec`/`augmentations`/
> `augSpecs`; `prepareFile`/`prepareContextualFile`/`compactPrepared`/`Contextualize`/
> `writeFile` simplified accordingly. `main.go`'s `chunk` preview call dropped the
> augmentation arg. Tests: rewrote `store_test.go` helpers/asserts for the new
> signatures (removed `TestIndexedFilesExposesMinorSpec`, bumped the migration
> seed to version 5), removed `TestMinorSpec`, and updated
> `TestCompactPreparedDropsZeroSignalChunks`. Full `go build`/`go vet`/`go test ./...`
> pass. Note: `README.md`/`pkb.toml`/`LOCAL.md` doc cleanup remains (Stage 6).

- Goal: drop `augmentation`, `aug_spec`, `minor_spec` columns + struct fields +
  query columns; bump `store.MajorVersion`; simplify
  `PutFile`/`IndexedFiles`/`ChunkEmbeddings` signatures and `internal/index`
  callers.
- Verification:
  - Behavior: from-scratch reindex then no-op reindex embeds nothing (reuse on
    `ChunkKey(headingContext, text)`).
  - Behavior: an edit re-embeds only changed chunks.
  - `store_test.go` updated for new signatures/schema.
- Before moving on: full `go test ./...`, `go vet ./...` pass.

## Stage 6 — Docs + config cleanup

- Goal: `README.md` describes only Voyage embedding (no augmentation, inference,
  `contextualizeText`, `maxparallelism`, other providers, AWS fields); delete
  `LOCAL.md`; `pkb.toml` drops `[inference]` and `contextualizeText`.
- Verification: `rg -in "augment|inference|contextualizeText|maxparallelism|bedrock|gemini|openai"`
  in docs/config returns nothing meaningful; `pkb reindex`/`estimate` still run.
- Before moving on: full `go test ./...`, `go vet ./...` pass.

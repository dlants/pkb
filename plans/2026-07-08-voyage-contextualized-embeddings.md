# Objective and Context

## User request (verbatim)

> so I think voyager ai recently released a model with late chunking. This allows you to send in the whole file, and it will automatically chunk it for you using an algorithm that looks at transitions in meaning space.
>
> Look into the docs for that.
>
> I'm thining about adding an option to use late chunking instead of context-augmented retrieval for text files, but I do want to still use the same embedding model for markdown and code. So, see if that model supports code + text, or just text.
>
> ok, write a plan

## Findings that shape the plan

- The model in question is Voyage's **contextualized chunk embedding** line: `voyage-context-3` (July 2025) and its successor `voyage-context-4` (June 2026). Voyage calls this "contextualized chunk embeddings" and explicitly positions it *against* Jina-style "late chunking" as a competing technique.
- The mechanism matches the user's description: the model sees all chunks of a document at once and injects global document context into each chunk's vector, replacing LLM-based context augmentation (exactly what our `Inference` augmentation step does today).
- `voyage-context-4` **supports code** — it is benchmarked across 8 domains including `code`, and doubles as a plain single-embedding model. So one model can serve both markdown/text and code, which is the user's stated requirement.
- Auto-chunking ("send the whole file, model chunks it") only exists in `voyage-context-4` via `enable_auto_chunking`, and requires the `/v1/contextualizedembeddings` endpoint. It is NOT in `voyage-context-3`.

## What we're building

Add an **option** so that, for **text** files, PKB stops running per-chunk LLM inference augmentation and its own text chunking, and instead sends the (near-)whole file to Voyage's contextualized-chunk endpoint with **auto-chunking on** (`enable_auto_chunk=true`). Voyage picks the chunk boundaries and returns one context-aware vector per model-chosen chunk. **Code files are unchanged**: they keep PKB's AST/tree-sitter chunking, heading breadcrumbs, and line ranges, and are embedded in ordinary isolated mode. The same model id (`voyage-context-4`) and output dimension are used for all files, so code and text vectors share **one vec table / embedding space**.

Two paths coexist, both in scope:

1. **Code — isolated + breadcrumbs (unchanged).** AST chunks with symbol-path breadcrumbs baked into the chunk text, embedded via the existing `/v1/embeddings` isolated call, flowing through the current cross-file `batchTexts` char-batching, with per-chunk `ChunkKey` reuse and precise line ranges intact. Breadcrumbs already supply global *positional* context (the chunk's address in the AST) in-band, so isolated code vectors remain comparable to contextualized text vectors in the shared space.
2. **Text — Voyage auto-chunking (in scope).** Estimate tokens via `chars/5`. If under ~120K, send the whole file to `/v1/contextualizedembeddings` with `enable_auto_chunk=true` in **one request per file**. If over ~120K, naive-split the file into sequential ~120K-token **windows with a 10K-token overlap**, one request per window. Voyage returns model-chosen chunks; we store each chunk's returned text keyed to its **source file** (file-tagged locator — no `start_line`/`end_line` for now). Inference augmentation is skipped for these files.

### Why the two regimes stay compatible

Embedding compatibility is a function of model + output dimension, not of how the input was chunked. Because `voyage-context-4` is designed to double as a standard embedder comparable to its own contextualized outputs, isolated code vectors and auto-chunked text vectors land in the same space and a single query vector scores sensibly against both. The residual difference is second-order: breadcrumbs give a code chunk its *address* (symbolic, in-band), while contextualization gives a text chunk its *neighborhood* (semantic, in-vector). This is a mild ranking effect, not an incompatibility, so **one shared vec table is correct** — no `+ctx` model-name suffix and no `MajorVersion` bump needed.

### Line ranges for text (deferred)

Auto-chunk responses return each chunk's **text** but not its character/line offsets. Reverse-mapping spans by locating the returned substring in the source is fragile, so for now text chunks carry only the **file name** as their locator; search hits cite the file, not a line range. Precise line ranges for auto-chunked text are a follow-up.

## Key types / entities involved

- `embed.EmbeddingModel` (`internal/embed/embed.go:6-17`) — the flat provider interface: `ModelName`, `Dimensions`, `EmbedChunk`, `EmbedQuery`, `EmbedChunks([]string) ([]Embedding, error)`. This is chunk-agnostic: one input string -> one vector, no grouping.
- `embed.Voyage` (`internal/embed/voyage.go`) — existing Voyage `/v1/embeddings` provider. We extend Voyage (or add a sibling) to also call `/v1/contextualizedembeddings` with document grouping.
- `embed.Build` (`internal/embed/factory.go`) — provider dispatch; already knows `"voyage"`.
- `config.ModelConfig` / `config.Config` (`internal/config/config.go`) — `[embedding]` / `[inference]` blocks, `Default()`, `Load()`.
- `index.Options` + `Reindex` (`internal/index/manager.go`) — the indexing pipeline: `route`, `chunkFile`, `prepareFile`, `compactPrepared`, cross-file char-batching, `writeFile`. `minorSpec()` (line 108-117) and `Contextualize()` (825-838) matter here.
- `store.PutFile` / `ChunkKey` / `ChunkEmbeddings` (`internal/store/store.go`) — transactional write and per-chunk reuse; `MajorVersion` const (store.go:30, currently `4`).
- `cost` package (`internal/cost/cost.go`) — pricing table; already has `embed-v4` at 0.12 but not `voyage-context`.

# Design

## The core friction: flat interface vs. whole-file auto-chunking

Everything in the pipeline today assumes **isolated** embedding of **our** chunks: a chunk's vector depends only on that chunk's contextualized text, and the manager already knows the chunk list (with line ranges) before embedding. Auto-chunking breaks three of those assumptions for text files:

1. **Interface shape.** `EmbedChunks([]string)` is a flat batch of *known* chunks, one vector out per string in. The auto-chunk endpoint instead takes a whole document and returns a *model-decided* number of chunks, each with its own text and vector. The caller does not know the chunk count or boundaries in advance.
2. **Cross-file char-batching.** `Reindex` accumulates reuse-miss chunks from *many files* into one flat `batchTexts` slice and calls `EmbedChunks` once. Auto-chunk text files must NOT join this batch — each file (or 120K window) is its own request so the model chunks a coherent document.
3. **Chunk provenance.** Today a chunk carries `start_line`/`end_line`/heading breadcrumb from our chunker. Auto-chunk gives back only text, so text chunks are **file-tagged** (no line range) and per-chunk `ChunkKey` reuse cannot apply.

## Chosen approach

Introduce an **optional capability interface** rather than widening `EmbeddingModel` for every provider:

```
// ContextualEmbeddingModel is implemented by providers that can auto-chunk and
// embed a whole document, returning the model-chosen chunks with their vectors.
type ContextualEmbeddingModel interface {
    EmbeddingModel
    EmbedDocument(document string) ([]ContextualChunk, error)
}

type ContextualChunk struct {
    Text      string
    Embedding Embedding
}
```

`embed.Voyage` gains `EmbedDocument`, which POSTs the document to `/v1/contextualizedembeddings` with `enable_auto_chunk: true`, `input_type: "document"`, `output_dimension: dims`, and returns the model's chunk texts paired with their vectors (validating each vector's dims exactly as `embed()` does). Mock models implement it deterministically for tests.

The manager decides, per file, which path to use:

- A file uses the **auto-chunk text path** iff **config enables it AND the file routes to text AND the model implements `ContextualEmbeddingModel`**. Everything else — all code, and text when the option is off — uses the existing isolated path unchanged.
- **Code (isolated path):** unchanged. AST chunks + breadcrumbs + line ranges, embedded through the cross-file `batchTexts` char-batching via `EmbedChunks`, with per-chunk `ChunkKey` reuse. Co-batching many code files into one request stays valid because isolated embeddings are independent.
- **Text (auto-chunk path):** the file is NOT chunked by PKB and NOT added to `batchTexts`. Estimate tokens via `chars/5`; if ≤ ~120K, one `EmbedDocument(fileText)` call; if larger, split into ~120K-token windows with a **10K-token overlap** and call `EmbedDocument` per window, concatenating the returned chunks. Inference augmentation is skipped entirely. Overlapping windows can emit duplicate chunks, so **dedup by chunk-text identity** before writing.

## Reuse and correctness under contextualization

Per-chunk reuse (`ChunkKey`) is **disabled for the auto-chunk text path** — we don't even have stable chunk identities before the call. Reuse falls back to the existing file-level `blob_sha` skip: an unchanged file (same blob, same model) is skipped wholesale on resume; a changed file is re-sent to `EmbedDocument` in full. This is simpler and correct — Voyage's chunk boundaries can shift when the file changes, so no per-chunk vector can be safely carried across an edit. The cost is that editing one line re-embeds the whole file, acceptable for text docs. Code keeps its existing per-chunk `ChunkKey` reuse unchanged.

## Versioning

Code (isolated) and text (auto-chunk) vectors **share one vec table** under the same `voyage-context-4` model id and output dimension. As established above, the two regimes are compatible: `voyage-context-4` doubles as a standard embedder, so isolated and contextualized vectors are comparable in one space. Therefore:

- **No `ModelName()` suffix and no `MajorVersion` bump.** Turning the option on changes only *how text files are embedded*, not the embedding space. There is no `+ctx` table split.
- Enabling/disabling the option should still re-embed affected text files. Since `blob_sha` reuse alone won't notice a mode flip (blob and model name are unchanged), the mode must participate in the reuse key for text files — e.g. fold an `auto_chunk` marker into the stored per-file embedding spec so a mode change invalidates those files (and only those files). Code files are untouched by the flip. This is the one versioning subtlety to nail down in Stage 4.

## Config surface

Add one field to the `[embedding]` block, kept minimal:

- `contextualizeText` (bool, default false) on `config.ModelConfig` (or on `Config`). When true and the embedding provider supports it, text files use the contextual path and skip inference augmentation.

`Default()` stays as-is (feature off). Document the new key in README's config reference. Consider changing the default embedding model in docs/examples to `voyage-context-4` but do NOT change the shipped `Default()` in this plan (that's a separate re-index-everyone decision).

## Cost

Add `voyage-context-4` (0.12) and `voyage-context-3` (0.18) to `embeddingPricesPerM` (`internal/cost/cost.go`). When a file is on the contextual path, its inference output/input tokens drop out of the projection (no augmentation calls), while its embedding tokens remain. The estimator must mirror the manager's per-file mode decision so the gate stays accurate.

Invariants:
- A text file's chunk vectors are never partially reused across an edit (auto-chunk path relies on file-level blob_sha reuse only); code keeps per-chunk `ChunkKey` reuse.
- Code and text vectors share one vec table under one `voyage-context-4` id + output dimension; they are comparable by design, so cosine comparisons across them are valid.
- Code files are never sent through the auto-chunk path; their behavior is byte-for-byte unchanged when the option is off, and functionally unchanged when it is on.
- With the option off, the entire pipeline behaves exactly as today (no new network shape, no interface change observed by existing providers).
- A text file over ~120K estimated tokens (`chars/5`) is split into ~120K-token windows with 10K-token overlap; overlap-induced duplicate chunks are deduped by chunk-text identity before writing.
- Each `EmbedDocument` response pairs every returned chunk text with exactly one vector of the configured dims; a mismatch is a hard error (mirrors the existing count check in `Voyage.embed`).

# Stages

## Stage 1: Provider capability — `EmbedDocument` on Voyage

**Status: DONE.** Added `ContextualChunk` + `ContextualEmbeddingModel` interface in `internal/embed/embed.go`, and `Voyage.EmbedDocument` in `internal/embed/voyage.go` POSTing to `/v1/contextualizedembeddings` with `enable_auto_chunking: true`, `input_type: "document"`, `output_dimension: dims`, and `inputs: [[document]]` (single whole-document input). Response parsed from nested `data[0].data[].{embedding,index,text}`; per REST docs the backend-generated chunk text is returned as `text` on each embedding item when auto-chunking. Dims validated per chunk and error/status handling mirrors `Voyage.embed`. Decisions/notes: the actual API param is `enable_auto_chunking` (not `enable_auto_chunk` as the plan prose loosely said); the `inputs` field is a list-of-lists (one inner element = the whole doc). Tests added in `voyage_test.go` (happy path returns 3 chunks with text+vectors; interface assertion; non-2xx error; dims mismatch). Full suite/vet/build green; pre-existing lint/gofmt findings in untouched files (gemini.go, openai.go, bedrock.go) left as-is.

- Goal: `embed.Voyage` can auto-chunk and embed a whole document via `/v1/contextualizedembeddings` with `enable_auto_chunk=true`, exposed through a new optional `ContextualEmbeddingModel` interface returning `[]ContextualChunk`. No pipeline wiring yet.
- Design notes: add request/response types for the endpoint (`inputs [][]string` with a single whole-document string, `enable_auto_chunk: true`, `input_type`, `model`, `output_dimension`; response nested `data[].data[].{embedding,index}` plus the returned chunk text). Reuse the existing auth/error-handling style from `Voyage.embed`. Validate each returned vector's dims exactly as `embed()` does. `EmbedDocument` maps the model's chunks (in returned order) to `ContextualChunk{Text, Embedding}`.
- Verification:
  - Behavior: EmbedDocument returns the model's chunks with one vector each, in order.
    - Setup: httptest server returning a canned auto-chunk response (e.g. 3 model-chosen chunks) for a whole-document input; construct `Voyage` pointed at it.
    - Actions: call `EmbedDocument(doc)`.
    - Expected outcome: 3 `ContextualChunk`s with the server's texts and per-index vectors at the configured dims.
  - Behavior: non-2xx / dims-mismatch surfaces an error (no panic).
    - Setup: server returns error JSON / a wrong-dimension vector.
    - Actions: call EmbedDocument.
    - Expected outcome: descriptive error, matching existing `voyage embeddings status ...` style.
- Before moving on: confirm `go build ./...`, `go vet ./...`, `go test ./...`, and lint pass.

## Stage 2: Mock support + config field

**Status: DONE.** Added `contextualizeText` (bool, default false) to `config.ModelConfig` in `internal/config/config.go` with TOML tag `contextualizeText`; it lives on the `[embedding]` block and round-trips via `Load()`'s merge-over-`Default()` (no `Default()` change needed since the zero value is false). `embed.MockModel` (`internal/embed/mock.go`) now implements `ContextualEmbeddingModel`: `EmbedDocument` deterministically auto-chunks by splitting on blank lines (`\n\n`), trims/skips empties, and returns each chunk with a `contextualVector(chunk, document)` that folds in the whole-document vector so contextual output is sibling-dependent and distinct from the isolated `EmbedChunk` vector. Added a `documentCalls` counter + `DocumentCalls()` accessor. Tests: `internal/config/config_test.go` (default false + round-trip true) and new `internal/embed/mock_test.go` (interface assertion, 3-chunk auto-chunk, contextual != isolated). Manager does not yet read the field (Stage 3). Full `go build/vet/test ./...` green; the only lint findings are pre-existing errcheck issues in untouched `gemini.go`/`openai.go`.

- Goal: `embed.MockModel` implements `ContextualEmbeddingModel` (deterministic, but sibling-dependent so contextual output differs from isolated for the same chunk); `config.ModelConfig` gains `contextualizeText` with TOML tag, wired through `Default()`/`Load()` merge. No manager behavior change yet (field read but unused).
- Verification:
  - Behavior: config round-trips the new key and defaults to false.
    - Setup: write a `pkb.toml` with `[embedding] contextualizeText = true`; also load with no file.
    - Actions: `config.Load(root)`.
    - Expected outcome: field true when set, false by default; existing fields unaffected.
  - Behavior: mock EmbedDocument auto-chunks a document and returns chunk+vector pairs.
    - Setup: MockModel with a deterministic splitter (e.g. split on blank lines) producing N chunks.
    - Actions: EmbedDocument a multi-paragraph document.
    - Expected outcome: N `ContextualChunk`s whose texts reconstruct the input and whose vectors are deterministic per chunk text.
- Before moving on: confirm tests, vet, build, lint pass.

**Status: DONE.** Added `ContextualizeText bool` to `index.Options` (`internal/index/manager.go`) plus an `Options.contextualModel()` helper that returns the model as `embed.ContextualEmbeddingModel` only when the flag is on and the model supports the capability. `Reindex`'s per-file loop now, before the isolated `prepareFile` path, checks `contextualModel() && route != Code`; matching text files go through the new `prepareContextualFile`, which reads the file, splits it into overlapping byte windows (`autoChunkWindows`), calls `EmbedDocument` per window, dedups returned chunks by trimmed-text identity, and builds a `preparedFile` with file-tagged chunks (zero-value `Start`/`End`, empty `HeadingContext`/augmentation). These files are written immediately (not added to the cross-file `batchTexts`), so no inference augmentation and no per-chunk `ChunkKey` reuse runs; unchanged files are still skipped by the caller's existing blob_sha check. Windowing constants: `charsPerAutoChunkToken=5`, `autoChunkTokenLimit=120000`, `autoChunkOverlapTokens=10000` → `autoChunkMaxWindowByte=600000`, `autoChunkOverlapByte=50000`. Files ≤ one window are sent whole. Wired `ContextualizeText: cfg.Embedding.ContextualizeText` in `main.go` `setup()`. Decisions/notes: stored `minorSpec`/`aug_spec` for auto-chunk files is the constant `"autochunk"` (a marker anticipating Stage 4's mode-flip invalidation; it does not affect reuse today, which is blob_sha-only). Cost estimation still mirrors the isolated path (over-counts inference for contextual text) — deferred to Stage 4 per the plan. Tests added in `manager_test.go` (`recordingContextModel` recorder + 5 tests: routes text through EmbedDocument with zero inference and file-tagged no-line-range chunks; windows a large file with correct overlap and dedups; code stays on the isolated path; edit re-embeds whole / unchanged skipped; option-off is a no-op). Full `go build/vet/test ./...` green; the only lint findings are pre-existing `errcheck` on `defer st.Close()` in tests (my additions follow the established test convention).

## Stage 3: Manager wiring — auto-chunk path for text files

- Goal: when `contextualizeText` is on and the model is a `ContextualEmbeddingModel`, text files (a) skip PKB chunking and inference augmentation, (b) are token-estimated via `chars/5` and sent whole (≤~120K) or per ~120K/10K-overlap window to `EmbedDocument`, (c) produce file-tagged chunks (no line ranges) from the model's output, (d) dedup overlap duplicates by text identity, and (e) skip per-chunk `ChunkKey` reuse (file-level blob_sha skip only). Code and option-off behavior are unchanged.
- Design notes: text files on this path bypass `chunkFile`/`prepareFile`'s chunk+augment logic; instead a new per-file routine calls `EmbedDocument` (windowing + dedup) and produces the chunk list *from the response*, tagged with the file path and no `start_line`/`end_line`. Code and option-off files flow through the existing `prepareFile` + cross-file char-batch `EmbedChunks` path untouched. `writeFile`/`PutFile` receive the file-tagged chunks + their embeddings (no augmentation blurbs, no inference specs).
- Verification:
  - Behavior: text file on auto-chunk path calls EmbedDocument, never Inference, never joins the char-batch.
    - Setup: harness with a multi-paragraph markdown file; `contextualizeText` on; recording `EmbedDocument` mock + inference mock (`infer.MockModel`).
    - Actions: `Reindex`.
    - Expected outcome: `inf.Calls() == 0`; EmbedDocument received the whole file; stored chunks are file-tagged with no line ranges; `EmbedChunks` not called for this file.
  - Behavior: large text file is windowed with overlap and deduped.
    - Setup: a text file whose `chars/5` estimate exceeds 120K; recording EmbedDocument mock capturing each window.
    - Actions: Reindex.
    - Expected outcome: multiple EmbedDocument calls with ~10K-token overlap between consecutive windows; chunks appearing in the overlap are written once (deduped by text).
  - Behavior: code file is unaffected by the option.
    - Setup: harness with a `.go` file + the option on.
    - Actions: Reindex.
    - Expected outcome: code chunks go through the isolated `EmbedChunks` path (not EmbedDocument); output identical to option-off run, line ranges + breadcrumbs intact.
  - Behavior: editing a contextual text file re-embeds it whole; unchanged file is skipped.
    - Setup: index a text file, then edit it, reindex; also reindex with no change.
    - Actions: Reindex three times.
    - Expected outcome: edit triggers a fresh EmbedDocument for the whole file; unchanged reindex is a blob_sha skip (no EmbedDocument call).
  - Behavior: option off is a no-op.
    - Setup: same fixtures, option off.
    - Actions: Reindex.
    - Expected outcome: identical behavior to current main (inference runs for text, EmbedDocument never called).
- Before moving on: confirm tests, vet, build, lint pass.

## Stage 4: Mode invalidation + cost + docs

**Status: DONE.** Kept code and text on one vec table (no `ModelName` suffix, no `MajorVersion` bump). Mode-flip invalidation: added `minorSpec` to `index.indexedEntry` (populated from `store.FileMeta.MinorSpec`) and an `Options.isContextual(path)` helper; the Reindex skip check now also requires `wasAutoChunk == isContextual(path)` (comparing only the `autochunk` marker, not the full minor spec, so an inference-model change still never re-embeds). Because a same-commit option flip changes no blob/commit, `touchedPaths` gained an `addModeFlips()` step (run on the nothing-changed, incremental, and divergence paths; the full path already includes everything) that adds any indexed file whose stored auto-chunk marker disagrees with the current mode — this re-embeds only the affected text files; code is never added since it is never contextual. Cost: added `voyage-context-4` (0.12) / `voyage-context-3` (0.18) to `embeddingPricesPerM`; `estimate` now short-circuits contextual text files to count whole-file embedding tokens (`len(content)/CharsPerToken`, one file/one chunk) and zero inference. Docs: README config reference gained a `contextualizeText` bullet; `context.md` provider notes updated. Tests: cost pricing entries in `cost_test.go`; index tests `TestContextualizeTextModeFlipReembedsTextOnly`, `TestContextualizeTextSharesOneVecTable`, `TestEstimateContextualTextDropsInferenceKeepsEmbedding`. Full `go build/vet/test ./...` green; only pre-existing errcheck/gofmt findings in untouched files remain (the two new `defer st.Close()` calls follow the established test convention).

- Goal: code and text vectors share one vec table (no name split); flipping the option re-embeds only text files; cost estimation mirrors the mode decision; README documents the option and pricing.
- Design notes:
  - **One vec table.** Do NOT suffix `ModelName()` or bump `MajorVersion`. Confirm `activeModels`, GC (`store.go:311+`), and query-time embedding all operate on the single `voyage-context-4` model id, and that a query vector scores against both isolated code and auto-chunk text rows in that one table.
  - **Mode flip invalidation.** Because `blob_sha` + model name are unchanged when only the mode flips, fold an `auto_chunk` marker into the per-file embedding spec (or minor/reuse key scoped to text files) so enabling/disabling re-embeds text files and only text files; code is untouched. Record the exact mechanism here once verified against how the per-file spec drives reuse.
  - Cost: add `voyage-context-4`/`voyage-context-3` to `embeddingPricesPerM`; make the reindex cost projection drop inference tokens for auto-chunk text files and price their embedding tokens at the voyage-context rate.
- Verification:
  - Behavior: code and text vectors coexist in one vec table and are both searchable.
    - Setup: index a repo with both `.go` and `.md` files, option on.
    - Actions: inspect `activeModels`/table names; run a search that should hit each.
    - Expected outcome: single vec table for `voyage-context-4`; queries return both code and text hits.
  - Behavior: flipping the option re-embeds text files only.
    - Setup: index option-off, then reindex option-on (same blobs).
    - Actions: Reindex twice, capture calls.
    - Expected outcome: text files re-embedded via EmbedDocument on the flip; code files skipped (blob_sha).
  - Behavior: cost estimate for an auto-chunk text corpus excludes inference cost, includes embedding cost.
    - Setup: cost unit test, text-only fixture, option on vs off.
    - Actions: run the estimator.
    - Expected outcome: option-on estimate has zero inference component and non-zero embedding component at the voyage-context rate.
- Before moving on: confirm tests, vet, build, lint pass, and update README config reference + `context.md` provider notes.

## Out of scope / follow-ups

- **Precise line ranges for auto-chunked text.** Reverse-map each returned chunk's text to `start_line`/`end_line` in the source so text hits cite line ranges instead of just the file. Deferred; file-tagged locators for now.
- **Exact token counting.** Replace the `chars/5` estimate with the model's Hugging Face tokenizer for tight window packing; treat a token-overflow 4xx as the backstop until then.
- **Applying auto-chunking / contextualization to code** files (they stay on the isolated AST + breadcrumb path).
- **Changing the shipped `Default()` embedding model** to `voyage-context-4` (a fleet-wide reindex decision).
- **Tuning the window size / overlap** (starting at ~120K window, 10K overlap) and the dedup rule for boundary chunks.

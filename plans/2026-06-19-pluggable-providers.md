# Objective and Context

## User requests (verbatim)

- "alright, so the current default is to use bedrock. I think for corporate environments this makes sense, but I would like to provide an option for individual users. What's a good default for embedding? Keep an 80/20 approach. Balance 'good enough' embedding accuracy against speed, cost, and ease of use (availability of libraries, etc...) I'm considering google for cost... jinna ai for ubiquity... or voyager models (since they're part of anthropic now)..."
- "we need an embedding model *and* an inference model (for augmentation)"
- "what about openai?"
- "I need something that will do well across code + text (markdown docs)"
- "ok... and finally, what if I do want to make ollama / local embedding + inference models possible. I think gemini doesn't get me any closer there, but openai might?"
- "ok, let's do that. Make a plan"

## What we're building and why

Today pkb is hard-wired to one backend: AWS Bedrock (Cohere embed-v4) for
embeddings, and there is no inference model at all — chunk "augmentation" is
purely deterministic heading-breadcrumb prefixing. Bedrock is a good corporate
default (IAM, no extra keys) but is friction for individuals: it needs AWS SSO,
which currently even breaks `./pkb search` locally.

We want pkb to support multiple provider backends for **both** an embedding
model and an inference model (used for augmentation), so individuals can pick a
turnkey single-key provider or run everything locally.

Decision (from the discussion):

- Default configuration is **Bedrock embeddings (Cohere embed-v4) + Bedrock
  Claude Haiku augmentation** — i.e. LLM augmentation is ON by default. This is
  the corporate-friendly turnkey path (single IAM credential covers both).
- The backbone is an **OpenAI-compatible** provider parameterized by base URL +
  model names. One code path serves OpenAI cloud, Ollama, and other local
  servers (llama.cpp, vLLM, LM Studio, LocalAI, text-embeddings-inference) —
  they all emulate `/v1/embeddings` and `/v1/chat/completions`. This is the
  highest-leverage abstraction and is what unlocks local.
- **Gemini** is a bespoke API kept as its own adapter (best free all-rounder
  for mixed code + markdown).
- **Bedrock** stays as-is (corporate default).
- Each provider supplies an embedding model and (where applicable) an inference
  model. Local (Ollama) and OpenAI satisfy both halves under the OpenAI-shaped
  path; Bedrock supplies Claude for inference; Gemini supplies Flash.

## Key types / entities

- `embed.EmbeddingModel` (internal/embed/embed.go): existing interface —
  `ModelName/Dimensions/EmbedChunk/EmbedQuery/EmbedChunks`. Unchanged.
- `embed.Build` (internal/embed/factory.go): provider switch. Extend.
- NEW `infer.InferenceModel` (new package internal/infer): a minimal text-in/
  text-out interface for augmentation.
- NEW `infer.Build`: provider switch mirroring `embed.Build`.
- `config.ModelConfig` / `config.Config` (internal/config/config.go): add an
  `Inference` block and generalize `ModelConfig` for non-AWS providers
  (base URL, api-key env var).
- `index.Options.indexFile` (internal/index/manager.go): where deterministic
  contextualization happens today; the optional inference-augmentation hook and
  its cache-key change live here.
- `store.ChunkKey` (internal/store/store.go): the embedding reuse key —
  load-bearing for correctness once augmentation becomes non-deterministic.

## Relevant files

- internal/embed/embed.go — embedding interface (stable contract).
- internal/embed/factory.go — embedding provider switch (extend).
- internal/embed/bedrock.go — existing Bedrock/Cohere model (reference pattern).
- internal/embed/openai.go (NEW) — OpenAI-compatible embedding model.
- internal/embed/gemini.go (NEW) — Gemini embedding model.
- internal/infer/*.go (NEW) — inference interface + provider impls + mock.
- internal/config/config.go — config schema + defaults.
- internal/index/manager.go — augmentation + embedding-reuse wiring.
- internal/store/store.go — chunk reuse key.
- cmd/pkb/main.go — builds models from config (line ~88).

# Design

## Two parallel provider families

Mirror the existing embedding factory with an inference factory. Both read a
`config.ModelConfig`-shaped selection and dispatch on `Provider`:

- `openai` / `openai-compatible`: HTTP client against an OpenAI-shaped base URL.
  - Embeddings: POST `{base}/v1/embeddings` with `{model, input:[...] }`.
  - Inference: POST `{base}/v1/chat/completions` — deliberately the **Chat
    Completions** wire format, not the newer Responses API. Chat Completions
    is still fully supported by OpenAI and is the lowest-common-denominator
    that every local server (Ollama, llama.cpp, vLLM, LM Studio, LiteLLM)
    implements; Responses support across local backends is still spotty. A
    single stateless "situate this chunk" call needs none of the Responses
    agentic features. (Responses could be added later for the OpenAI-cloud
    path if ever needed.)
  - Config carries `baseURL` (default `https://api.openai.com`) and an
    `apiKeyEnv` naming the env var holding the key (default `OPENAI_API_KEY`;
    for Ollama, point baseURL at `http://localhost:11434` and accept an empty/
    dummy key). This single adapter is the local story.
- `gemini`: bespoke adapter (Google Generative Language API), key from
  `GEMINI_API_KEY`.
- `bedrock`: unchanged. Note the default inference path (Bedrock Claude Haiku)
  already speaks the **Anthropic Messages** format, so we build a
  Messages-shaped request regardless.
- `anthropic` (optional sibling): an Anthropic Messages adapter reusing the
  Bedrock request shape — covers the direct Anthropic API and Ollama's
  Anthropic compatibility layer. Cheap to add since Bedrock already needs it;
  include only if it falls out naturally.
- `mock`: deterministic, for tests (already exists for embed; add for infer).

`ModelConfig` generalization (additive, back-compatible): keep `Provider`,
`Model`, `Dimensions`, `Region`/`awsregion`, `Profile`/`awsprofile`; add
`BaseURL` (`baseurl`) and `APIKeyEnv` (`apikeyenv`). Bedrock ignores the new
fields; OpenAI/Gemini ignore the AWS ones.

`Config` gains an `Inference ModelConfig` (`[inference]`) defaulting to Bedrock
Claude Haiku, so LLM augmentation runs by default. Setting the inference
provider to `none` (explicit disable) falls back to the deterministic
heading-prefix path with no LLM calls.

## Inference interface

Keep it minimal — augmentation only needs one call:

    type InferenceModel interface {
        ModelName() string
        // Augment returns a short contextual blurb situating a chunk within
        // its document, given the chunk text and surrounding context.
        Complete(prompt string) (string, error)
    }

LLM augmentation applies **only to markdown/text files**, not code. The prompt
situates a chunk within its whole document (file content + chunk), asking for a
one-paragraph context that is prepended to the chunk before embedding — the
contextual-retrieval pattern. Code files keep deterministic chunking with the
heading/breadcrumb prefix and no LLM calls. Failures must degrade gracefully to
the deterministic path, never fail the whole index run.

## Reindex granularity: whole-file for augmented text, per-chunk for code

Because a text chunk's augmented context is derived from the **entire file**,
any edit anywhere in the file can change the meaning (and thus the embedding) of
every chunk in it. Trying to reuse individual chunk vectors there is both unsafe
and pointless. So:

- For **markdown/text files** (LLM-augmented): treat the file as the unit of
  reuse. If the file's blob sha is unchanged, reuse all of its chunk vectors as
  they are; if it changed at all, re-augment and re-embed every chunk in the
  file. No per-chunk reuse within a changed text file.
- For **code files** (deterministic, no LLM augmentation): keep today's
  per-chunk reuse keyed by `store.ChunkKey(headingContext, text)`, so a small
  edit to a large code file still only re-embeds the affected chunks.

This sidesteps the non-determinism problem entirely for the path that has it:
augmented embeddings are only ever produced and stored as a coherent
whole-file set, never partially reused.

Additionally fold the inference model identity into reuse for text files (e.g.
via the stored model name) so switching/upgrading the inference model
invalidates stale augmented embeddings rather than silently reusing them.

Invariants:
- An embedding stored for a chunk must always correspond to the exact text that
  was embedded. For augmented text files this is guaranteed by whole-file
  invalidation (a changed blob sha re-embeds the whole file); for code files by
  the per-chunk `ChunkKey`.
- With inference explicitly disabled, indexing and search behave like today's
  deterministic heading-prefix path with no LLM calls; the default config
  instead runs Bedrock Haiku augmentation.
- Vec tables remain keyed by embedding model name; embeddings from different
  embedding models never mix.
- A provider/inference failure during indexing degrades to the deterministic
  path with a warning; it does not abort the run or corrupt the store.
- Search-time query embedding uses the same embedding model/provider as indexing
  (query is never augmented).

## Alternatives considered

- A separate adapter per local server (Ollama, vLLM, …): rejected — they all
  speak the OpenAI shape, so one parameterized adapter covers them.
- Making Gemini the local enabler: rejected — bespoke API, no local story.
  OpenAI-compatible is what unlocks Ollama.
- Embedding augmentation into the deterministic ChunkKey unchanged: rejected —
  would silently serve stale vectors when augmentation output changes.

# Stages

## Stage 1 — Generalize config schema (no behavior change)  ✅ DONE

Decisions/notes:
- Added `BaseURL` (`baseurl`) and `APIKeyEnv` (`apikeyenv`) to `ModelConfig`.
- Added `Inference ModelConfig` (`[inference]`) to `Config`, defaulting to
  Bedrock Claude Haiku (`us.anthropic.claude-3-5-haiku-20241022-v1:0`).
- Explicit disable is `provider = "none"` in `[inference]` (consumed by later
  stages; no special config-load handling needed — it round-trips as data).
- Tests added in config_test.go: round-trip of new HTTP fields + inference
  block, and default-inference fallback when `[inference]` is absent.

- Goal: `ModelConfig` carries `BaseURL`/`APIKeyEnv`; `Config` has an optional
  `Inference` block defaulting to Bedrock Claude Haiku (the deterministic
  heading-prefix always applies). An explicit disable turns LLM augmentation off.
- Verification:
  - Behavior: loading a config with `[inference]` + `baseurl`/`apikeyenv`
    round-trips into the struct; absent fields fall back to defaults.
  - Setup: temp config files (existing config_test.go pattern).
  - Actions: `config.Load` on fixtures with/without the new blocks.
  - Expected outcome: new fields populate; a config with no `[inference]` block
    falls back to the new default (Bedrock embeddings + Bedrock Claude Haiku).
- Before moving on: tests, `go vet`, build all pass.

## Stage 2 — OpenAI-compatible embedding provider  ✅ DONE

Decisions/notes:
- Added `internal/embed/openai.go` (`OpenAICompatible`) hitting
  `{baseURL}/v1/embeddings`; baseURL trailing slashes trimmed, defaults to
  `https://api.openai.com`, key from `APIKeyEnv` (default `OPENAI_API_KEY`).
- Empty key tolerated (no Authorization header) so Ollama/local servers work.
- `embed.Build` signature extended with `baseURL, apiKeyEnv` and now handles
  `openai`/`openai-compatible`; `cmd/pkb/main.go` caller updated.
- Non-2xx surfaces `error.message`; data-length mismatch is an actionable error.
- Tests in openai_test.go use an httptest fake (success, error status, empty data).

- Goal: `embed.Build` handles `provider="openai"`/`openai-compatible`, hitting
  `{baseURL}/v1/embeddings`, with key from `APIKeyEnv`. Works against OpenAI and
  Ollama by base-URL swap.
- Verification:
  - Behavior: request body shape (`model`, `input` batch) and response parsing
    into `[]Embedding` of the configured dimensionality.
  - Setup: httptest server returning a canned embeddings payload.
  - Actions: `EmbedChunk`, `EmbedQuery`, `EmbedChunks` against the fake server.
  - Expected outcome: correct vectors/lengths; non-2xx and empty-data responses
    return actionable errors.
- Before moving on: tests, vet, build pass.

## Stage 3 — Gemini embedding provider

- Goal: `embed.Build` handles `provider="gemini"` against the Generative
  Language embeddings endpoint, key from `GEMINI_API_KEY`.
- Verification: same shape as Stage 2 with an httptest fake of the Gemini
  response; assert dimension handling (no small-dim truncation reliance).
- Before moving on: tests, vet, build pass.

## Stage 4 — Inference package + providers (no indexing wiring yet)

- Goal: new `internal/infer` with `InferenceModel`, `Build`, a `mock`, an
  OpenAI-compatible impl (`/v1/chat/completions`), a Gemini impl, and a Bedrock
  (Claude) impl. Pure unit-level; not yet called by the indexer.
- Verification:
  - Behavior: `Complete(prompt)` returns the assistant text; errors surface.
  - Setup: httptest fakes (OpenAI/Gemini) and a mock for deterministic tests.
  - Actions: call `Complete` and assert parsed output.
  - Expected outcome: correct text extraction per provider; clear errors.
- Before moving on: tests, vet, build pass.

## Stage 5 — Reindex granularity: whole-file for text, per-chunk for code

- Goal: split the reuse path by file type before any real inference runs. Code
  files keep today's per-chunk `ChunkKey` reuse. Text/markdown files reuse only
  at whole-file granularity (unchanged blob sha → reuse all chunk vectors;
  changed → re-embed every chunk in the file), with the inference-model identity
  folded in so a model switch invalidates the file's vectors.
- Verification:
  - Behavior 1 (code file): unchanged chunk reuses its vector; edited chunk
    re-embeds — same as current tests.
  - Behavior 2 (text file, unchanged blob): all chunk vectors reused, no
    re-embed.
  - Behavior 3 (text file, any change): every chunk in the file re-embeds, no
    per-chunk reuse.
  - Behavior 4 (text file, inference-model identity changed): file's vectors
    invalidated and re-embedded.
  - Setup: store + mock embedding model; a code fixture and a text fixture.
  - Actions: run the reuse path twice with varied inputs per file type.
  - Expected outcome: per-chunk reuse for code; all-or-nothing per-file reuse
    for text keyed on blob sha + inference-model identity.
- Before moving on: tests, vet, build pass.

## Stage 6 — Wire augmentation into indexing

- Goal: when `[inference]` is configured, `indexFile` builds an augmentation
  prompt per chunk, calls the inference model, prepends the result to the chunk
  before embedding, and persists the contextualized text. On inference error,
  fall back to the deterministic prefix with a warning.
- Verification:
  - Behavior: with a mock inference model, contextualized text includes the
    generated blurb and is what gets embedded/stored; with inference disabled,
    output matches today.
  - Behavior: inference error path falls back deterministically and does not
    abort the run.
  - Setup: index a small fixture repo with mock embed + mock/failing infer.
  - Actions: run a full index pass; inspect stored contextualized text and
    vectors.
  - Expected outcome: augmented input embedded/stored when enabled; graceful
    degradation on failure; reuse semantics from Stage 5 hold across reindex.
- Before moving on: tests, vet, build pass.

## Stage 7 — main.go wiring, defaults, and docs

- Goal: `cmd/pkb/main.go` builds both the embedding and (optional) inference
  models from config. Document the provider options and recommended defaults
  (default/corporate: Bedrock embeddings + Bedrock Haiku augmentation;
  individual: Gemini or OpenAI for both; local: OpenAI-compatible
  pointed at Ollama) in context.md/README, including required env vars.
- Verification:
  - Behavior: `./pkb` selects providers per config; missing key/baseURL yields
    an actionable error; mock provider drives an end-to-end smoke test without
    network.
  - Setup: smoke test (internal/smoke) with mock providers.
  - Actions: run index + search end to end.
  - Expected outcome: pipeline works with mock providers offline; real provider
    selection is config-driven and documented.
- Before moving on: full suite, vet, build, and the smoke test pass.

# Running pkb locally on Apple Silicon

This guide covers a fully local pkb setup on an Apple Silicon Mac: a recommended
**inference** model for chunk augmentation (with quantization, settings, memory,
and throughput guidance), and an optional **MLX embeddings** server.

pkb makes two kinds of model calls:

- **Inference** (`[inference]`): one short text-generation call per chunk that
  situates the chunk within its whole document (Anthropic's contextual-retrieval
  pattern). Optional — `provider = "none"` skips it.
- **Embedding** (`[embedding]`): turns each (augmented) chunk into a vector.

Both speak the OpenAI wire format, so any OpenAI-compatible local server works.

---

# Inference (chunk augmentation)

## Recommended model: Qwen3-4B-Instruct-2507

For augmentation we recommend the **non-thinking** `Qwen3-4B-Instruct-2507`
build. Reasons specific to this task:

- **No thinking-mode leakage.** pkb prepends the model's raw reply into the
  chunk's `<context>`. Reasoning models emit `<think>…</think>` blocks that would
  pollute the embedded text. The dedicated Instruct build is non-reasoning by
  default, so there's nothing to strip. (Gemma 3 4B is also non-thinking and a
  fine alternative; Qwen just follows the terse "answer only" instruction more
  reliably and quantizes more cleanly.)
- **Context window.** Augmentation feeds the _whole document_ (not just the
  chunk) on every call, so the model needs a large window — 32K is the floor.
  Qwen3-4B handles 32K+ natively.
- **Quantization robustness.** Qwen3 holds up well at 4-bit; Gemma's outlier
  activations make it more quant-sensitive.

If you used a reasoning model instead, append `/no_think` to the prompt as a
portable escape hatch — but prefer the Instruct variant.

## Settings

```toml
[inference]
provider = "openai"
model    = "qwen3:4b-instruct-2507-q4_K_M"  # match the id your runner reports
baseurl  = "http://localhost:11434"   # Ollama; pkb appends /v1/chat/completions
```

- **Weights:** 4-bit (MLX). The quality hit for a short single-paragraph blurb
  is negligible; 8-bit is a cheap upgrade (~4.5 GB) if blurbs read generic.
- **KV cache:** Q8 to halve cache memory at no meaningful quality cost.
- **Context length:** set to **32768** (or larger than your biggest document +
  ~2K slack). This is the single most important setting — it sizes the KV cache.

### Don't truncate the document (the #1 footgun)

Context length and KV cache size are the same knob: the cache is allocated to
hold the configured window. If the window is smaller than a document, the doc is
**silently truncated**, which quietly degrades retrieval quality _and_ defeats
prefix caching.

- **Ollama:** default `num_ctx` is only ~2–4K. Set `OLLAMA_CONTEXT_LENGTH=32768`
  (or `PARAMETER num_ctx 32768` in a Modelfile) and `OLLAMA_KV_CACHE_TYPE=q8_0`.

### Prefix caching makes re-feeding the document cheap

pkb's prompt is laid out as `<document> … </document>` first, then the varying
`<chunk>`. That entire document block is byte-identical for every chunk in a
file and sits at the front, so an OpenAI-compatible server with prefix caching
reuses its KV from the previous call — you only pay full prefill once per file.
pkb processes a file's chunks back-to-back to keep that cache hot. (A too-small
context window evicts the prefix and defeats this, hence the sizing above.)

## Memory & throughput (rough, M4-class)

For a 4B model at 4-bit weights + Q8 KV cache, 32K context:

- **Weights:** ~2.5 GB. **KV cache @ 32K:** ~0.5–0.75 GB (Q8).
- **Resident working set:** ~3.5–5 GB. Comfortable on a 16 GB Mac; trivial on
  18/24/32 GB.
- **Throughput:** ~70–120+ tok/s generation on M3/M4 Pro/Max at 4-bit. Blurbs
  are short (`augmentMaxTokens` caps output at 512), so **prefill dominates** —
  prefix caching across a file's chunks is what actually saves wall-clock time.

Augmentation is the bottleneck for a full reindex: it's one sequential
`Complete()` call per _text/markdown_ chunk (code files are never augmented). In
a local run of this repo (Ollama, `qwen3:4b-instruct-2507-q4_K_M`) the
end-to-end rate was **~1.3 chunks/sec** — 47 files / 366 chunks in ~4m43s —
versus pure-embedding throughput of a few thousand tokens/sec (see below). If
indexing time matters more than blurb quality, set `provider = "none"` to skip
augmentation and fall back to the deterministic heading-prefix path.

`pkb reindex` logs per-file progress (cumulative files/chunks, chunks/sec, and
an approximate embed tok/sec) to stderr so you can watch the rate live.

---

# MLX embeddings on Apple Silicon

`scripts/mlx_embed_server.py` is a small, optional helper that serves a local
embedding model on Apple Silicon's **MLX** engine behind an OpenAI-compatible
`/v1/embeddings` endpoint. pkb's `openai` provider talks to it unchanged.

You do **not** need this to use pkb. It exists for one specific situation:
running a high-quality embedding model **locally and fast** on a Mac.

## Why this exists

MLX is Apple's ML framework built for Apple Silicon's unified memory (CPU and
GPU share one memory pool, no copies). For models under ~14B parameters it is
meaningfully faster than the llama.cpp/Metal path, which makes it attractive for
the embedding pass over a large codebase.

The catch: the popular local runners don't (yet) serve MLX _embedding_ models
correctly.

- **LM Studio** misclassifies the MLX build of Qwen3-Embedding as a generative
  LLM. It loads, but it's routed through the text-generation path with no
  pooling head, so `/v1/embeddings` rejects it (and chat completions emit
  garbage). LM Studio _does_ serve the **GGUF** build of the same model
  correctly — but that runs on Metal, not MLX.
- **Ollama** has the same gap; first-class embedding support for the
  Safetensors → MLX path is still open upstream
  (ollama/ollama#16076).

The reason is that embedding models need a dedicated forward + pooling +
L2-normalize path that's separate from token generation, and that wiring is
still landing across the general-purpose runners.

This server sidesteps the runners entirely. It uses the `mlx_embeddings`
library, which implements the correct pooling and normalization for embedding
models, and exposes it on the OpenAI wire format pkb already speaks.

## When you want it

- You're on an Apple Silicon Mac and want a fully local, no-API-key embedding
  model that actually uses MLX.
- You want Qwen3-Embedding-0.6B specifically (strong on mixed code + markdown,
  32K context, multilingual) running as fast as the hardware allows.

If you don't care about MLX specifically, the simpler options are:

- **Simplest local:** LM Studio with the **GGUF** build of Qwen3-Embedding-0.6B
  (`Qwen/Qwen3-Embedding-0.6B-GGUF`). Runs on Metal; no Python needed.
- **Corporate/turnkey:** the default Bedrock path (no local model at all).

## Setup

Requires [`uv`](https://docs.astral.sh/uv/). No manual install or virtualenv —
`uvx` builds an ephemeral environment on the fly:

```bash
uvx --with mlx-embeddings --with fastapi --with uvicorn \
    python scripts/mlx_embed_server.py --port 8788
```

On first run it downloads the model (~600 MB) from Hugging Face and caches it.
The server loads the model, then prints `Uvicorn running on http://127.0.0.1:8788`.

Point pkb at it via `pkb.toml`:

```toml
[embedding]
provider   = "openai"
baseurl    = "http://localhost:8788"
model      = "mlx-community/Qwen3-Embedding-0.6B-mxfp8"
dimensions = 1024
```

Then `pkb reindex` as usual. The server only needs to be running during
`reindex` and `search` (anything that embeds); it is not needed to read an
already-built index.

### Configuration

- `--port` / `MLX_EMBED_PORT`: listen port (default `8788`).
- `--host` / `MLX_EMBED_HOST`: bind address (default `127.0.0.1`).
- `MLX_EMBED_MODEL`: any `mlx-community` embedding repo (default
  `mlx-community/Qwen3-Embedding-0.6B-mxfp8`). The 4B/8B Qwen3-Embedding builds
  work too if you want more quality at the cost of speed.
- `MLX_EMBED_MAX_BATCH`: max texts run through the model per forward pass
  (default `8`). Peak memory scales with `batch × seq_len²` and the batch is
  padded to its longest member, so the server sub-batches large requests and
  clears MLX's buffer cache between sub-batches to keep RSS bounded. Lower it if
  you index files with very long lines/chunks; raise it for more throughput on a
  high-memory machine.

## Verify it works

```bash
curl -s http://localhost:8788/v1/embeddings \
  -H "Content-Type: application/json" \
  -d '{"input": "search_document: hello world"}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("dims", len(d["data"][0]["embedding"]))'
# -> dims 1024
```

A correct response is a 1024-dimensional, unit-normalized vector. If you instead
get an error or a non-embedding response, you're talking to a generative runner,
not this server.

## Notes

- **Throughput:** the embedding pass itself runs at **~3,000–4,000 tok/sec**
  (observed locally on an M4 mac book pro, approximating 3 chars/token), so embedding is rarely the
  bottleneck — LLM augmentation of text chunks dominates a full reindex (see the
  Inference section). Incremental reindexes only re-embed changed files, so they
  stay cheap.
- **Memory:** the server sub-batches requests (`MLX_EMBED_MAX_BATCH`, default 8)
  and clears MLX's buffer cache between sub-batches. Without this, an unbounded
  padded batch plus MLX's retained allocator cache could balloon RSS into the
  tens of GB; with it, resident memory tracks the live working set (a few GB).
- **Prefixes:** Qwen3-Embedding expects an instruction on _queries_ and raw
  _documents_. pkb applies this through its query/document embedding split — the
  server stays a thin pass-through.

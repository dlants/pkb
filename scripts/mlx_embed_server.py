#!/usr/bin/env python3
"""Minimal OpenAI-compatible /v1/embeddings server backed by MLX.

Why this exists: general-purpose local runners (LM Studio, Ollama) currently
misclassify MLX embedding models (e.g. Qwen3-Embedding) as generative LLMs and
route them through a text-generation path with no pooling head, so their
/v1/embeddings endpoint rejects the model. This shim uses the `mlx_embeddings`
library directly (correct mean/last-token pooling + L2 normalization) and
exposes it on the OpenAI wire format that pkb's `openai` provider speaks.

See MLX_EMBEDDINGS.md at the repo root for the full rationale and setup.

Run (no install needed) with uv:

    uvx --with mlx-embeddings --with fastapi --with uvicorn \\
        python scripts/mlx_embed_server.py --port 8788

Then point pkb at it:

    [embedding]
    provider   = "openai"
    baseurl    = "http://localhost:8788"
    model      = "mlx-community/Qwen3-Embedding-0.6B-mxfp8"
    dimensions = 1024
"""
import argparse
import os
from typing import List, Union

import mlx.core as mx
import uvicorn
from fastapi import FastAPI
from mlx_embeddings import generate, load
from pydantic import BaseModel

DEFAULT_MODEL = "mlx-community/Qwen3-Embedding-0.6B-mxfp8"
MODEL = os.environ.get("MLX_EMBED_MODEL", DEFAULT_MODEL)
# Cap how many texts are run through the model at once. Peak memory scales with
# batch_size * seq_len^2 (attention), and the batch is padded to its longest
# member, so an unbounded batch with one long chunk can balloon RSS into tens of
# GB. Sub-batching keeps the working set small and steady.
MAX_BATCH = int(os.environ.get("MLX_EMBED_MAX_BATCH", "8"))

app = FastAPI()
print(f"loading {MODEL} ...", flush=True)
_model, _tokenizer = load(MODEL)
print("model loaded", flush=True)


class EmbeddingRequest(BaseModel):
    input: Union[str, List[str]]
    model: str = MODEL


@app.get("/v1/models")
def list_models():
    return {"object": "list", "data": [{"id": MODEL, "object": "model", "owned_by": "mlx"}]}


@app.post("/v1/embeddings")
def create_embeddings(req: EmbeddingRequest):
    texts = [req.input] if isinstance(req.input, str) else req.input
    vectors = []
    for start in range(0, len(texts), MAX_BATCH):
        batch = texts[start : start + MAX_BATCH]
        out = generate(_model, _tokenizer, texts=batch)
        vectors.extend(out.text_embeds.tolist())
        # MLX retains freed buffers in a cache that otherwise grows unbounded
        # across requests; release them so RSS tracks the live working set.
        del out
        mx.clear_cache()
    data = [
        {"object": "embedding", "index": i, "embedding": vec}
        for i, vec in enumerate(vectors)
    ]
    return {
        "object": "list",
        "data": data,
        "model": req.model,
        "usage": {"prompt_tokens": 0, "total_tokens": 0},
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=int(os.environ.get("MLX_EMBED_PORT", "8788")))
    parser.add_argument("--host", default=os.environ.get("MLX_EMBED_HOST", "127.0.0.1"))
    args = parser.parse_args()
    uvicorn.run(app, host=args.host, port=args.port)


if __name__ == "__main__":
    main()

# PKB (git-repo-rooted code + docs search)

A semantic search index over the code and docs of a git repository. It sits at
the repo root, is refreshed out-of-band (commit hook / CI when code lands on the
default branch), and exposes a CLI for agents to ask broad orientation
questions. Written in Go; statically links SQLite + sqlite-vec and tree-sitter
grammars via cgo.

## Providers

PKB uses a pluggable **embedding** model (all files) and an optional **inference** model (augments markdown/text chunks before embedding; code is never augmented). Both are selected in `pkb.toml` via `[embedding]` / `[inference]` blocks. Providers: `bedrock` (corporate default — Cohere embed-v4 - Claude Haiku, IAM creds), `openai`/`openai-compatible` (OpenAI cloud or local servers like Ollama via `baseurl`), `gemini`, `none` (inference only — disables augmentation), and `mock` (tests). HTTP providers read the API key from the env var named by `apikeyenv` (`OPENAI_API_KEY` / `GEMINI_API_KEY` defaults). See README.md for the full config reference.

## Storage (SQLite)

The index is a single SQLite file (`pkb.db`) with the `sqlite-vec` extension statically linked. Two app tables plus one `vec0` virtual table per embedding model:

- **`files`** — `id`, `path`, `model_name`, `embedding_version` (the global `MajorVersion`), `blob_sha`, `inference_model`, `complete`, `indexed_gen`, `minor_spec`; `UNIQUE(path, model_name, embedding_version)`.
- **`chunks`** — `id`, `file_id` (FK → `files`, cascade delete), `text`, `contextualized_text`, `heading_context`, `start_line`, `start_col`, `end_line`, `end_col`, `gen`, `augmentation`, `aug_spec`.
- **`vec_<model>_v<major>`** — `vec0` virtual table, `chunk_id INTEGER PRIMARY KEY` → `embedding float[dims] distance_metric=cosine` (e.g. `vec_voyage_code_3_256_v3`). sqlite-vec also creates its own shadow tables (`*_chunks`, `*_rowids`, `*_info`, `*_vector_chunks00`).

**Major vs minor versioning.** The vec table is keyed by `model_name` + `MajorVersion`, so bumping the major version (chunking algorithm, breadcrumbs, tree-sitter/scm, embedding model, or dimensions) re-keys the index and forces a full re-embed. `minor_spec`/`aug_spec` (augmentation on/off, inference model, prompt version) are recorded but **never** invalidate a vector.

**Generations (crash-safe incremental reindex).** Each file is reindexed under a fresh generation (`indexed_gen + 1`): `StartFile` discards half-written chunks from a crashed attempt and leaves the committed generation searchable; new chunks are written one transaction at a time under the new `gen`; `FinalizeFile` atomically advances `indexed_gen` and drops the superseded generation's rows. `Search` and `ChunkEmbeddings` only read rows where `c.gen = f.indexed_gen`, so a partial generation is invisible and per-chunk reuse (keyed by `ChunkKey(heading_context, text)`) lets unchanged chunks skip both the embedding and inference calls.

## Commands

### Build

```bash
go build -o pkb ./cmd/pkb
```

### Type Check / Vet / Test

```bash
go build ./...
go vet ./...
go test ./...
```

## Searching this repo

This repo dogfoods its own pkb setup. The `pkb` binary lives at the repo root (`./pkb`, built with `go build -o pkb ./cmd/pkb`), and the index `pkb.db` is checked into git, so search works out of the box. Reindexing is wired up via a git hook — see `setup-hooks.sh` (installs hooks) and `hooks/pre-push` (reindexes on push to the default branch and reminds you to commit `pkb.db`).

```bash
./pkb search "<natural language query>"   # -k N sets result count (default 5)
./pkb stats                               # index commit, indexedAt, file/chunk counts
./pkb reindex                             # sync the index against HEAD. No need to do this manually, it will happen on push
./pkb chunk <file>                        # display how this file would be chunked (for debugging)
```

Each result is a scored snippet with its file path — treat it as a pointer and open the file to read the real code. The index reflects the last indexed commit, not your working tree; don't worry about reindexing local changes, the pre-push hook handles it.

<system_reminder>
For exploratory / orientation questions about this codebase ("where is X handled?", "how does Y work?"), use `./pkb search` rather than grepping with `rg`. Reserve `rg` for exact symbol/string lookups.
</system_reminder>

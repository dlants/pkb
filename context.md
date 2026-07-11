# PKB (git-repo-rooted code + docs search)

A semantic search index over the code and docs of a git repository. It sits at
the repo root, is refreshed out-of-band (commit hook / CI when code lands on the
default branch), and exposes a CLI for agents to ask broad orientation
questions. Written in Go; statically links SQLite + sqlite-vec and tree-sitter
grammars via cgo.

## Providers

PKB uses a pluggable **embedding** model (all files) and an optional **inference** model (augments markdown/text chunks before embedding; code is never augmented). Both are selected in `pkb.toml` via `[embedding]` / `[inference]` blocks. Providers: `bedrock` (corporate default — Cohere embed-v4 - Claude Haiku, IAM creds), `openai`/`openai-compatible` (OpenAI cloud or local servers like Ollama via `baseurl`), `gemini`, `none` (inference only — disables augmentation), and `mock` (tests). HTTP providers read the API key from the env var named by `apikeyenv` (`OPENAI_API_KEY` / `GEMINI_API_KEY` defaults). See README.md for the full config reference.

Optional **contextualized text** path: setting `contextualizeText = true` in `[embedding]` (Voyage `voyage-context-4` only) makes text files skip PKB chunking + LLM augmentation and go whole to Voyage's `/v1/contextualizedembeddings` auto-chunk endpoint (large files split into ~120K-token windows with 10K overlap, chunks deduped by text). Code stays on the isolated AST+breadcrumb path; both regimes share one vec table under the same model id + dimension (no `ModelName` suffix, no `MajorVersion` bump). Mode is recorded per file as the `autochunk` minor_spec marker so flipping the option re-embeds only the affected text files (via `touchedPaths` mode-flip detection). Cost estimation (`internal/cost`) prices `voyage-context-4` (0.12) / `voyage-context-3` (0.18) and drops inference tokens for auto-chunk text files.

## Storage (SQLite)

The index is a single SQLite file (`pkb.db`) with the `sqlite-vec` extension statically linked. Two app tables plus one `vec0` virtual table per embedding model:

- **`files`** — `id`, `path`, `model_name`, `embedding_version` (the global `MajorVersion`), `blob_sha`, `minor_spec`; `UNIQUE(path, model_name, embedding_version)`.
- **`chunks`** — `id`, `file_id` (FK → `files`, cascade delete), `text`, `contextualized_text`, `heading_context`, `start_line`, `start_col`, `end_line`, `end_col`, `augmentation`, `aug_spec`.
- **`vec_<model>_v<major>`** — `vec0` virtual table, `chunk_id INTEGER PRIMARY KEY` → `embedding float[dims] distance_metric=cosine` (e.g. `vec_voyage_code_3_256_v3`). sqlite-vec also creates its own shadow tables (`*_chunks`, `*_rowids`, `*_info`, `*_vector_chunks00`).

**Major vs minor versioning.** The vec table is keyed by `model_name` + `MajorVersion`, so bumping the major version (chunking algorithm, breadcrumbs, tree-sitter/scm, embedding model, or dimensions) re-keys the index and forces a full re-embed. `minor_spec`/`aug_spec` (augmentation on/off, inference model, prompt version) are recorded but **never** invalidate a vector.

**Per-file crash safety.** A full reindex is "wipe `pkb.db`, then run reindex." Each file is (re)indexed by `PutFile` in a single transaction: it deletes the path's old rows, inserts the file row recording the new `blob_sha` + `minor_spec`, and inserts every chunk + vector. The expensive embedding/inference work happens in memory first, so the write is quick; a crash before commit leaves the previously committed file rows intact. On resume, a file whose recorded `blob_sha` already matches the tree (same embedding model) is skipped, so completed files are not redone — recovery is at file granularity (an interrupted file is simply reindexed). Per-chunk reuse (keyed by `ChunkKey(heading_context, text)` via `ChunkEmbeddings`, read before the rewrite) lets unchanged chunks of a changed file skip both the embedding and inference calls.

## Commands

## Versioning

`pkb version` (also `--version`/`-v`) prints the module version via `runtime/debug.ReadBuildInfo()`: a clean `vX.Y.Z` for `go install ...@latest` builds off a tag, or `(devel)` for local builds. Releases are auto-tagged by `.github/workflows/tag-release.yml`: on every push to main (ignoring the `pkb.db`/`pkb-state.toml` reindex commits) it bumps the highest `vX.Y.Z` tag's patch and pushes the new tag (seeding `v0.1.0` if none exist), so `@latest` always tracks main. Push a tag manually for minor/major bumps; the next auto-bump continues from there.

### Build

```bash
go build -o pkb .
```

### Type Check / Vet / Test

```bash
go build ./...
go vet ./...
go test ./...
```

## Searching this repo

This repo dogfoods its own pkb setup. The `pkb` binary lives at the repo root (`./pkb`, built with `go build -o pkb .`), and the index `pkb.db` is checked into git, so search works out of the box. Reindexing is wired up via a git hook — see `setup-hooks.sh` (installs hooks) and `hooks/pre-push` (reindexes on push to the default branch and reminds you to commit `pkb.db`).

```bash
./pkb search "<natural language query>"   # -k N sets result count (default 5)
./pkb stats                               # index commit, indexedAt, file/chunk counts
./pkb reindex                             # sync the index against HEAD. No need to do this manually, it will happen on push
./pkb chunk <file>                        # show each chunk exactly as it is embedded (heading breadcrumb rendered as comments/context blocks ahead of the chunk text; augmentation omitted since it needs inference). For debugging.
```

Each result is a scored snippet with its file path — treat it as a pointer and open the file to read the real code. The index reflects the last indexed commit, not your working tree; don't worry about reindexing local changes, the pre-push hook handles it.

<system_reminder>
For exploratory / orientation questions about this codebase ("where is X handled?", "how does Y work?"), use `./pkb search` rather than grepping with `rg`. Reserve `rg` for exact symbol/string lookups.
</system_reminder>

# PKB (git-repo-rooted code + docs search)

A semantic search index over the code and docs of a git repository. It sits at
the repo root, is refreshed out-of-band (commit hook / CI when code lands on the
default branch), and exposes a CLI for agents to ask broad orientation
questions. Written in Go; statically links SQLite + sqlite-vec and tree-sitter
grammars via cgo. No watcher, no node runtime.

## Project Structure

```
cmd/pkb/main.go          # CLI entry (stdlib flag): reindex, search, stats
internal/
  git/git.go             # repo root, ref sha, ls-tree, diff --name-status, cat-file -e, merge-base
  index/manager.go       # change detection (incremental/divergence/full) + reindex flow
  store/store.go         # sqlite + sqlite-vec: vec0 table per (modelName,version), files table, CRUD, search
  chunk/chunk.go         # ChunkInfo + Position types + shared split helpers
  chunk/markdown.go      # markdown/text chunker
  chunk/code.go          # tree-sitter code chunker (ChunkCode)
  chunk/grammars.go      # grammar registry (go, js, ts, tsx, python, rust)
  embed/embed.go         # EmbeddingModel interface
  embed/bedrock.go       # Cohere-on-Bedrock embeddings (aws-sdk-go-v2 InvokeModel)
  embed/factory.go       # Build(provider, model, dims) -> EmbeddingModel ("bedrock" | "mock")
  embed/mock.go          # deterministic test model
  filetype/filetype.go   # ext -> {type, grammar}
  config/config.go       # load pkb.toml / .pkb/config.toml + Default()
```

At runtime, `.pkb/state.json` (marker) and `.pkb/pkb.db` live at the repo root.

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

### CLI Usage

Run from anywhere inside the git repo (it discovers the repo root). No `<dbPath>`
argument — the db lives at `.pkb/pkb.db`.

```bash
pkb reindex            # sync the index with the target ref (default HEAD)
pkb search "<query>"   # search; -k N sets result count (default 5)
pkb stats              # print the marker (commit, indexedAt, file/chunk counts)
```

## Configuration

`pkb.toml` or `.pkb/config.toml` at the repo root selects a single `embedding`
model (`provider`/`model`/`dimensions`/`region`/`profile`) used for both code
and text, an optional `ref`, and optional `extOverrides` (ext -> "code"|"text").
Missing fields use defaults; `provider` is `bedrock` or `mock`. `.pkbignore`
filters paths.

## Dependencies

- `github.com/tree-sitter/go-tree-sitter` + per-grammar modules - code chunking
- `github.com/mattn/go-sqlite3` + `github.com/asg017/sqlite-vec-go-bindings` - storage + vector search
- `github.com/aws/aws-sdk-go-v2` (config + bedrockruntime) - Bedrock embeddings
- `github.com/stretchr/testify` - tests

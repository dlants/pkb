# PKB (git-repo-rooted code + docs search)

A semantic search index over the code and docs of a git repository. It sits at
the repo root, is refreshed out-of-band (commit hook / CI when code lands on the
default branch), and exposes a CLI for agents to ask broad orientation
questions. Written in Go; statically links SQLite + sqlite-vec and tree-sitter
grammars via cgo.

## Providers

PKB uses a pluggable **embedding** model (all files) and an optional
**inference** model (augments markdown/text chunks before embedding; code is
never augmented). Both are selected in `pkb.toml` via `[embedding]` /
`[inference]` blocks. Providers: `bedrock` (corporate default — Cohere embed-v4
+ Claude Haiku, IAM creds), `openai`/`openai-compatible` (OpenAI cloud or local
servers like Ollama via `baseurl`), `gemini`, `none` (inference only — disables
augmentation), and `mock` (tests). HTTP providers read the API key from the env
var named by `apikeyenv` (`OPENAI_API_KEY` / `GEMINI_API_KEY` defaults). See
README.md for the full config reference.

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

This repo dogfoods its own pkb setup. The `pkb` binary lives at the repo root
(`./pkb`, built with `go build -o pkb ./cmd/pkb`), and the index `pkb.db` is
checked into git, so search works out of the box. Reindexing is wired up via a
git hook — see `setup-hooks.sh` (installs hooks) and `hooks/pre-push`
(reindexes on push to the default branch and reminds you to commit `pkb.db`).

```bash
./pkb search "<natural language query>"   # -k N sets result count (default 5)
./pkb stats                               # index commit, indexedAt, file/chunk counts
./pkb reindex                             # sync the index against HEAD. No need to do this manually, it will happen on push
```

Each result is a scored snippet with its file path — treat it as a pointer and
open the file to read the real code. The index reflects the last indexed commit,
not your working tree; don't worry about reindexing local changes, the pre-push
hook handles it.

<system_reminder>
For exploratory / orientation questions about this codebase ("where is X
handled?", "how does Y work?"), use `./pkb search` rather than grepping with
`rg`. Reserve `rg` for exact symbol/string lookups.
</system_reminder>

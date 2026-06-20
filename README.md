# PKB — git-repo-rooted code + docs search

Semantic search helps agents orient themselves in a large codebase quickly. To achieve this, the semantic index doesn't need to be synchronized to the current state of the code - an index of the last commit to `main` lets the agent orient itself, and the agent can then read files to get the most up-to-date information on any changes.

PKB is a single file go binary that exposes a search CLI, with a very simple interface:

```bash
pkb reindex            # bring the index in sync with the current HEAD
pkb search "<query>"   # search; -k N controls result count (default 5)
pkb stats              # print the current index marker (commit, file/chunk counts)
```

`pkb reindex` is meant to be run at a single well-defined moment — when code lands on the default branch (typically a CI step). PKB keeps track of the last commit that was indexed (in `pkb-state.toml`), and uses git to identify changed files, then brings the index up to date with the current commit.

This will create a pkb.db sql file at the root of your repo. Check that into your repo. Now, everyone in your repo has access to the full index, while you only pay the embedding cost once.

`pkb.db` is a binary that is rewritten in full on every reindex, so storing it directly in git bloats history fast. Track it with [Git LFS](https://git-lfs.com) instead — the repo keeps a small pointer while the blob lives in LFS storage, and clones still get the file transparently:

```bash
git lfs install            # one-time per machine
git lfs track "pkb.db"     # writes the rule to .gitattributes
git add .gitattributes pkb.db
```

Commit `.gitattributes` along with `pkb.db`. If `pkb.db` is already in your history as a regular blob, rewrite it with `git lfs migrate import --include="pkb.db"`.

`reindex` is idempotent: running it twice with no git changes performs zero embedding calls.

## Keeping the index fresh

`pkb reindex` indexes the current `HEAD` and writes the indexed commit sha to `pkb-state.toml`. Because the refreshed `pkb.db` / `pkb-state.toml` then need to be committed, the index is always delivered one commit *behind* the code it represents — the index commit on `main` contains an index that reflects its parent. This lag is harmless: agents orient on the last indexed commit and read files for anything newer.

The recommended trigger is **CI on merge to the default branch**, not a client-side git hook (a `pre-push` hook can't add a commit to the push that's already in flight, and would clobber the Git LFS pre-push hook). This repo ships such a workflow at [`.github/workflows/pkb-index.yml`](.github/workflows/pkb-index.yml): on push to `main` it builds `pkb`, runs `pkb reindex`, and commits the refreshed index back. The follow-up commit only touches `pkb.db` / `pkb-state.toml` (both in `paths-ignore`, and `GITHUB_TOKEN` pushes don't trigger workflows), so it does not loop. It needs `VOYAGE_API_KEY` and `ANTHROPIC_API_KEY` as repository secrets.

Locally you can always run `pkb reindex` by hand and commit the result.

Run the pkb binary from anywhere inside the git repository. PKB discovers the repo root based on cwd, and runs against `pkb.db` at the repo root.

# Install

PKB uses cgo (it statically links SQLite + sqlite-vec and the tree-sitter grammars), so you need a C toolchain in addition to Go:

- **macOS:** `xcode-select --install` (Clang).
- **Debian/Ubuntu:** `sudo apt-get install build-essential`.
- **Fedora/RHEL:** `sudo dnf install gcc`.

Then install the binary onto your `$PATH` (it lands in `$GOBIN`, or `$(go env GOPATH)/bin`):

```bash
go install github.com/dlants/pkb/cmd/pkb@latest
```

Or, checkout this repo and build from source, then make the binary available in your PATH (or just copy + .gitignore it in your repo)

```bash
go build -o pkb ./cmd/pkb
```

# Configuration

PKB uses two models: an **embedding** model (embeds all files) and an optional
**inference** model (augments markdown/text chunks with a one-paragraph context
before embedding — the contextual-retrieval pattern; code files are never
augmented). Both are pluggable across providers.

The default configuration uses **Voyage AI** embeddings (`voyage-code-3`,
authenticated with `VOYAGE_API_KEY`) plus **Anthropic** Claude Haiku
augmentation (authenticated with `ANTHROPIC_API_KEY`).

A repo-root config file — `pkb.toml` or `.pkb/config.toml` (first found wins) —
selects the embedding and inference models. Any unset field falls back to
defaults, and a missing file uses defaults entirely.

PKB always indexes `HEAD`. Run `pkb reindex` when the default branch is checked
out (e.g. in CI after checkout) so the index tracks the default branch.

```toml
exclude = ["node_modules", "dist", "vendor/generated"]

[embedding]
provider = "voyage"
model = "voyage-code-3"
dimensions = 1024
apikeyenv = "VOYAGE_API_KEY"

[inference]
provider = "anthropic"
model = "claude-haiku-4-5"
apikeyenv = "ANTHROPIC_API_KEY"

[extOverrides]
".tsx" = "code"
```

- `provider`: the backend for a model block. Supported values:
  - `bedrock`: AWS Bedrock (Cohere embed-v4 for `[embedding]`, Claude for
    `[inference]`). Corporate default; uses IAM credentials, no extra keys.
  - `openai` / `openai-compatible`: any OpenAI-shaped server. Hits
    `{baseurl}/v1/embeddings` and `{baseurl}/v1/chat/completions`. Point
    `baseurl` at `https://api.openai.com` for OpenAI cloud, or at a local
    server (e.g. `http://localhost:11434` for Ollama, plus llama.cpp, vLLM,
    LM Studio, LocalAI) for a fully local setup.
    For a fully local setup on Apple Silicon (MLX-accelerated embeddings and
    a recommended local inference model with quantization, memory, and
    throughput guidance), see [LOCAL.md](LOCAL.md).
  - `gemini`: Google Generative Language API (good free all-rounder for mixed
    code + markdown).
  - `voyage` (embedding only): Voyage AI (`{baseurl}/v1/embeddings`). Default
    embedding provider; `voyage-code-3` is tuned for code retrieval. Chunks and
    queries are tagged with Voyage's `document`/`query` input types.
  - `anthropic` (inference only): Anthropic's native Messages API
    (`{baseurl}/v1/messages`). Default augmentation provider (Claude Haiku).
  - `none` (inference only): disables LLM augmentation, falling back to the
    deterministic heading-prefix path with no inference calls.
  - `mock`: deterministic, for tests.
- `baseurl`: API base URL for `openai`/`openai-compatible`/`gemini`/`voyage`/
  `anthropic` providers (defaults: `https://api.openai.com`,
  `https://generativelanguage.googleapis.com`, `https://api.voyageai.com`,
  `https://api.anthropic.com`). Ignored by Bedrock.
- `apikeyenv`: name of the environment variable holding the API key for HTTP
  providers (defaults: `OPENAI_API_KEY` for OpenAI, `GEMINI_API_KEY` for
  Gemini, `VOYAGE_API_KEY` for Voyage, `ANTHROPIC_API_KEY` for Anthropic). An
  empty key is tolerated for local servers like Ollama. Ignored by Bedrock.
- `awsregion`: AWS region for the Bedrock provider (defaults to `us-east-1`).
- `awsprofile`: AWS shared-config profile for the Bedrock provider; empty uses the
  default credential chain. Credentials are checked eagerly — if they're missing
  or expired, pkb exits with a hint to run `aws sso login`.
- `dimensions`: embedding width. `voyage-code-3` is Matryoshka, so it supports
  256/512/1024/2048; lower means smaller/faster with minor quality loss
  (default 256). Changing this re-keys the index; delete `pkb-state.toml` and
  run `pkb reindex` to rebuild.
- `extOverrides`: force an extension to `code` or `text`.
- `exclude`: paths to skip during indexing. Each entry matches a path either by
  basename (any file/dir with that name) or as a path prefix (a leading
  repo-relative path segment); full glob/gitignore semantics are not supported.
- `maxparallelism`: number of inference (augmentation) calls issued concurrently
  during indexing. Augmentation against a remote model is the slowest part of a
  run, so raising this speeds it up at the cost of more concurrent requests
  (default 4; values below 1 are treated as 1).

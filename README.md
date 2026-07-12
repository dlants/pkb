# PKB — git-repo-rooted code + docs search

Semantic search helps agents orient themselves in a large codebase quickly. To achieve this, the semantic index doesn't need to be synchronized to the current state of the code - an index of the last commit to `main` lets the agent orient itself, and the agent can then read files to get the most up-to-date information on any changes.

PKB is a single file go binary that exposes a search CLI, with a very simple interface:

```bash
pkb reindex            # bring the index in sync with the current HEAD
pkb estimate           # project the cost of the next and a full reindex (no API calls)
pkb search "<query>"   # search; -k N controls result count (default 5)
pkb stats              # print the current index marker (commit, file/chunk counts)
pkb chunk <file>       # display how this file would be chunked (for debugging)
```

`pkb reindex` is meant to be run at a single well-defined moment — the recommended default is a **pre-commit hook**, so the refreshed index rides along in the same commit as your code. PKB keeps track of the last commit that was indexed (in `pkb-state.toml`), and uses git to identify changed files, then brings the index up to date with the current commit.

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

### Recommended: pre-commit hook

The recommended default is a **pre-commit hook** that refreshes the index _in the same commit_ as your code. Reindexing is fast and driven purely by per-file git blob shas, so this adds negligible overhead. `pkb reindex --staged` indexes the staging area (`git write-tree`) instead of `HEAD`, so it sees exactly the content the commit will contain — with no separate index commit and no lag between the code and the index that represents it.

This repo ships a working hook at [`hooks/pre-commit`](hooks/pre-commit): it runs `pkb reindex --staged` and then `git add pkb.db pkb-state.toml` so the refreshed index rides along in the same commit. Install it by pointing git at the `hooks/` directory (this repo already does so):

```
git config core.hooksPath hooks
```

LFS caveat: `pkb.db` is LFS-tracked, so the `git add` in the hook stages the small LFS pointer (not the blob) — this is fine and does not fight the LFS `pre-push` hook. Make sure `git lfs install` has been run in your clone so the blob uploads on `git push`.

### Alternative: manual reindex

If you'd rather not run a hook, run `pkb reindex` by hand whenever code lands on the default branch and commit the refreshed `pkb.db` / `pkb-state.toml`. Because those files then need their own commit, the index is delivered one commit _behind_ the code it represents — the index commit on `main` reflects its parent. This lag is harmless: agents orient on the last indexed commit and read files for anything newer. Make sure `git lfs install` has been run in your clone (see the Git LFS setup above) so the blob uploads on `git push`.

Avoid a `pre-push` hook for this: it can't add a commit to the push that's already in flight and would clobber the Git LFS pre-push hook.

See these files in this repo for a complete working configuration:

- [`hooks/pre-commit`](hooks/pre-commit) — the recommended pre-commit reindex hook.
- [`pkb.toml`](pkb.toml) — embedding model configuration (Voyage).
- [`.gitattributes`](.gitattributes) — the Git LFS tracking rule for `pkb.db`.
- [`pkb-state.toml`](pkb-state.toml) — the tracked index marker (indexed commit sha + file/chunk counts).

Run the pkb binary from anywhere inside the git repository. PKB discovers the repo root based on cwd, and runs against `pkb.db` at the repo root.

# Install

PKB uses cgo (it statically links SQLite + sqlite-vec and the tree-sitter grammars), so you need a C toolchain in addition to Go:

- **macOS:** `xcode-select --install` (Clang).
- **Debian/Ubuntu:** `sudo apt-get install build-essential`.
- **Fedora/RHEL:** `sudo dnf install gcc`.

Then install the binary onto your `$PATH` (it lands in `$GOBIN`, or `$(go env GOPATH)/bin`):

```bash
go install github.com/dlants/pkb@latest
```

Or, checkout this repo and build from source, then make the binary available in your PATH (or just copy + .gitignore it in your repo)

```bash
go build -o pkb .
```

# Configuration

PKB uses a single **embedding** model to embed all files. Only providers that support contextualized whole-document embeddings are supported for text, which today means **Voyage** (`voyage-context-*`). A deterministic `mock` model is available for tests.

- **Text files** are sent whole (or in ~120K-token windows with 10K-token overlap for large files) to Voyage's contextualized-embeddings endpoint with auto-chunking on; Voyage picks the chunk boundaries and injects document context into each vector.
- **Code files** are chunked along AST boundaries with breadcrumb heading context and embedded with the same Voyage model.

The default configuration uses **Voyage AI** embeddings (`voyage-context-3`, authenticated with `VOYAGE_API_KEY`).

A repo-root config file — `pkb.toml` or `.pkb/config.toml` (first found wins) — selects the embedding model. Any unset field falls back to defaults, and a missing file uses defaults entirely.

PKB indexes `HEAD` by default, or the staging area with `--staged` (used by the recommended pre-commit hook). See [Keeping the index fresh](#keeping-the-index-fresh).

```toml
exclude = ["node_modules", "dist", "vendor/generated"]

[embedding]
provider = "voyage"
model = "voyage-context-4"
dimensions = 256
apikeyenv = "VOYAGE_API_KEY"

[extOverrides]
".tsx" = "code"
```

- `provider`: the embedding backend. Supported values:
  - `voyage`: Voyage AI (`{baseurl}/v1/contextualizedembeddings`). Default (and only) real provider; the configured model must be a contextual model (`voyage-context-*`). Chunks and queries are tagged with Voyage's `document`/`query` input types.
  - `mock`: deterministic, for tests.
- `model`: the embedding model id. Must implement contextual whole-document embeddings — `pkb` fails fast at startup otherwise.
- `baseurl`: API base URL for the Voyage endpoint (defaults to `https://api.voyageai.com`).
- `apikeyenv`: name of the environment variable holding the API key (defaults to `VOYAGE_API_KEY`).
- `dimensions`: embedding width. `voyage-context-*` models are Matryoshka, so they support 256/512/1024/2048; lower means smaller/faster with minor quality loss (default 256). Changing this re-keys the index; delete `pkb-state.toml` and run `pkb reindex` to rebuild.
- `extOverrides`: force an extension to `code` or `text`.
- `exclude`: paths to skip during indexing. Each entry matches a path either by basename (any file/dir with that name) or as a path prefix (a leading repo-relative path segment); full glob/gitignore semantics are not supported.
- `maxReindexCost`: cap, in US dollars, on the projected cost of a single reindex run. Before any paid embedding work, `reindex` estimates the run's cost from the changed files and per-chunk reuse (no API calls) and aborts when the estimate exceeds this per-run cap, so an unexpectedly large change set must be reindexed locally instead. The projected cost is printed on every run (default $5; a non-positive value disables the gate). `pkb reindex --max-reindex-cost <dollars>` overrides the configured value for a single run, and `pkb estimate` prints the same projection without spending anything.

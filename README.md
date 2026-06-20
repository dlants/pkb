# PKB — git-repo-rooted code + docs search

Semantic search helps agents orient themselves in a large codebase quickly. To achieve this, the semantic index doesn't need to be synchronized to the current state of the code - an index of the last commit to `main` lets the agent orient itself, and the agent can then read files to get the most up-to-date information on any changes.

PKB is a single file go binary that exposes a search CLI, with a very simple interface:

```bash
pkb reindex            # bring the index in sync with the current HEAD
pkb search "<query>"   # search; -k N controls result count (default 5)
pkb stats              # print the current index marker (commit, file/chunk counts)
```

`pkb reindex` is meant to be run at a single well-defined moment — when code lands on the default branch (via a git hook or CI step). PKB keeps track of the last commit that was indexed, and uses git to identify changed files, then brings the index up to date with the current commit.

This will create a pkb.db sql file at the root of your repo. Check that into your repo. Now, everyone in your repo has access to the full index, while you only pay the embedding cost once.

`reindex` is idempotent: running it twice with no git changes performs zero embedding calls.

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

## Configuration

At runtime PKB needs AWS credentials with Bedrock access for the configured embedding models.

A repo-root config file — `pkb.toml` or `.pkb/config.toml` (first found wins) —
selects the embedding model. Any unset field falls back to defaults, and a
missing file uses defaults entirely.

PKB always indexes `HEAD`. Run `pkb reindex` when the default branch is checked
out (e.g. in CI after checkout) so the index tracks the default branch.

```toml
exclude = ["node_modules", "dist", "vendor/generated"]

[embedding]
provider = "bedrock"
model = "us.cohere.embed-v4:0"
dimensions = 256
awsregion = "us-east-1"
awsprofile = "my-sso-profile"

[extOverrides]
".tsx" = "code"
```

- `provider`: `bedrock` (Cohere on AWS Bedrock) or `mock` (deterministic, for tests).
- `awsregion`: AWS region for the Bedrock provider (defaults to `us-east-1`).
- `awsprofile`: AWS shared-config profile for the Bedrock provider; empty uses the
  default credential chain. Credentials are checked eagerly — if they're missing
  or expired, pkb exits with a hint to run `aws sso login`.
- `dimensions`: embedding width. embed-v4 is Matryoshka, so it supports
  256/512/1024/1536; lower means smaller/faster with minor quality loss
  (default 256). Changing this re-keys the index; delete `.pkb/state.json` and
  run `pkb reindex` to rebuild.
- `extOverrides`: force an extension to `code` or `text`.
- `exclude`: paths to skip during indexing. Each entry matches a path either by
  basename (any file/dir with that name) or as a path prefix (a leading
  repo-relative path segment); full glob/gitignore semantics are not supported.

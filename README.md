# PKB — git-repo-rooted code + docs search

PKB builds a semantic search index over the code and docs in a git repository.
It is meant to sit at the root of a repo, be refreshed at a single well-defined
moment — when code lands on the default branch (via a commit hook or CI step) —
and expose a CLI that an agent can use to answer broad orientation questions
("how do we handle auth here?") against the state of the repo in dev.

It is **not** a live view of the working tree: there is no watcher. Reindexing
happens only when you run `pkb reindex`.

## How it works

- **Git is the source of truth.** PKB records the commit it was last indexed
  against in `.pkb/state.json` and asks `git diff` for the delta to the target
  ref. `.gitignore` is honored for free (git only tracks non-ignored files);
  `.pkbignore` (gitignore-ish, segment/prefix matching) filters further.
- **Two embedding models.** Code files are chunked along syntactic boundaries
  with tree-sitter and embedded by a *code* model; markdown/text files use a
  structural chunker and a *text* model. Each file is embedded by exactly one
  model, and chunks live in that model's vector table.
- **Single binary.** PKB is a Go binary that statically links SQLite +
  sqlite-vec and the tree-sitter grammars (cgo). No node/npm runtime.

## Build

```bash
go build -o pkb ./cmd/pkb
```

Requires a C toolchain (cgo) and AWS credentials with Bedrock access for the
configured embedding models.

## Usage

Run from anywhere inside the git repository. PKB discovers the repo root, reads
`.pkb.json` / `.pkb/config.json` and `.pkbignore`, and stores the index at
`pkb.db` at the repo root.

```bash
pkb reindex            # bring the index in sync with the target ref (default HEAD)
pkb search "<query>"   # search; -k N controls result count (default 5)
pkb stats              # print the current index marker (commit, file/chunk counts)
```

`reindex` is idempotent: running it twice with no git changes performs zero
embedding calls.

### Search output contract

`search` prints score-ordered markdown sections to stdout, one per result:

```
## Result 1 (score: 0.873)
File: path/to/file.go

<chunk text>

---

## Result 2 (score: 0.812)
...
```

If there are no matches it prints `No results found.`. Consumers (skills,
agent integrations) can parse these sections or feed the raw output to an LLM.

## Configuration

A repo-root config file — `pkb.toml` or `.pkb/config.toml` (first found wins) —
selects the two embedding models and the target ref. Any unset field falls back
to defaults, and a missing file uses defaults entirely.
```toml
ref = "HEAD"

[codeEmbedding]
provider = "bedrock"
model = "us.cohere.embed-v4:0"
dimensions = 1536
region = "us-east-1"
profile = "my-sso-profile"

[textEmbedding]
provider = "bedrock"
model = "us.cohere.embed-v4:0"
dimensions = 1536

[extOverrides]
".tsx" = "code"
```
- `provider`: `bedrock` (Cohere on AWS Bedrock) or `mock` (deterministic, for tests).
- `region`: AWS region for the Bedrock provider (defaults to `us-east-1`).
- `profile`: AWS shared-config profile for the Bedrock provider; empty uses the
  default credential chain. Credentials are checked eagerly — if they're missing
  or expired, pkb exits with a hint to run `aws sso login`.
- `ref`: git ref to index; defaults to `HEAD` (the default branch in CI).
- `extOverrides`: force an extension to `code` or `text`.
`.pkbignore` is a separate gitignore-style file at the repo root.

## Refreshing on merge (commit hook / CI)

Reindex at the moment code lands on the default branch. A minimal CI step:

```bash
# after checkout of the default branch
go build -o pkb ./cmd/pkb
./pkb reindex
# commit the refreshed index so consumers get it on pull
git add pkb.db
git commit -m "pkb: reindex" || true
```

Because the marker is written only after the DB transaction commits, a crash
mid-run leaves the marker stale (behind the DB), never ahead — the next run
re-diffs from the older commit and reprocesses already-current files harmlessly.

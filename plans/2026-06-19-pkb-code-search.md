# Objective and Context

## The user's request (verbatim)

> been thinking about this. Things like lumen aren't really meant to provide a "live" view of the code. Really they're there to help the agent be oriented within the code base, and to be combined with other tools (agentic search) to find relevant bits of code faster, or answer broad questions like "how do we handle auth here?". So for example, lumen reindexing doesn't really try to follow the "live" state of the codebase. There's a delay of a few minutes before things get reindexed.
>
> So we could use a fancier code embedding model, like one of the ones available to us via bedrock, and just embed the sqlite db / skill inside the aurelia repo. We can hook up the reindexing to happen at the moment code lands in dev. We pay the cost of reindexing once per file, org-wide... and then everyone gets a skill (or a cli, which they can hook into their agent of choice) that allows their agent to search over "what's in dev" that's updated every time you pull from origin...
>
> I'd like to turn pkb into this kind of tool.
>
> - add treesitter-based chunking for coding files, and embedding via a coding embedding model
> - extend the configuration to be able to configure a coding embedding model, and a text embedding model
> - get rid of the location tracking jank. The design is that it's meant to sit at the root of a git repo, and update when files change in git. So we can rely on git commits / file content tracking instead of chunk content tracking.
> - we rely on .gitignore and .pkbignore configuration files to decide what to reindex vs not.
>
> no watch mode. Reindexing only happens when you manually trigger, meant to happen either via a commit hook or a ci step, when something is merged into the default branch

## What we're building, in our own words

We are repurposing PKB from a "track arbitrary files/dirs and poll for changes" knowledge base into a **git-repo-rooted code+docs search index**. The index is meant to be committed (or rebuilt) at the root of a repo and refreshed at a single well-defined moment — when code lands on the default branch — via a commit hook or CI step, not via a live file watcher. Consumers get a CLI that lets their agent ask broad orientation questions ("how do we handle auth here?") and get back relevant chunks of the code as it exists in dev. We expose only the CLI; turning it into a skill, plugin, or agent integration is left to the consumer and is out of scope for this repo.


Three substantive changes:

1. **Tree-sitter chunking for code.** Markdown keeps its existing structural chunker; code files are chunked along syntactic boundaries (functions, classes, methods) instead of being treated as prose.
2. **Two configurable embedding models.** A *code* embedding model and a *text* embedding model. The file's type decides which model embeds it.
3. **Git as the source of truth for file discovery and change detection.** No tracked-source/exclusion tables, no mtime/md5 chunk tracking, no polling. The set of files and their content hashes come straight from git; `.gitignore` is honored implicitly (git only knows about tracked files) and `.pkbignore` filters further.

## Implementation language: Go (cgo)

PKB is being rewritten from TypeScript to **Go** for this redesign. The CLI-only consumer contract makes the implementation language invisible to consumers, so we pick what ships best for an out-of-band reindex job: a single statically-linked binary with no runtime (node/npm) and grammars baked in. cgo is the accepted tax.

Dependency stack (all confirmed current and mature):
- **Tree-sitter:** `github.com/tree-sitter/go-tree-sitter` (official binding) + one Go module per grammar (e.g. `github.com/tree-sitter/tree-sitter-typescript/bindings/go`). Grammars are cgo deps compiled *into the binary* at `go build` time — no vendored `.wasm`, no runtime loading, no ABI matching. The grammar registry is a plain `map[string]func() *tree_sitter.Language`.
- **SQLite + vectors:** `github.com/mattn/go-sqlite3` (cgo) + `github.com/asg017/sqlite-vec-go-bindings/cgo`, which statically links sqlite-vec into the binary. Call `sqlite_vec.Auto()` before opening connections; everything goes through stdlib `database/sql`. (macOS emits a harmless `sqlite3_auto_extension` deprecation warning; build succeeds.)
- **LLM context augmentation (Haiku 4.5 via Bedrock):** official `github.com/anthropics/anthropic-sdk-go` + its `bedrock/` middleware (`bedrock.WithLoadDefaultConfig`). Keep manual per-block `cache_control` — Bedrock does not support automatic (top-level) caching, only explicit per-block, which is what the current TS code does.
- **Embeddings (Cohere/Titan):** no Anthropic embedding model exists, so embeddings call Bedrock directly via `github.com/aws/aws-sdk-go-v2/service/bedrockruntime` (`InvokeModel`). Both SDKs read the same AWS default credential chain, so auth is unified.
- **CLI:** stdlib `flag` (three commands; cobra is overkill).
- **Tests:** stdlib `testing` (table-driven) + `github.com/stretchr/testify` for assertions; a git-repo test harness creates temp repos.

The existing TypeScript tree stays in place during the port (for reference) and is deleted once the Go implementation reaches parity at Stage 4.

## Key types and entities

- `EmbeddingModel` (`internal/embed`) — interface (`ModelName()`, `Dimensions()`, `EmbedChunk/EmbedQuery/EmbedChunks`). The context holds **two** instances (code + text).
- `ChunkInfo` (`internal/chunk`) — `{ Text string; HeadingContext string; Start, End Position }`. Both the markdown and the tree-sitter chunker produce this same shape, so the embedding/storage path stays uniform.
- `Context` (`internal/...`) — holds a code model + text model + repo root + ref (replacing the single embedding model).
- A `FileType` enum (`code | text`) plus a routing function `ext -> { Type, Grammar }`.
- Git-derived `RepoFile = { Path RelPath; BlobSha string }` replacing tracked-source/mtime+hash bookkeeping.

## Relevant files (Go package layout)

```
cmd/pkb/main.go          # CLI entry (stdlib flag): reindex, search, stats
internal/
  git/git.go             # repo root, ref sha, ls-tree, diff --name-status, cat-file -e, read content
  index/manager.go       # change detection (incremental/divergence/full) + reindex flow (no watcher)
  store/store.go         # sqlite + sqlite-vec: vec0 tables per (modelName,version), files table, CRUD
  chunk/chunk.go         # ChunkInfo + Position types
  chunk/markdown.go      # markdown chunker (port of chunker.ts)
  chunk/code.go          # tree-sitter code chunker (chunkCode)
  chunk/grammars.go      # ext -> tree_sitter.Language registry
  embed/embed.go         # EmbeddingModel interface + router
  embed/bedrock.go       # Bedrock InvokeModel embeddings (aws-sdk-go-v2)
  embed/mock.go          # test double
  llm/llm.go             # Haiku-via-Bedrock context augmentation (anthropic-sdk-go + bedrock) + mock
  filetype/filetype.go   # ext -> {type, grammar}
  config/config.go       # load .pkb config (.pkb.json / .pkb/config.json) + .pkbignore
```
At runtime, `.pkb/state.json` (marker) and `.pkb/pkb.db` live at the repo root.

# Design

## Git as source of truth

PKB records a single marker — the **commit sha it was last successfully indexed against** — in a plaintext file at the repo root, and figures out work by asking git for the diff between that commit and the target ref. This is safe specifically because reindexing is an out-of-band job (pre-commit hook / CI on the default branch), not something individual users run ad hoc: if a DB is tagged with a commit sha, it's because a reindex run completed successfully and the resulting DB was checked in. So the stored sha is a trustworthy "everything up to here is indexed" pointer.

State stored in the DB:
- The last-indexed commit sha lives in a plaintext file at the repo root, `.pkb/state.json` (`{ commit, indexedAt, fileCount, chunkCount }`) — *not* in the DB. This is the whole "how far have we indexed" answer, and keeping it as text means each reindex commit shows a readable `commit: X → Y` diff in git (the DB itself is an opaque binary blob).
- `files` rows keyed by relative path (recording which model embedded each file so deletes hit the right vec table). Each row also stores a **per-file content identifier** (the git blob sha; an md5 of contents works equally well). The normal incremental path never consults it — change detection is driven entirely by the overall commit sha + `git diff`. Its sole purpose is recovery: it lets the full-rebuild / merge-base paths skip files whose content is genuinely unchanged, and lets us self-verify or rebuild the marker by reconciling the DB against the tree if the marker file is lost or distrusted. Keeping it is cheap insurance even though the happy path ignores it.

Determining what to (re)index:
- **Incremental (normal case):** `git diff --name-status <state.commit> <ref>` gives exactly the changed paths. Map status codes to operations:
  - `A` → index (new)
  - `M` → reindex (delete chunks, re-embed)
  - `D` → delete
  - `R` (rename) → delete old path + index new path
  - `C` (copy) → index new path
- **Divergence (force-push / rebase):** if the stored commit `S` is *not* an ancestor of the target ref `C` (`git merge-base --is-ancestor S C` fails) but both objects are still present locally, history has been rewritten rather than extended. Establish the last common ancestor `A = git merge-base S C`, then build the reindex set from the **union of `git diff --name-status A S` and `git diff --name-status A C`**. The first set covers files the DB indexed on the now-abandoned branch (so they get re-aligned to the truth), the second covers files changed on the new history; together they're a superset of the true `S → C` delta, and blob-sha equality short-circuits any path that didn't actually change. This avoids a full rebuild on the common force-push case.
- **Full (cold start / total recovery):** if `.pkb/state.json` is missing, or the stored commit object is gone entirely (GC'd / shallow clone, so no merge-base can be computed — detected via `git cat-file -e <sha>`), fall back to `git ls-tree -r <ref>` and index everything, using the stored per-file blob shas to skip files whose content is unchanged.

`<ref>` defaults to `HEAD` (the default branch when run in CI). File contents are read from the working tree at the checked-out ref.

`.pkbignore` (gitignore-syntax, at repo root) filters the candidate paths from either the diff or the full listing. `.gitignore` is honored for free because git only tracks non-ignored files. Keep matching simple initially: prefix/segment matching like the current `isExcluded`, or a small glob matcher — full gitignore semantics are out of scope for v1.

Reindex flow (replaces the polling loop):
1. Resolve repo root + ref + config + `.pkbignore`.
2. Read `.pkb/state.json`; decide incremental vs. divergence vs. full.
3. Produce candidate operations (from `git diff` or full `ls-tree`); drop ignored + unsupported-extension paths.
4. Process operations sequentially (existing queue mechanism is fine; it just no longer runs on a timer).
5. **Only after the DB transaction commits**, write the target ref's commit sha to `.pkb/state.json` as the final step. Ordering matters: the marker file is always written *after* the DB is durable, so a crash in between leaves the file *stale* (pointing at an older commit), never ahead. A stale marker just causes the next run to re-diff from an older commit and reprocess already-current files — harmless, because blob-sha equality short-circuits the embed. A marker that was ahead of the DB would be dangerous (skipped work), and this ordering makes that impossible.

## File-type routing

`filetype.Route(ext)` returns `{ Type FileType; Grammar string }`. Markdown / `.txt` / unknown-but-texty -> `text`. Recognized source extensions -> `code` with the grammar name to load. The router also picks the embedding model: `Type == code` -> code model, else text model. A file is always embedded by exactly one model, so its `files` row records `model_name`, and its chunks live in that model's vec table.

## Tree-sitter chunker

`chunk.ChunkCode(content []byte, grammar string) ([]ChunkInfo, error)` mirrors the markdown chunker's output shape so downstream code is unchanged.

- Use the official `github.com/tree-sitter/go-tree-sitter` binding. Each grammar is a Go module dependency (e.g. `tree-sitter-typescript/bindings/go`) compiled into the binary via cgo — no `.wasm` files, no runtime loading, no ABI matching. `chunk/grammars.go` holds a `map[string]func() *tree_sitter.Language` registry keyed by grammar name; the curated language set is the set of grammar modules we depend on.
MARK

select <<MARK
  - A leaf declaration still over budget falls back to line-based splitting (reuse `splitCodeBlockByLines`/`splitByCharacters` from `chunker.ts`).
MARK
replace <<BODY
  - A leaf declaration still over budget falls back to line-based splitting (shared line/char split helpers in `chunk`, ported from `chunker.ts`).
- Algorithm: parse to a syntax tree; walk top-level *named declarations* (functions, classes, methods, etc.). Each declaration is a candidate chunk.
  - If a declaration exceeds the target size, recurse into its named children (e.g. methods of a class) so a class becomes one chunk per method when it's too big.
  - A leaf declaration still over budget falls back to line-based splitting (reuse `splitCodeBlockByLines`/`splitByCharacters` from `chunker.ts`).
  - Gap/top-level code between declarations (imports, constants) is grouped into "filler" chunks by line ranges.
- `HeadingContext` carries **structural context**: the file's relative path plus the enclosing symbol path (e.g. `path/to/file.ts > class Foo > method bar`). This orients retrieval, analogous to markdown heading breadcrumbs.
- Positions (`Start`/`End` line/col) come directly from tree-sitter node ranges (`node.StartPosition()` / `node.EndPosition()`).

`ChunkCode` is a pure function over `(content, grammar)`; `Language` construction is cheap (the grammar is statically linked), and a `*tree_sitter.Parser` can be reused per call.

## Context generation for code

The Anthropic-context-generation step (`llm` package, Haiku 4.5 via Bedrock through `anthropic-sdk-go`) is valuable for prose but expensive and less useful for code, where the structural breadcrumb already gives strong context. Apply LLM context generation to `text` files only by default; for `code`, the `HeadingContext` breadcrumb is the sole prepended context. Keep the LLM optional and configurable so this can be revisited.

## Configuration

A repo-root config file (`.pkb.json` or `.pkb/config.json`) specifies:
- `codeEmbedding` and `textEmbedding` model options (a tagged struct selecting the Bedrock model + dimensions for each).
- `ref` / default branch (optional; default `HEAD`).
- optional extension routing overrides.

`.pkbignore` is a separate gitignore-style file. The db lives at a fixed repo-relative path (e.g. `.pkb/pkb.db`), so the CLI no longer takes a `<dbPath>` argument — it discovers the repo root from cwd.

## Invariants

- After a successful `reindex`, the DB reflects exactly the git tree at the indexed ref, and `.pkb/state.json`'s commit equals that ref's commit sha.
- The marker file is written only after the DB transaction commits and every operation succeeds; it can be stale (behind the DB) but never ahead. A stale marker only causes harmless reprocessing; an aborted run never skips work.
- `reindex` is idempotent: running it twice with no git changes performs zero embedding calls (empty diff).
- Each file is embedded by exactly one model (determined by its type); its chunks live only in that model's vec table.
- Vec tables remain keyed by `(modelName, version)`. Changing a configured model creates a new table; old tables become orphaned and are cleaned up rather than silently mixed.
- Chunk shape (`ChunkInfo`) is identical regardless of chunker, so storage/search are agnostic to source language.
- No process stays resident: every command runs to completion and exits (no timers, no watchers).

## Alternatives considered

- **Per-file set-diff vs. single commit sha + `git diff`.** Chose the commit-sha marker (stored in `.pkb/state.json`, asking git for the delta to the target ref). This is sound because reindexing is an out-of-band CI/hook job whose successful output (the DB) is checked in, so the stored sha is a trustworthy "indexed up to here" pointer. We keep per-file blob shas only to make the full-rebuild/recovery path idempotent. A pure per-file set-diff (list whole tree every run, compare each blob sha) was the simpler-but-slower alternative; it survives as the fallback when the stored commit is unreachable.
- **WASM (`web-tree-sitter`) vs cgo (`go-tree-sitter`).** Originally the plan favored WASM for portability and ABI-pinning when this was a TS tool. With the Go rewrite, grammars become cgo Go-module dependencies compiled straight into a single static binary at `go build` time — eliminating runtime grammar loading, vendored `.wasm` blobs, and ABI matching entirely, while parsing natively (faster). cgo requires a C toolchain to build, which is acceptable for a tool already statically linking sqlite-vec.
- **TypeScript vs Go.** Rewrote to Go: the CLI-only consumer contract hides the implementation language, so we optimize the tool's own distribution — a single static binary, no node runtime, grammars baked in — which fits an out-of-band CI/hook reindex job.
- **Keeping per-chunk text dedup.** Dropped for code: structural edits reshuffle chunks, so reuse rarely hits. On a changed file we delete its chunks and re-embed wholesale, which is simpler and correct. (Can be reintroduced as an optimization later.)

# Stages

## Stage 0 — Go module scaffolding  ✅ DONE

Decisions/notes:
- Module path: `github.com/dlants/pkb`.
- cgo sqlite-vec linking requires a blank import of `github.com/mattn/go-sqlite3`
  (it compiles the sqlite3 amalgamation the sqlite-vec C code links against).
  Without it, link fails with undefined `_sqlite3_*` symbols.
- `import "C"` is not allowed in `_test.go` files; cgo is pulled in transitively
  via the mattn blank import instead, so the smoke test needs no `import "C"`.
- Core types live in `internal/chunk` (ChunkInfo/Position), `internal/embed`
  (EmbeddingModel, Embedding=[]float32), `internal/filetype` (FileType enum +
  RouteExt/RoutePath). Smoke test in `internal/smoke`.
- macOS emits the expected harmless `sqlite3_auto_extension` deprecation warning;
  build/vet/test all pass (exit 0).

- Goal: a buildable Go module with the package layout above, the dependency set wired, and a smoke test proving cgo links (sqlite-vec + one tree-sitter grammar).
- Work: `go mod init`; add deps (`go-tree-sitter` + grammars, `mattn/go-sqlite3`, `sqlite-vec-go-bindings/cgo`, `anthropic-sdk-go`, `aws-sdk-go-v2`, `testify`); `cmd/pkb/main.go` flag skeleton (`reindex`/`search`/`stats` stubs); define `chunk.ChunkInfo`/`Position`, `embed.EmbeddingModel`, `filetype` enum; commit a `.gitignore` for `.pkb/`.
- Verification:
  - `go build ./...` succeeds (confirms cgo toolchain links sqlite-vec).
  - A smoke test opens an in-memory DB, runs `select vec_version()`, and parses a trivial source string with one grammar — both succeed.
- Before moving on: confirm `go build ./...`, `go vet ./...`, and `go test ./...` all pass.

## Stage 1 — Git-driven discovery & change detection (markdown only, single model)

- Goal: `reindex` indexes/updates/deletes files based on the git diff between the marker commit (`.pkb/state.json`) and the target ref (with a full `ls-tree` cold-start/recovery path) + `.pkbignore`, with no tracked-source tables and no watcher. Markdown chunker and a single embedding model are still in use, so behavior is verifiable end-to-end before code support lands.
- Work: `internal/git` (resolve repo root + ref sha, `ls-tree -r` listing, `diff --name-status` between commits, `cat-file -e` reachability check, read file content from working tree); `internal/store` schema with `files` keyed by (relativePath, blobSha, model) + vec0 tables, no tracked-source tables; `internal/index` builds operations from the diff (or full listing) and writes `.pkb/state.json` only after the DB transaction commits and the run succeeds; no timer/watcher; extend the test harness to create temp git repos.
- Verification:
  - Behavior: cold start indexes everything. Setup: temp git repo (init, commit `.md` files) via the test harness extended to create a git repo; mock embedding model; no `.pkb/state.json`. Actions: `reindex`. Expected: all files indexed; marker commit == HEAD sha.
  - Behavior: no-op when nothing changed. Actions: `reindex` twice. Expected: empty diff, zero embed calls on the second run (assert via mock call count).
  - Behavior: incremental add/modify/delete via diff. Actions: commit an added file, a modified file, and a `git rm`; `reindex`. Expected: only those three files touched; marker advances to the new HEAD.
  - Behavior: rename handled. Actions: `git mv` + commit, `reindex`. Expected: old path's rows purged, new path indexed.
  - Behavior: partial-run safety. Setup: force an embed failure midway. Actions: `reindex` (fails), fix, `reindex`. Expected: marker file unchanged (or absent) after the failed run; the retry completes all pending work.
  - Behavior: force-push/divergence via merge-base. Setup: index commit `S`, then rewrite history so the target ref `C` shares ancestor `A` with `S` but `S` is not an ancestor of `C` (both objects still present). Actions: `reindex`. Expected: reindex set = union of diffs `A..S` and `A..C`; only genuinely-changed files re-embed (blob-sha short-circuits the rest); marker advances to `C`.
  - Behavior: total recovery when stored commit object is gone. Setup: set the marker commit to a sha that fails `git cat-file -e`. Actions: `reindex`. Expected: falls back to full `ls-tree`, blob-sha equality skips unchanged files, marker reset to HEAD.
  - Behavior: `.pkbignore` excludes a path. Setup: `.pkbignore` listing a dir. Expected: matching files never indexed.
- Before moving on: confirm `go build ./...`, `go vet ./...`, and `go test ./...` all pass.

## Stage 2 — Two embedding models + file-type routing (still markdown chunker for text; code files embedded as plain text via code model)

- Goal: context holds a code model and a text model; `file-type.ts` routes each file to a type and model; config selects both models. Code files are *discovered and embedded by the code model* (chunked naively for now) so the routing + dual-vec-table path is proven before the tree-sitter chunker exists.
- Work: `internal/config` loads both model configs; the context builds a code model + text model; `internal/filetype` routes each file; `store`/`index` route indexing + search by model; ensure two vec0 tables coexist and orphaned tables are cleaned up.
- Verification:
  - Behavior: a `.md` and a `.ts` file land in different vec tables. Setup: distinct mock models with different `modelName`/dimensions. Expected: each file's chunks in its model's table; counts correct.
  - Behavior: search uses the matching model. (For v1, decide search strategy — query both tables and merge by score, or accept a type hint. Plan assumes: query both tables with the respective query embeddings and merge by score.) Expected: results include chunks from both, ordered by score.
  - Behavior: changing a configured model name creates a new table and the old one is cleaned up. Expected: orphan cleanup removes stale table/rows.
- Before moving on: confirm `go build ./...`, `go vet ./...`, and `go test ./...` all pass.

## Stage 3 — Tree-sitter code chunker

- Goal: code files are chunked along syntactic boundaries with structural breadcrumbs, replacing naive chunking for recognized languages.
- Work: add grammar module deps + `chunk/grammars.go` registry (`map[string]func() *tree_sitter.Language`); implement `chunk.ChunkCode` producing `[]ChunkInfo` with symbol-path `HeadingContext`; wire routing so `code` files use `ChunkCode`; restrict LLM context generation to `text` files.
- Verification:
  - Behavior: a file with two functions yields one chunk per function with correct line ranges. Setup: small fixture in one language (e.g. TS). Actions: `chunkCode`. Expected: chunk count + positions match; `headingContext` contains file path + symbol name.
  - Behavior: an oversized class splits into per-method chunks. Expected: methods become separate chunks, each breadcrumbed `... > class X > method Y`.
  - Behavior: an oversized single function falls back to line-splitting. Expected: multiple chunks covering contiguous line ranges, no gaps/overlaps beyond configured overlap.
  - Behavior: top-level filler (imports/consts) is captured. Expected: a chunk covering the leading non-declaration lines.
  - Behavior: unsupported/parse-error file degrades gracefully. Expected: falls back to line-based chunking rather than throwing.
- Before moving on: confirm `go build ./...`, `go vet ./...`, and `go test ./...` all pass.

## Stage 4 — CLI and config surface

- Goal: the CLI matches the new model: run from a repo root, no `<dbPath>` arg, commands `reindex`, `search`, `stats`; reads `.pkb` config + `.pkbignore`; db at fixed repo-relative path. Provide CLI usage docs and a sample commit-hook/CI invocation. No skill/plugin is shipped from this repo — consumers wrap the CLI however they like.
- Work: finalize `cmd/pkb` command surface (`reindex`, `search`, `stats`); repo-root + config discovery from cwd; fixed db path; document the `search` output contract for consumers; update `README.md`/`context.md`; delete the legacy TypeScript tree once Go reaches parity.
- Verification:
  - Behavior: `reindex` from repo root indexes the repo end-to-end (integration, mock models). Expected: stats report expected file/chunk counts.
  - Behavior: `search "<query>"` returns formatted results across code+text. Expected: non-empty, score-ordered output.
  - Behavior: missing/partial config falls back to sensible defaults (or errors clearly). Expected: defined behavior, covered by a test.
- Before moving on: confirm `go build ./...`, `go vet ./...`, and `go test ./...` all pass.

# Out of scope for v1

- Full `.gitignore`/`.pkbignore` glob semantics (negation, `**`, anchoring nuances) — start with simple segment/prefix matching.
- Incremental per-chunk reuse on changed files — re-embed the whole file.
- Indexing a ref that isn't checked out via `git cat-file` — read from the working tree initially.
- A standalone full-rebuild command — recovery is automatic when the stored commit is unreachable; an explicit "force rebuild" flag can come later.
- Choosing the specific Bedrock code-embedding model — left to config; Bedrock has no dedicated "code" embedding model, so the code model will be a general embedding model (e.g. Cohere Embed v4 or Titan v2) selected via config.

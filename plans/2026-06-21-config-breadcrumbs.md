# Objective and Context

User request, verbatim across the conversation:

> what are we currently doing for json, yaml, toml and other config files? Use
> the chunking debug to try a couple config files. are we packaging treesitter
> grammars for these formats?

> ok, let's make it consistent. Add treesitter grammars for the most common
> config languages. Let's see what we get when we do just this.

> hmm... so if we pack a.b.c as a single chunk, I would like "a.b.c" to be part
> of the breadcrumb. That would be useful for both semantic embedding and for
> displaying to tne end user. But this is different from an scm "definition"
> query, since every traversal is meaningful here. And I want to support things
> like a.b[1].c. Come up with a plan for how we can do this. I think just
> special logic for our breadcrumb generating code, specifically for config
> files

> write a full plan file (see the skill). I don't want to create a whole new
> implementation of the defn/cAST traversal just for this. So either we abstract
> out that piece and reuse it across code + chunking, or we just use the
> existing traverser, and make it a bit more pluggable/generic. I think allowing
> to specify "how to produce a breadcrumb for a chunk we're about to commit" as
> a customization point is sufficient here

## What we're building and why

Config files (`.json`, `.yaml`/`.yml`, `.toml`) now route to tree-sitter
grammars (already wired up: grammars added to `grammars.go`, extensions routed
in `filetype.go`). With no tags query vendored for them, the existing sweeper
falls back to pure budget-packing along node boundaries — good chunk boundaries,
but every chunk's breadcrumb is just the file path. The structural location of a
chunk (`services.web.ports[1]`) is lost.

We want each config chunk's breadcrumb to carry its **structural path from the
document root** — formatted dotted/bracketed (`a.b[1].c`). This breadcrumb is
both stored for display and prepended into the embedded text (the `<context>`
block in `manager.go`), so it directly improves semantic retrieval.

The crucial difference from code: for code, only `@definition.*` nodes from a
`tags.scm` query contribute breadcrumb segments. For config, **every container
traversal is meaningful** — each object key or array index is a path segment.
This is not expressible as a definition query, so it needs its own
breadcrumb-generation logic. Per the user's direction, we will NOT reimplement
the cAST traversal; we will make the existing sweeper's breadcrumb generation a
pluggable customization point and supply a config-specific implementation.

## Key types and entities

- `sweeper` (`internal/chunk/code.go`): the single recursive cAST pass that packs
  named nodes into budget-sized windows and flushes them as `ChunkInfo`. Today
  its breadcrumb comes from `breadcrumb()`, which joins `pathContext` with the
  labels of the active `defSpan` stack.
- `defIndex` / `defSpan` / `defEntry` (`internal/chunk/defindex.go`): the
  `tags.scm` query result that drives code breadcrumbs and hard chunk breaks.
  For config grammars `buildDefIndex` returns `(nil, nil)` (no vendored query),
  so `idx == nil` and span logic is inert — packing is pure budget windowing.
- `ChunkInfo` (`internal/chunk/chunk.go`): `{Text, HeadingContext, Start, End}`.
  `HeadingContext` is the breadcrumb.
- `ChunkCode` (`internal/chunk/code.go`): entry point; parses, builds the def
  index, runs the sweeper, falls back to `lineChunks` on parse failure.
- `manager.go` `chunkFile`: dispatches `ChunkCode` vs `ChunkMarkdown` on
  `route(path)`, then prepends `HeadingContext` into the embedded string.
- `filetype.go`: extension -> grammar routing (config grammars already added).

# Design

## The pluggable breadcrumb point

Add one customization point to the sweeper: a function that, given the byte
range of a window about to be flushed, returns the breadcrumb string for that
chunk. Conceptually:

    breadcrumbFn func(loByte, hiByte int) string

`flush()` is the single place breadcrumbs are attached, so this is the natural
seam. When `breadcrumbFn` is nil (the code path, unchanged), `flush()` uses the
existing def-span `breadcrumb()`. When set (the config path), `flush()` calls
`breadcrumbFn(winStart, winEnd)` instead.

Everything else in the traversal — windowing, comment attachment, the
line-split backstop, whitespace-window dropping — is reused verbatim. No second
traversal implementation. `idx` stays nil for config, so `spanFor`/`containsSpan`
remain inert and packing stays pure budget windowing (already verified to break
on clean node boundaries).

## How the config breadcrumb is computed

The breadcrumb of a config chunk is the **path from the root to the lowest
container node that fully encloses the window `[lo, hi)`** (the window's lowest
common ancestor), formatted as a config path.

- A window holding `svc8`..`svc15`, all under `services` -> `services`.
- A window that is one slice of an oversized `services.svc9` value ->
  `services.svc9` (or deeper).
- The whole-file root window -> empty path (breadcrumb is just the file path).

This is well-defined for any window the packer can emit, including windows that
straddle sibling keys (the LCA simply resolves to their common parent).

Resolution walks down from the root: at each node, find the unique named child
that fully contains `[lo, hi)`; if none, the current node is the LCA and we stop.
Each downward step may append a path segment. Formatting joins object-key
segments with `.` and renders array indices as `[i]`, producing `a.b[1].c`.
The result is combined with the file path via the existing
`joinBreadcrumb(pathContext, configPath)` (" > " separator), matching how code
and markdown breadcrumbs already read.

## The only grammar-specific piece: segment extraction

A small per-grammar adapter answers: "descending from parent into child, what
path segment (if any) is added, and is this a transparent pass-through?"

- JSON: `object`->`pair` => key string (unquoted); `pair`->value => transparent;
  `array`->element => `[i]` by child position.
- YAML: block/flow mapping pair => key text; block/flow sequence item => `[i]`;
  pair->value => transparent.
- TOML: tables are flat siblings whose header is a (possibly dotted) key path, so
  `document`->`table` => the full header path (a single segment that may itself
  contain dots); `table`->`pair` => key; `table_array_element` => header `[i]`;
  arrays => `[i]`.

Exact tree-sitter node-type names must be confirmed against the pinned grammar
versions with a throwaway parse during implementation, not assumed from memory.

## Dispatch

Config grammars must call the config chunker, not the code chunker. Add a
`chunk.IsConfigGrammar(grammar) bool` (set: json/yaml/toml) and a `ChunkConfig`
entry point that mirrors `ChunkCode` but installs `breadcrumbFn`. `chunkFile` in
`manager.go` selects `ChunkConfig` when the grammar is a config grammar, else
`ChunkCode`. `FileType` stays `Code`, so the embedding-model choice and the
deterministic per-chunk embedding-reuse path are unchanged.

## Alternatives considered and rejected

- Vendoring a `tags.scm` per config grammar: definition queries label only
  matched nodes; config needs a segment for *every* container traversal, which
  the query model cannot express.
- A separate config traversal/chunker: duplicates the cAST packing, comment
  handling, and line-split backstop. Rejected per the user's explicit direction.
- Prepending the path into chunk text directly: unnecessary — `HeadingContext`
  is already injected into the embedded `<context>` block by `manager.go`.

Invariants:
- The code path is byte-for-byte unchanged: with `breadcrumbFn == nil`, `flush()`
  produces exactly today's breadcrumbs (guard with existing `code_test.go`).
- Chunk boundaries for config are identical whether or not the breadcrumb hook is
  installed — the hook affects only `HeadingContext`, never window geometry.
- Parse failure / unknown grammar still falls back to `lineChunks` (file-path
  breadcrumb only), never erroring out.
- The resolved path is always an ancestor-prefix of the window: a chunk's
  breadcrumb names a container that fully encloses every byte in the chunk.
- Array indices are 0-based, matching the underlying data.

# Stages

## Stage 1: Make the sweeper's breadcrumb generation pluggable

- Goal: the sweeper exposes a breadcrumb customization point used at flush time;
  `ChunkCode` behavior is unchanged because it leaves the hook nil.
- Verification:
  - Behavior: existing code chunking (Go/TS/etc.) produces identical breadcrumbs
    and chunk boundaries after the refactor.
  - Setup: the existing `code_test.go` fixtures.
  - Actions: run `ChunkCode` over the fixtures.
  - Expected outcome: no diffs in `HeadingContext`, `Text`, `Start`, `End`.
- Before moving on: confirm tests, type checks, and linting all pass.

## Stage 2: Config path resolver + per-grammar segment adapters

- Goal: a pure resolver that, given a parsed tree and a byte range, returns the
  formatted config path (`a.b[1].c`) of the window's lowest enclosing container,
  with correct JSON/YAML/TOML segment extraction.
- Verification:
  - Behavior: a byte range maps to the expected dotted/bracketed path for each
    grammar, including nested objects, array indices, TOML dotted-table headers,
    and the root range (empty path).
  - Setup: small in-memory source strings per grammar with known offsets; verify
    node-type names via a throwaway parse first.
  - Actions: parse, call the resolver for chosen ranges.
  - Expected outcome: e.g. JSON `deps.nested`, YAML `services.web.ports[1]`,
    TOML `servers.alpha.ip`; root -> "".
- Before moving on: confirm tests, type checks, and linting all pass.

## Stage 3: ChunkConfig entry point + dispatch wiring

- Goal: `ChunkConfig` parses, installs the resolver as `breadcrumbFn`, reuses the
  sweeper, and `manager.go` routes config grammars to it; falls back to
  `lineChunks` on parse failure.
- Verification:
  - Behavior: config files chunk with path breadcrumbs; small files are a single
    chunk with a file-path-only breadcrumb; large files split on node boundaries
    with each chunk's breadcrumb naming its enclosing container.
  - Setup: small and large json/yaml/toml fixtures; a small `maxChunkSize` to
    force deterministic splits.
  - Actions: run `ChunkConfig` (and an end-to-end `chunkFile` check via the
    index manager / `pkb chunk`).
  - Expected outcome: `HeadingContext` like `f.yaml > services.web` and
    `f.json > deps.nested`; boundaries match the pre-breadcrumb packing.
- Before moving on: confirm tests, type checks, and linting all pass.

## Stage 4: Validate on real config + reindex

- Goal: confirm behavior on real-world config (lockfiles, CI workflows, k8s
  manifests) and that the index reflects the new breadcrumbs.
- Verification:
  - Behavior: `pkb chunk` on representative real files shows sensible paths;
    `pkb search` for a config-located concept returns the right chunk with its
    path breadcrumb.
  - Setup: a few real files in/around the repo; reindex.
  - Actions: `pkb chunk <file>`; `pkb reindex`; `pkb search`.
  - Expected outcome: paths read naturally; no parse-failure fallbacks on common
    files; search surfaces config chunks with structural breadcrumbs.
- Before moving on: confirm tests, type checks, and linting all pass.

# Objective and Context

User request (verbatim across the conversation):

> "write a plan for doing the whole thing. I'd also like to include hcl"

where "the whole thing" refers to the preceding discussion: replace the code
chunker's hand-rolled `declKinds`/`declName`/`labelFromKind` heuristics with
tree-sitter **`tags.scm`** query files vendored per grammar, keeping the
existing descent/traversal and size-budget logic but driving the
declaration-vs-filler decision (and symbol names, labels, and doc-comment
attachment) from query captures. Additionally, add **HCL** as a supported code
grammar.

## What we're building and why

Today `internal/chunk/code.go` walks the tree-sitter CST by hand. It guesses
which node kinds are "declarations" via a global `declKinds` map, derives the
symbol name by looking for the first `identifier`-ish child (`declName`), and
derives a human label by trimming kind suffixes (`labelFromKind`). This is one
language-agnostic heuristic stretched across Go/JS/TS/Python/Rust. It has two
known weaknesses:

1. **Per-language inaccuracy** — node kinds and name fields differ per grammar;
   the global map and first-identifier fallback are brittle.
2. **Doc comments are divorced from their symbols** — a `//`/`/** */`/`///` doc
   comment preceding a declaration is a *sibling* node, not part of the decl's
   byte range, so it lands in the preceding filler chunk carrying the parent's
   breadcrumb.

Each grammar ships a `tags.scm` query (the basis of GitHub code-nav / ctags)
that declares, per language, exactly which nodes are definitions
(`@definition.function`, `@definition.method`, `@definition.type`,
`@definition.class`, `@definition.interface`, `@definition.module`, ...), their
names (`@name`), and adjacent doc comments (`@doc` with `#set-adjacent!`). We
will run that query once per file to build a definition index, then keep the
existing recursion but consult the index instead of the heuristics.

## Key entities

- `ChunkInfo` (`internal/chunk/chunk.go`) — uniform chunker output: `Text`,
  `HeadingContext` breadcrumb, `Start`/`End` `Position`. Unchanged.
- `ChunkCode` (`internal/chunk/code.go`) — entry point. Parses, then
  `chunkContainer` walks children grouping filler vs declarations; `emitDecl`
  emits one declaration (fit-as-one / recurse-into-body / line-split);
  `emitRange` emits filler. These stay, but their "is this a declaration?"
  predicate and name/label derivation change.
- `grammars` / `languageFor` / `HasGrammar` (`internal/chunk/grammars.go`) —
  grammar-name -> tree-sitter `Language` registry. Gains HCL and a parallel
  registry of vendored query sources.
- `codeExts` / `RouteExt` (`internal/filetype/filetype.go`) — ext -> grammar.
  Gains HCL extensions.
- New: vendored `internal/chunk/queries/<grammar>.scm` files, embedded via
  `//go:embed`.

## Relevant files

- `internal/chunk/code.go` — traversal + emit logic to be query-driven.
- `internal/chunk/grammars.go` — grammar registry; add HCL + query registry.
- `internal/chunk/queries/*.scm` — new vendored tags queries (one per grammar).
- `internal/chunk/code_test.go` — existing behavioral tests to preserve/extend.
- `internal/filetype/filetype.go` — ext routing; add HCL.
- `go.mod` / `go.sum` — add the HCL grammar binding.

# Design

## Definition index

Add a per-file pre-pass that runs the grammar's vendored `tags.scm` over the
parsed tree with `tree_sitter.NewQuery` + a `QueryCursor` (note: both must be
`Close()`d — the Go binding requires explicit Close on Parser/Tree/Query/
QueryCursor). From the matches we build:

- `defNodes`: set of node IDs that are `@definition.*` captures.
- `defInfo[nodeID] -> {name, label, docStartByte}` where:
  - `name` comes from the `@name` capture inside the same match,
  - `label` comes from the capture name suffix (`definition.method` ->
    "method", `definition.function` -> "function", etc.),
  - `docStartByte` comes from the `@doc` capture in the same match (the earliest
    byte of the adjacent doc-comment run), if present.

Node identity: use the node's start/end byte span (or `Node.Id()` if exposed by
v0.25 bindings — implementer to confirm) as the map key, since we re-encounter
the same nodes during the manual walk.

## Traversal changes (keep the shape, swap the predicate)

`chunkContainer` and `emitDecl` keep their current structure. The changes:

- **Declaration test**: replace `isDeclKind(unwrap(child).Kind())` with "is this
  node (or its unwrapped inner node) in `defNodes`?" `unwrap` (export_statement /
  decorated_definition peeling) stays, because `tags.scm` typically captures the
  inner `function_declaration`, not the `export_statement` wrapper.
- **Name/label**: in `emitDecl`, replace `declName` + `labelFromKind` with a
  lookup into `defInfo`. Keep `joinBreadcrumb` and the `label + " " + name`
  formatting so existing breadcrumb strings (`"a/b.ts > function foo"`,
  `"c.ts > class Foo > method alpha"`) are preserved.
- **Doc-comment attachment**: when emitting a declaration that has a
  `docStartByte`, extend the chunk's start byte backward to `docStartByte` (and
  its `Start` position to the doc comment's start) so the doc travels with the
  symbol. Correspondingly, `chunkContainer`'s filler accumulation must *exclude*
  comment nodes that were claimed as `@doc` for the following declaration, so
  they aren't also emitted as filler. Simplest approach: when flushing filler
  before a declaration, trim any trailing nodes whose byte range is at/after the
  declaration's `docStartByte`.

## Fallbacks (unchanged contract)

`ChunkCode` keeps every existing fallback to `lineChunks`: unknown grammar, nil
language, parser failure, nil tree, root `HasError()`, or zero chunks produced.
Additionally, if a grammar has no vendored query, or the query fails to compile,
fall back to the *current* heuristic path (`declKinds` etc.) rather than losing
structure. This means `declKinds`/`declName`/`labelFromKind` are retained as the
no-query fallback, not deleted — important for resilience and for any grammar
whose `tags.scm` we cannot vendor.

## HCL specifics

HCL has no functions/classes; its structure is `block`s (e.g.
`resource "aws_instance" "web" { ... }`) and top-level `attribute`s. The
`tree-sitter-grammars/tree-sitter-hcl` binding exposes `func Language()
unsafe.Pointer`, matching our `NewLanguage(...)` pattern. Two facts the
implementer must verify before relying on queries for HCL:

1. Whether that module vendors a `queries/tags.scm`, and what captures it uses.
   HCL tags conventions differ (blocks aren't "functions"). If absent or
   unsuitable, HCL should fall back to the heuristic path, or we hand-write a
   minimal `hcl.scm` capturing `(block) @definition.*` with the block type/labels
   as `@name`.
2. ABI compatibility with `go-tree-sitter v0.25.0` (our other grammars are
   v0.23.x and work fine; confirm HCL binding parses without error).

For breadcrumbs, an HCL block's "name" should be the block type plus its labels
(e.g. `resource "aws_instance" "web"`); the label is the block keyword. This may
require HCL-specific name extraction rather than a generic `@name` capture.

## Vendoring

The `.scm` files live in the Go module cache, not our repo, and the binary is
statically linked cgo, so queries are *not* compiled in. Copy each grammar's
`tags.scm` into `internal/chunk/queries/<grammar>.scm` and `//go:embed` them.
Vendored queries must match the grammar version pinned in `go.mod`; bumping a
grammar means re-vendoring its query. Source versions currently:
go v0.23.4, javascript v0.23.1, python v0.23.6, rust v0.23.2, typescript v0.23.2
(typescript ships separate tsx vs typescript trees but a shared `tags.scm`).

Invariants:
- Existing breadcrumb strings and chunk boundaries for the current test corpus
  must not regress (the query path must reproduce today's outputs for the cases
  in `code_test.go`).
- Every existing fallback to `lineChunks` is preserved; add a new fallback to
  the heuristic path when no/!compiling query.
- Oversized declarations still recurse into bodies with nested declarations, and
  still line-split when they are leaves.
- Filler coalescing never crosses a declaration boundary; a node claimed as a
  declaration's `@doc` is emitted with the declaration, never also as filler.
- All resources (`Query`, `QueryCursor`) are `Close()`d.

# Stages

## Stage 1: Vendor queries + query-source registry (no behavior change)

> Status: DONE. Vendored tags.scm for go/javascript/python/rust/typescript into
> `internal/chunk/queries/*.scm`, embedded via `//go:embed` in
> `internal/chunk/queries.go`, exposing `queryFor(grammar) string`. Decision:
> typescript.scm and tsx.scm concatenate javascript.scm (upstream TS tags rely
> on query inheritance) so they capture the full ECMAScript construct set; tsx
> reuses the typescript query. Verified by `queries_test.go`
> (TestVendoredQueriesCompile: every grammar compiles with >0 patterns and is
> Close()d; TestQueryForUnknownGrammar). Nothing consumes the registry yet.

- Goal: `tags.scm` for the five existing grammars are copied into
  `internal/chunk/queries/` and embedded; a `queryFor(grammar)` helper returns
  the source (or "" if none). Nothing consumes it yet.
- Verification:
  - Behavior: each vendored query compiles against its grammar's `Language`.
  - Setup: table of grammar -> embedded source.
  - Actions: `tree_sitter.NewQuery(languageFor(g), queryFor(g))` for each.
  - Expected outcome: no compile error; query has > 0 patterns; resources closed.
- Before moving on: confirm tests, type checks, and linting all pass.

> Status: DONE. Added `internal/chunk/defindex.go` with `defEntry`
> {name,label,docStartByte} and `defIndex` {nodes set, info map}, plus
> `buildDefIndex(root, source, grammar)` which runs the vendored tags.scm via
> `NewQuery` + `QueryCursor.Matches` (both Close()d). Node identity uses
> `Node.Id()` (uintptr, stable within a parsed tree). Per match we pick the
> `@definition.*` node (label = suffix), its `@name` text, and the earliest
> `@doc` start byte (-1 when absent). Returns (nil,nil) when no query is
> vendored (heuristic-fallback signal) and a non-nil error on query compile
> failure. Verified by `defindex_test.go`: Go (Foo function carries `//` doc;
> Bar type has docStartByte == -1), TypeScript (foo function, Bar class, alpha
> method indexed), and the no-query grammar returns nil. Not yet wired into
> traversal. Note: the `defIndex.has` accessor was deferred to Stage 3 to avoid
> an unused-symbol lint error.

## Stage 2: Definition index builder

- Goal: a function that, given a tree + source + grammar, returns the
  `defNodes` set and `defInfo` map (name, label, docStartByte) by running the
  query. Pure data, not yet wired into traversal.
- Verification:
  - Behavior: for a small Go/TS sample, the index identifies each function/
    method/type with correct name and label, and captures a preceding `//` doc
    comment's start byte.
  - Setup: inline source strings (mirror `code_test.go` style).
  - Actions: parse, build index, assert membership and fields.
  - Expected outcome: names/labels match; doc start precedes the decl start;
    a decl with no doc comment has zero `docStartByte`.
- Before moving on: confirm tests, type checks, and linting all pass.

> Status: DONE. `ChunkCode` now builds the defIndex (via `buildDefIndex`) once
> per file after the root check; a query compile error sets idx=nil so the
> heuristic path runs. Threaded `*defIndex` through `chunkContainer`, `emitDecl`,
> and `hasDeclChild`. New `isDecl(node, idx)` predicate consults the index when
> non-nil, else `isDeclKind`. `unwrap` is retained as-is (tags.scm captures the
> inner decl; peeling export/decorated wrappers via isDeclKind still finds the
> node whose Id we look up). `emitDecl` uses `defEntry.{label,name}` when indexed
> (falling back to `labelFromKind`/`declName`), and when `docStartByte >= 0`
> extends the chunk start backward to the doc comment (start Position via new
> `posFromByte`). `chunkContainer` now accumulates filler as a node slice and,
> before flushing for a doc-carrying decl, trims trailing filler nodes at/after
> docStartByte so the comment isn't double-emitted. `declKinds`/`declName`/
> `labelFromKind` retained as the no-query fallback. Added accessors
> `defIndex.has`/`defIndex.entry` (nil-safe). Tests added to `code_test.go`:
> TestChunkCodeDocCommentAttachedToDecl (Go doc travels with Foo, absent from
> filler, chunk starts at line 3) and TestChunkContainerHeuristicFallback
> (nil idx reproduces the two-function breadcrumbs). Existing cases unchanged.
> Full `go build/vet/test ./...` green.

## Stage 3: Wire index into traversal with heuristic fallback

- Goal: `chunkContainer`/`emitDecl` use the index when a query exists; retain
  `declKinds` path when it doesn't or the query fails to compile. Doc comments
  are attached to their declarations and excluded from filler.
- Verification:
  - Behavior 1: existing `code_test.go` cases (two functions, top-level filler,
    oversized class -> per-method) still pass unchanged.
  - Behavior 2: a function with a leading doc comment produces a single chunk
    whose `Text` includes the comment and whose breadcrumb is the function's
    (not the parent filler's), and the comment is absent from any filler chunk.
  - Behavior 3: forcing an empty query for a grammar reproduces the old
    heuristic output (fallback works).
  - Setup: inline sources; a test hook to simulate "no query".
  - Actions: `ChunkCode` over each sample.
  - Expected outcome: breadcrumbs, chunk counts, and doc placement as described.
- Before moving on: confirm tests, type checks, and linting all pass.

## Stage 4: Add HCL grammar

- Goal: HCL binding added to `go.mod` and the `grammars` registry; `.tf`/`.hcl`
  extensions route to grammar "hcl" in `filetype.go`; HCL files chunk into
  per-block chunks with sensible breadcrumbs (block keyword + labels as name),
  using a vendored or hand-written `hcl.scm`, else the heuristic fallback.
- Verification:
  - Behavior 1: routing — `RouteExt(".tf")` and `RouteExt(".hcl")` return
    `{Code, "hcl"}`.
  - Behavior 2: a sample with two `resource` blocks yields two declaration
    chunks with breadcrumbs reflecting the block type and labels; top-level
    attributes group into filler.
  - Behavior 3: an oversized block line-splits (leaf) rather than being dropped.
  - Setup: inline HCL source; confirm the binding parses without `HasError()`.
  - Actions: `RouteExt`, then `ChunkCode([]byte(src), "hcl", "main.tf", ...)`.
  - Expected outcome: correct routing, block-level chunking, no parse errors.
- Before moving on: confirm tests, type checks, and linting all pass.

## Stage 5: Cleanup + cross-language coverage

- Goal: add per-grammar smoke tests (Go method with doc, Python class with
  docstring still attached, Rust impl, JSX) asserting names/labels/doc handling;
  document the re-vendoring requirement near the embed directives.
- Verification:
  - Behavior: representative sample per grammar yields expected breadcrumbs and
    doc attachment.
  - Setup: small inline sources per language.
  - Actions: `ChunkCode` per sample.
  - Expected outcome: stable breadcrumbs; docs attached; no regressions.
- Before moving on: confirm full test suite (`go test ./...`), `go vet ./...`,
  and `go build ./...` all pass.

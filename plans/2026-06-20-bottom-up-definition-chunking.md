# Objective and Context

The user's request, captured verbatim:

> ok, so it seems most others use cAST, but I'm not a fan. I don't think
> adjacency or size are good signals of orthogonality / meaningfulness...
>
> I'd like to opt for something like... split on AST boundaries, but be clever
> about packing them together. packing the doc string *with* the function is one
> example. Packing an interface declaration with its implementation another...
>
> I think we're close with our current implementation. Just a bit off, since we
> have this top-down bias.
>
> So yeah. Let's do the "chunk on definitions" approach, extend to line starts.
> Add a custom scm file for hcl (to capture blocks as definitions).
>
> And then we may have to do some custom tuning for various languages...
>
> Write up a plan

## What we're building and why

Today `ChunkCode` (internal/chunk/code.go) is **top-down**: it walks the named
children of a container node and asks, per node, "is this AST node kind a
declaration?" (the `declKinds` table) or "is this exact node one the tags.scm
query captured as a `@definition.*`?" (the `defIndex`). The brittleness: the
tags.scm capture often lands on an *inner* node (Go's `type_spec`) while the
container's direct child is an outer wrapper (`type_declaration`). The walker
sees the wrapper, the index keyed the inner node, so the definition is missed
and silently demoted to filler — e.g. `type State struct {...}` gets merged into
the import block. The current workaround is `unwrap`, a hand-maintained,
per-language table of wrapper node kinds (`export_statement`,
`decorated_definition`). That is exactly the per-language encoding we want to
stop relying on.

We are switching to a **bottom-up** model: the `@definition.*` captures from each
grammar's vendored tags.scm ARE the chunk boundaries. We stop reasoning about
tree topology (parent/child, "is this a decl kind") and instead reason about the
byte-range intervals of the captured definitions, nested by containment. This
keeps the "split on real AST/definition boundaries, then pack thoughtfully"
philosophy (doc comment travels with its definition; over-budget parents emit a
header chunk then recurse into member definitions) while removing the topological
special-casing.

## Key entities

- `ChunkInfo` (internal/chunk/chunk.go) — uniform chunker output: `Text`,
  `HeadingContext` (breadcrumb), `Start`/`End` `Position`. Unchanged.
- `defIndex` / `defEntry` (internal/chunk/defindex.go) — result of running a
  grammar's tags.scm: per-`@definition.*` node, the `name`, `label`, and
  `docStartByte`. This becomes the *primary* input to chunking rather than a
  side lookup. We will need its entries enumerable (with byte ranges), not just
  queryable by node id.
- tags.scm queries (internal/chunk/queries/*.scm, embedded via
  internal/chunk/queries.go) — the language-agnostic contract for "what is a
  meaningful definition." We add an HCL query here.
- `grammars` / `languageFor` (internal/chunk/grammars.go) — grammar name to
  tree-sitter language. HCL is already registered.
- `splitCodeByLines` / `lineChunks` (internal/chunk/code.go) — budget-based
  line splitting and the no-grammar fallback. Reused as-is.

## Relevant files

- internal/chunk/code.go — the chunker. Heart of the change: replace the
  top-down `chunkContainer`/`emitDecl`/`unwrap`/`declKinds` machinery with the
  single cAST-with-forced-flushes sweep. Keep `splitCodeByLines`, `lineChunks`,
  position helpers, `joinBreadcrumb`.
- internal/chunk/defindex.go — make captured definitions enumerable as spans;
  keep doc-comment association.
- internal/chunk/queries/hcl.scm (new) + queries.go — add an HCL tags.scm
  capturing top-level blocks as `@definition.*`.
- internal/chunk/code_test.go — existing behavior contract to preserve/port.
- internal/index/manager.go — caller (`grammarFor`, `ChunkCode`); the HCL
  `"config_file > body"` descent currently lives in `ChunkCode` and may move or
  disappear. No API change expected.

# Design

## Terminology

These terms are used consistently throughout the rest of this document:

- **definition span** (or **span**): one `@definition.*` capture from the
  grammar's tags.scm — a byte interval `[start,end)` plus its `name`, `label`,
  and `docStartByte`. The unit that drives boundaries and breadcrumbs.
- **extending the span**: adjusting a definition span's *start* before use, in
  two steps — (1) snap it back to the beginning of its line (to pick up leading
  keywords/modifiers like Go's `type `), and (2) pull it further back over the
  immediately-preceding comment/docstring run, which the tags.scm `@doc` capture
  already identifies (`docStartByte`). End is left as captured.
- **enter / exit event**: crossing the start (enter) or end (exit) of a
  definition span's extended interval during the sweep. Both force a flush.
- **window**: the buffer of text the cAST packer is currently accumulating
  toward the size budget; a **flush** emits it as one chunk (dropped if
  whitespace-only).
- **sweep**: the single recursive pass over the parse tree that does cAST
  window-packing, refined by enter/exit events.
- **active-span stack**: the definition spans currently entered-but-not-exited;
  its contents at flush time form the chunk's breadcrumb.

## One pass: cAST sweep with definition-forced flushes

The two ideas (def-driven boundaries and cAST packing) collapse into a **single
recursive sweep**. We do ordinary cAST token-window packing over the parse tree,
*refined* by the definition data: a definition span is a **hard break** —
entering or exiting one force-flushes the current window — and the set of
definitions we are currently inside drives the breadcrumb. There is no separate
"nested-def" vs "flat-filler" machinery; it is one walk.

Setup:

1. Parse the file, run the grammar's tags.scm, and collect every `@definition.*`
   capture as a **definition span**: a byte interval `[start,end)` plus `name`, `label`,
   and `docStartByte`. This is the only structural input beyond the parse tree;
   we never consult AST node *kinds*.

2. **Extend each definition span's start** to the beginning of its line (and back over any
   `docStartByte` doc run). Go's `type_spec` capture starts at `State`, not
   `type `; snapping to column 1 recovers the leading keyword/modifiers
   language-agnostically. All enter/exit boundaries below use these *extended*
   spans, so the leading `type ` token lands inside the definition rather than in
   the preceding window.

The sweep walks named nodes in source order, carrying a current window and a
**stack of active definition spans** (the breadcrumb). At each node:

- **Entering a definition span** (the node is, or its extended span begins, a definition
  not yet on the stack): flush the current window, push the definition span, and recurse —
  so the definition span's own header text accumulates into a fresh window under the new
  breadcrumb.
- **Exiting a definition span** (we pass its end): flush the current window and pop. A
  definition that fit entirely within one window is thus emitted whole, because
  the exit immediately flushes it.
- **A plain node that fits** the budget and contains no definition span boundary: add it
  to the window and continue.
- **A plain node that overflows** the window: flush, start a new window at the
  node.
- **A node too large to fit even alone**, or one that *contains* a nested definition span:
  recurse into its named children and apply the same rules one level down. A
  childless leaf over budget is line-split via `splitCodeByLines` as the final
  backstop.

Worked example (`class A { private x; function b() {...} }`):

- Reach `class A` → enter definition span A: flush (empty), push A. `class A {` and the
  `private x` filler accumulate in a window with breadcrumb `... > class A`.
- Reach `function b` → enter definition span b: flush that window (the class header +
  filler chunk, breadcrumb `class A`), push b. `b`'s body accumulates under
  `class A > function b`.
- Pass `b`'s end → exit b: flush (b emitted whole if it fit, or as several cAST
  windows if not), pop. The trailing `}` of A is filler under `class A`.
- Pass `A`'s end → exit A: flush, pop.

So the whole design reduces to: **cAST packing, where crossing a definition
boundary forces a flush and the active-span stack supplies the breadcrumb.**
Doc comments ride with their definition because the definition span's extended start
swallows the doc run before the enter-flush. Whitespace-only flushes are dropped
(as today).

**Fallbacks.** A grammar with a tags.scm but a region with no definition spans is just the
same sweep with an empty stack (pure cAST under the file-path breadcrumb). A
grammar with *no* vendored tags.scm is the same again — every node is plain, so
the whole file is cAST-packed. Only an unknown grammar or a source that fails to
parse drops to the dumb `lineChunks` line-splitter. There is no per-language
unwrap table.

This removes `declKinds`, `isDeclKind`, `labelFromKind`, `declName`, `unwrap`,
`isDecl`, `hasDeclChild`, and `hclBlockParts` — all of which existed to bridge
topology to captures. Labels/names now come straight from the tags.scm capture
(`label` from the `@definition.<label>` suffix, `name` from `@name`).

## HCL via tags.scm

HCL currently has no vendored tags.scm, so it rode the `declKinds` `"block"`
heuristic plus the `config_file > body` descent in `ChunkCode`. Under the new
model that heuristic is gone, so HCL needs its own query
(internal/chunk/queries/hcl.scm) capturing top-level blocks (`block` nodes:
`resource`/`variable`/`module`/...) as `@definition.block`, with the block type
identifier as `@name`. With blocks captured as definition spans, the special
`config_file > body` unwrapping is unnecessary — the block intervals are found
regardless of how deep they sit, so that branch in `ChunkCode` can be removed.

## "Packing" follow-ups (out of scope for first cut, noted as future tuning)

The user named two packing goals: doc-with-definition (already handled by
`docStartByte`, preserved here) and interface-with-implementation. The latter
(and other per-language tuning) is deliberately *not* in the first
implementation; the bottom-up interval representation is the foundation that
makes such packing rules expressible as operations over definition spans. Capture the
idea, don't build it yet.

Invariants:
- Every byte of the file lands in at most one chunk; chunks are emitted in
  source order; concatenating chunk spans (plus dropped whitespace) reconstructs
  the file. No byte is emitted twice (the current doc-comment double-emit guard
  must be preserved by construction, since a definition span owns its doc run and the
  preceding filler ends at the doc start).
- A definition is never silently demoted to filler because of wrapper nodes —
  the whole point. `type State struct {...}` must be its own chunk with
  breadcrumb `... > type State`.
- Doc comments travel with their definition, not the preceding filler.
- Breadcrumbs match today's format (`path > label name`, nested with ` > `) so
  existing expectations and downstream search formatting are unchanged.
- Anchors that share a start line but are siblings (grouped `type ( A; B )`,
  `var (...)`) each become their own chunk; line-start snapping must not collapse
  or overlap sibling intervals.
- A window never straddles a definition boundary: entering or exiting a definition span
  force-flushes, so no chunk mixes a definition's body with code outside it. The
  active-span stack at flush time is exactly the chunk's breadcrumb.
- The sweep terminates: every node either fits a window, starts a fresh window,
  or is recursed into; a childless leaf over budget is line-split as the final
  backstop, so progress is always made.
- No-query files go through the same sweep with an empty definition span stack (whole-file
  cAST); only unknown-grammar / parse-error files fall back to dumb line
  chunking.
- Position values remain 1-based line / 1-based col, consistent with `posOf`.

# Stages

## Stage 1: Make definitions enumerable as spans (DONE)

Implemented: added a `defSpan` type (embeds `defEntry` plus `start`/`end` byte
range) and a `spans []defSpan` field on `defIndex`, populated during
`buildDefIndex` and sorted source-order (by start, then end) via
`sort.SliceStable`. The existing id-keyed `nodes`/`info` lookups are unchanged,
so `ChunkCode` behavior is untouched. Added `TestBuildDefIndexSpansGo` covering
function/method/`type ... struct` spans, ordering, and doc bytes. Full suite
(`go build ./...`, `go vet ./...`, `go test ./...`, golangci-lint) green.

- Goal: `defindex.go` can return all `@definition.*` captures as an ordered list
  of `{start, end, name, label, docStartByte}`, in addition to the existing
  id-keyed lookup. No behavior change to `ChunkCode` yet.
- Verification:
  - Behavior: a Go source with a function, a method, and a `type ... struct`
    yields definition spans for all three, with the type definition span's interval being the
    `type_spec` range and correct name/label.
  - Setup: parse fixture source with the `go` grammar, call `buildDefIndex`,
    enumerate.
  - Actions: enumerate definition spans.
  - Expected outcome: three definition spans with expected names (`Foo`, method name,
    `State`), labels (`function`/`method`/`type`), and doc bytes where present.
- Before moving on: confirm tests, type checks (go build ./..., go vet ./...),
  and tests (go test ./...) all pass.

## Stage 2: The unified cAST-with-forced-flushes sweep (DONE)

Implemented: `ChunkCode` now drives a single recursive `sweeper`
(internal/chunk/code.go) that does cAST byte-range window-packing with
definition spans as hard breaks and an active-span stack for the breadcrumb.
Spans are matched to their captured node by `Node.Id()` via `idx.entry`; the
chunk text starts at an *extended* start (line start, pulled further back over
`docStartByte`), so wrapper keywords (`type `/`export `) and doc comments ride
with the definition even though the capture lands on an inner node. The window
is a `[winStart,winEnd)` byte interval, so anonymous tokens between named
children are naturally included. Deleted: `declKinds`, `isDeclKind`,
`labelFromKind`, `unwrap`, `chunkContainer`, `isDecl`, `emitDecl`, `emitRange`,
`hasDeclChild`, `hclBlockParts`, `declName`, `posOf`, plus the now-unused
`defIndex.has`/`nodes`. Kept `splitCodeByLines`, `lineChunks`, `posFromByte`,
`joinBreadcrumb`; added `lineStartByte`.

Decisions/deviations:
- `containsSpan` uses a *strict* `sp.start > node.start` test: a definition
  span (e.g. Go `type_spec`) shares its start byte with its own first child
  (the name identifier), and a `>=` test made that child recurse and shatter the
  span into `type `/`State`/`struct{...}` fragments. Strict `>` is correct
  because a span that begins exactly at the node is either the node itself
  (handled by `spanFor`) or the active span's coinciding boundary.
- The `config_file > body` HCL descent was simply dropped (the sweep walks from
  root); HCL currently has no vendored tags.scm so it whole-file cAST-packs.
  The two HCL tests (`TestChunkCodeHCLBlocks`,
  `TestChunkCodeHCLOversizedBlockLineSplit`) are `t.Skip`-ped with a note;
  Stage 3 adds hcl.scm and un-skips them.
- `TestChunkContainerHeuristicFallback` was deleted (it exercised the removed
  `chunkContainer` + kind heuristic). Decorators/interfaces packing remains a
  Stage 4 / future-tuning concern.

Added tests: `TestChunkCodeGoTypeNotMergedWithImports` (motivating bug),
`TestChunkCodeGoGroupedTypes` (grouped `type ( A; B )` → two chunks),
`TestChunkCodeCASTPacksSiblings` (oversized function packs into multiple
budget-bounded, node-boundary windows). Full suite + `go vet` + golangci-lint
green; `./pkb chunk internal/index/manager.go` shows `type State` as its own
`> type State` chunk.

- Goal: rewrite the body of `ChunkCode` as the single recursive sweep —
  line-start/doc-extended definition spans as hard breaks, a current window, and an
  active-span breadcrumb stack — deleting the topological helpers (`unwrap`,
  `declKinds`, `chunkContainer`, `emitDecl`, `isDecl`, `hasDeclChild`,
  `hclBlockParts`, `labelFromKind`, `declName`). Keep `splitCodeByLines`,
  `lineChunks`, position helpers, `joinBreadcrumb`. This is the whole mechanism;
  there is no separate filler pass.
- Goal detail: port every existing case in code_test.go and add the cAST-packing
  and motivating-bug cases below.
- Verification:
  - Behavior: two-function TS file → 2 chunks with correct breadcrumbs/positions
    (port of TestChunkCodeTwoFunctions).
  - Behavior: top-level imports + function → filler chunk + function chunk (port
    of TestChunkCodeTopLevelFiller).
  - Behavior: oversized class → header chunk (class line + leading filler) plus
    one chunk per method, correct nested breadcrumbs (port of
    TestChunkCodeOversizedClassSplitsMethods); confirm the enter/exit flush
    around each method produces whole-method chunks when they fit.
  - Behavior (new, the motivating bug): Go file with imports immediately
    followed by `type State struct {...}` yields a separate `... > type State`
    chunk that includes the `type ` keyword line, NOT merged with imports.
    - Setup: small Go fixture; `ChunkCode(..., "go", "m.go", TargetChunkSize)`.
    - Actions: chunk it.
    - Expected outcome: a chunk whose Text starts with `type State struct` and
      whose HeadingContext is `m.go > type State`; the import lines are in a
      separate filler chunk.
  - Behavior (cAST packing): a flat region of several small sibling statements
    that together exceed the budget is packed into windows that each fit, split
    at node boundaries (not mid-node / mid-line).
    - Setup: a Go/TS fixture region of sibling statements with a small budget.
    - Expected outcome: multiple chunks, each <= budget, each ending on a node
      boundary; no node split that would have fit whole.
  - Behavior (oversized node recursion): a single node larger than the budget
    recurses into its named children; only a childless oversized leaf falls back
    to `splitCodeByLines`.
  - Behavior: grouped `type ( A struct{}; B struct{} )` yields two definition spans / two
    chunks (each enter/exit flushes independently).
  - Behavior: no-vendored-tags.scm grammar chunks the whole file via the sweep
    with an empty definition span stack; unknown grammar / parse error still falls back to
    `lineChunks` (port of existing fallback tests).
- Before moving on: confirm tests, type checks, and linting all pass; spot-check
  with `./pkb chunk internal/index/manager.go` that `type State` is now its own
  chunk.

## Stage 3: HCL tags.scm and removal of the config_file special case

- Goal: add internal/chunk/queries/hcl.scm capturing top-level blocks as
  `@definition.block` with the block type as `@name`; register it in queries.go;
  remove the `config_file > body` descent from the old code path (now obsolete).
- Verification:
  - Behavior: a `.tf` file with two top-level `resource` blocks yields one chunk
    per block with a breadcrumb derived from the block type and labels.
    - Setup: HCL fixture with `resource "aws_instance" "web" {...}` and a second
      block; `ChunkCode(..., "hcl", "main.tf", TargetChunkSize)`.
    - Actions: chunk it.
    - Expected outcome: two chunks, each spanning a full block, breadcrumb
      reflecting block type/labels; no block merged into filler.
  - Behavior: regression — `./pkb chunk main.tf`-style output matches prior HCL
    chunk boundaries (or improves on them) with no panic when there is no
    `config_file` wrapper assumption.
- Before moving on: confirm tests, type checks, and linting all pass.

## Stage 4: Cross-language smoke + doc-packing confirmation

- Goal: confirm the bottom-up model holds across the supported grammars and that
  doc/comment packing is intact, using the `pkb chunk` CLI for manual inspection
  and a couple of automated assertions.
- Verification:
  - Behavior: Python file with a decorated function — confirm current decorator
    handling status (decorators previously pulled in via `decorated_definition`
    in `unwrap`). Decide and document whether decorators are included via the
    Python tags.scm capture span or are (acceptably) left as preceding filler in
    the first cut.
    - Setup: small `.py` fixture with `@decorator\ndef f(): ...`.
    - Actions: chunk it; assert on whether the decorator line is inside the
      function chunk or a separate filler chunk, and record the chosen behavior.
  - Behavior: doc comment immediately above a Go function is part of the function
    chunk, not the preceding filler (port/keep existing doc-association test).
  - Manual: `./pkb chunk` over one file per grammar (go, typescript, python,
    rust, hcl, and a markdown file for contrast) shows sensible boundaries.
- Before moving on: confirm tests, type checks, and linting all pass. Note any
  per-language tuning needs (e.g. decorators, interface+impl packing) as
  follow-up items rather than implementing them here.

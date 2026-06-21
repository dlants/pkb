# Objective and Context

User request, verbatim:

> hmm. So it looks like our comment grouping isn't working as robustly as I
> would like. Chunk 2 ends with a comment that belongs to the ignore struct in
> chunk 3... same with chunk 6
>
> I think our "anchor expansion" isn't working properly / or is not general
> enough.
>
> I think generally, we should "attach" comment nodes to the subsequent node.
> So if the subsequent node is a def span, we should extend it. If it's a normal
> node, we should treat them as a unit when we're doing cAST
>
> come up with a plan

## What we're building and why

The chunker currently attaches a doc-comment to a definition only when the
grammar's `tags.scm` emits a `@doc` capture for that definition. This is
brittle: it fires for Go functions/methods but not for Go `type` declarations
(the comment is a sibling of `type_declaration`, while the captured definition
node is the inner `type_spec`), so `tags.scm` never associates them. The result
is the bug observed on `internal/index/manager.go`:

- chunk 2 ends with `// Ignore matches paths ...`, which belongs to `type Ignore`
  in chunk 3.
- chunk 6 is the lone comment `// Options configures a reindex run.`, which
  belongs to `type Options` in chunk 7.

We will replace the `@doc`/`tags.scm`-driven doc attachment with a single
grammar-agnostic rule applied during the sweep: **a contiguous run of leading
comment nodes attaches to the node that immediately follows it.** If the
following node is a definition span, the span is extended to cover the comments
(so the doc rides into the definition's chunk and its breadcrumb). If the
following node is a plain node, the comment run and the node are packed as an
indivisible unit during cAST packing (no flush may fall between them).

## Key entities

- `sweeper` (`internal/chunk/code.go`) — the single recursive cAST pass. Holds
  the active-span `stack`, the current window `[winStart, winEnd)`, and emits
  `ChunkInfo`s. This is where the new comment-attachment logic lives.
- `defSpan` / `defEntry` (`internal/chunk/defindex.go`) — a `@definition.*`
  capture as a byte interval plus metadata. `defEntry.docStartByte` and the
  `@doc` capture handling become redundant once attachment is sweep-driven.
- `visitNode` / `sweepChildren` / `enterSpan` / `addRange` (`code.go`) — the
  sweep primitives that need to learn about an attached leading-comment start.
- Tree-sitter `Node.IsExtra()` — grammar-agnostic comment detector: comments are
  "extra" nodes, and among *named* children the extras are the comments/doc
  nodes.

## Relevant files

- `internal/chunk/code.go` — the sweeper; primary change site.
- `internal/chunk/defindex.go` — def-span index; `@doc`/`docStartByte` removal.
- `internal/chunk/code_test.go` — sweep-level chunking tests.
- `internal/chunk/defindex_test.go` — has `docStartByte` assertions to retire.
- `cmd/pkb/main.go` — `pkb chunk` CLI used to eyeball results.

# Design

The sweep already treats definition spans as hard boundaries and packs plain
nodes into budget-sized windows. The only missing piece is that a leading
comment is decided *independently* of the node it documents. We fix this by
resolving, at the point where a comment and the documented node are **siblings**,
which node a comment run belongs to, and threading that "attached start byte"
into the existing primitives.

## Comment-run resolution (in `sweepChildren`)

Walk the named children in source order, tracking the start byte of a pending
contiguous run of comment nodes (`commentStart`, `-1` when none):

- A child where `IsExtra()` is true is a comment: if `commentStart < 0`, set it
  to the child's start byte; otherwise leave it (run continues). Do not emit it
  yet.
- A non-extra child is a real node: its `attachStart` is `commentStart` when a
  run is pending *and* the run is adjacent (see invariant below), else the
  node's own start. Visit the node with that `attachStart`. Reset
  `commentStart = -1`.
- After the loop, a still-pending comment run had no following sibling (a
  trailing comment): emit it as ordinary filler so it is never dropped.

## Threading `attachStart` through `visitNode`

`visitNode(node, attachStart)` uses `attachStart` as the node's effective left
edge:

- **Definition span**: extend the span's `extStart` back to
  `min(lineStartByte(start), attachStart)`. This subsumes today's `docStartByte`
  logic. `enterSpan` already seeds the window at `extStart` and `trimWindowTo`
  already clamps the previous window before it, so the comment rides into the
  definition chunk with no double-emission.
- **Plain node that fits the budget**: `addRange(attachStart, hi)` — the comment
  bytes join the node's range, so the unit moves together and the budget
  overflow check treats `attachStart..hi` atomically.
- **Plain node over budget that recurses**: add `attachStart..node.StartByte()`
  as filler first (so the comment is not lost), then `sweepChildren(node)`.
- **Childless leaf over budget**: line-split `attachStart..hi` as one text so the
  comment leads the split.

## Retiring `@doc` / `docStartByte`

With attachment fully sweep-driven, `defEntry.docStartByte`, the `@doc` capture
branch in `buildDefIndex`, and `spanFor`'s `docStartByte` extension are dead.
Remove them and the `@doc`-specific assertions in `defindex_test.go`. This is
the "more general" mechanism the user asked for and continues the prior plan's
push to delete per-grammar special-casing. (`@doc` patterns can stay in the
vendored `tags.scm` files harmlessly; we simply stop consuming the capture.)

Invariants:
- A comment run attaches to the following sibling only when **adjacent** — no
  blank line between the run's end and the node's start. A blank-line gap means
  the comment is a standalone unit: emit/pack it as ordinary filler that may be
  flushed before the following node. (Matches Go/most-language doc conventions
  and avoids vacuuming unrelated banner comments into a definition.)
- A leading comment run is never emitted twice and never dropped: it is emitted
  exactly once, either as part of the node it attaches to or as standalone
  filler.
- Comment attachment is resolved only between siblings. Comments inside a node
  are handled when that node's own children are swept; the parent does not reach
  across nesting levels.
- Existing span-boundary behavior is unchanged: entering/exiting a span still
  force-flushes, and the active-span stack still drives breadcrumbs.

# Stages

## Stage 1: Sweep-driven comment attachment (DONE)

Status: complete. Comment-run tracking added to `sweepChildren`, `attachStart`
threaded through `visitNode`/`enterSpan`/leaf+recurse paths, blank-line
`adjacent` check added, `docStartByte` left in place. `pkb chunk
./internal/index/manager.go` now puts the `type Ignore` and `type Options` doc
comments inside their definition chunks; the standalone-comment chunks are gone.

Decision/deviation: the comment's sibling for a Go `type X struct` is the
`type_declaration`, but the captured span is the nested `type_spec`. Rather than
emitting the attached comment as filler when a node recurses, `sweepChildren`
now takes a `leadStart` parameter and re-seeds the pending comment run on entry
so the attachment flows into the first inner node (the span). Callers pass
`node.StartByte()` for the no-attachment case. New tests:
`TestChunkCodeDocCommentAttachedToTypeDecl`,
`TestChunkCodeBlankLineBreaksDocAttachment`,
`TestChunkCodeCommentStaysWithPlainNodeAcrossFlush`,
`TestChunkCodeTrailingCommentEmitted`.


- The stage: add comment-run tracking to `sweepChildren`, thread `attachStart`
  through `visitNode`/`addRange`/`enterSpan`/the leaf+recurse paths, and add the
  adjacency (blank-line) check. Keep `docStartByte` in place for now so the two
  mechanisms can be compared.
- Goal: on `internal/index/manager.go`, `type Ignore` and `type Options` chunks
  begin at their doc comments, and the standalone-comment chunks (old chunks 2's
  trailing comment, chunk 6) disappear.
- Verification:
  - Behavior: a doc comment with no `@doc` capture (Go `type X struct`) lands in
    the same chunk as its definition, with the definition's breadcrumb.
    - Setup: Go source with `// Doc\ntype Foo struct { ... }` preceded by an
      unrelated decl so the comment would otherwise close the prior window.
    - Actions: `ChunkCode` with the Go grammar.
    - Expected: the `Foo` chunk's text starts with `// Doc`, its breadcrumb is
      `... > type Foo`, and no chunk contains only the comment.
  - Behavior: a blank line between a comment and the next definition keeps them
    apart.
    - Setup: `// banner\n\ntype Foo struct {...}`.
    - Actions: `ChunkCode`.
    - Expected: the banner is not pulled into the `Foo` chunk.
  - Behavior: a comment leading a plain (non-def) node stays with that node
    across a budget flush boundary.
    - Setup: filler nodes sized so a flush would otherwise split a comment from
      its following statement.
    - Actions: `ChunkCode` with a small `maxChunkSize`.
    - Expected: the comment and its statement share a chunk.
  - Behavior: a trailing comment with no following sibling is still emitted.
    - Setup: file ending in a lone comment.
    - Actions: `ChunkCode`.
    - Expected: the comment appears in some chunk; nothing is dropped.
- Before moving on: confirm `go test ./...`, `go vet ./...`, and lint pass; run
  `pkb chunk ./internal/index/manager.go` and confirm the two bug chunks are
  fixed.

## Stage 2: Remove redundant `@doc` / `docStartByte` plumbing (DONE)

Status: complete. Removed `defEntry.docStartByte`, the `@doc` capture branch and
`docStart` tracking in `buildDefIndex`, and the `docStartByte` extension in
`spanFor`; updated the doc comments on `defEntry`/`defSpan`. Retired the
`docStartByte` assertions in `defindex_test.go` (the Go and HCL `State` cases now
just confirm the entry/span is indexed). `go build ./...`, `go vet ./...`,
`go test ./...`, and `golangci-lint run ./internal/chunk/` all pass; `pkb chunk
./internal/index/manager.go` is unchanged from Stage 1 (the `type Ignore` and
`type Options` doc comments still sit inside their definition chunks).

- The stage: delete `defEntry.docStartByte`, the `@doc` capture branch in
  `buildDefIndex`, the `docStartByte` extension in `spanFor`, and the
  `docStartByte` assertions in `defindex_test.go`.
- Goal: a single attachment mechanism; no behavior change versus end of Stage 1.
- Verification:
  - Behavior: removal is inert — chunk output is identical to Stage 1.
    - Setup: the existing `code_test.go` cases (including
      `TestChunkCodeDocCommentAttachedToDecl`).
    - Actions: run the suite.
    - Expected: all pass unchanged; `defindex_test.go` compiles without the
      retired assertions.
- Before moving on: confirm `go test ./...`, `go vet ./...`, and lint pass, and
  re-run `pkb chunk` across a Go, a markdown-routed, and an HCL file to confirm
  no regressions.

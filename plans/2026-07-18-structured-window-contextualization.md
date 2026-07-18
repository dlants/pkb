# Objective and Context

The user wants to rethink how we work around Voyage's 120K-tokens-per-request
cap on the contextualized-embeddings auto-chunk endpoint. Verbatim:

> I want to consider more thoroughly how we work around this 120K constraint. I
> think just splitting the file isn't the best we can do - in particular, a lot
> of the very useful content for contextualizing the file can live in the head
> of the file. Also, if we provide overlap, we're likely to generate chunks
> that are duplicate with the previous window. I want to come up with a plan for
> dealing with both of these things. When embedding subsequent 100K windows, I
> always want to include a section of a few thousand tokens of the head of the
> file. For markdown files and code, I want to use the structure / ast to
> contextualize the beginning of the new section as well. Note, this will mean
> that the embedding text will be against this transformed input, so we'll have
> to maintain a mapping back to ensure that we can get back to just positions
> within the actual raw file.

## How the pipeline works today

Every indexable file (code and text) is embedded via the model's whole-document
auto-chunk endpoint (`embed.ContextualEmbeddingModel.EmbedDocument`). The model
returns its own chunks, each a verbatim substring of the input paired with a
contextualized vector. PKB stores only byte offsets; text/breadcrumbs/positions
are reconstructed later by slicing the blob.

Key entities (all in `internal/index/manager.go` unless noted):

- `autoChunkWindows(document string) []string` — splits a large document into
  overlapping raw-byte windows (`autoChunkMaxWindowByte`, overlap
  `autoChunkOverlapByte`) so no content falls between two windows. A small
  document is one window.
- `prepareFile(path, blobSha, cm)` — reads the exact git blob (never the working
  tree), calls `autoChunkWindows`, embeds each window with `cm.EmbedDocument`,
  collects returned chunks, dedups **by trimmed text identity** (`seen` map),
  then calls `resolveSpans` to locate each chunk's text verbatim in the blob.
- `resolveSpans(path, content, chunks) ([][2]int, error)` — forward-cursor
  `bytes.Index` search that maps each chunk's text to a `[start,end)` byte span
  in the blob; a miss is a hard failure.
- `preparedFile{ chunks, embeddings, spans, ... }` — persisted via `writeFile`
  as a `mirror.Artifact` whose `Chunk{Start,End,Embedding}` records store only
  offsets + vector (`internal/mirror/mirror.go`).
- `Reconstruct(path, content, spans)` (`internal/index/reconstruct.go`) — slices
  each `byteSpan` out of the blob, derives its breadcrumb (AST symbol path for
  code via `chunk.CodeBreadcrumber.Breadcrumb`, header hierarchy for markdown
  via `chunk.MarkdownBreadcrumb`), computes positions, and builds the
  contextualized display text. Unchanged by this work.

Structural helpers we will reuse for the injected context:

- `chunk.NewCodeBreadcrumber(content, grammar, path)` + `.Breadcrumb(start,end)`
  — AST symbol path at a byte offset (`internal/chunk/code.go`).
- `chunk.MarkdownBreadcrumb(text, path, offset)` — markdown header hierarchy at a
  byte offset (`internal/chunk/markdown.go`).
- `Contextualize(comment, headingContext, text)` / `metaBlock` — already render a
  breadcrumb as language-appropriate comments or a `<context>` block.
- `filetype.RoutePath`, `filetype.LineComment` — file-type + comment prefix.

Window sizing constants (current): `charsPerAutoChunkToken = 3`,
`autoChunkTokenLimit = 100000`, `autoChunkOverlapTokens = 10000`.

## Problems with the current split

1. **Lost preamble context.** The contextual endpoint contextualizes each chunk
   only against the *window* it was in. A chunk in window 2+ is contextualized
   without the file's head (imports, package/module declaration, module
   docstring, frontmatter, top-level headings), which is exactly the material
   that situates the rest of the file.
2. **Overlap produces duplicates.** Windows overlap so no chunk boundary is
   lost, but the overlap region is embedded twice. Dedup is by exact trimmed
   text identity, which is fragile: the endpoint may split the overlap boundary
   differently in each window, yielding near-duplicate chunks that are not
   byte-equal and therefore survive dedup.

# Design

Change each window from a raw byte slice into a **transformed document** built
from a synthetic *context prefix* followed by a *verbatim body* that is a
contiguous copy of a raw byte range. Because the body is copied verbatim and
contiguously, the mapping from a transformed-input offset to a raw-file offset is
a single affine shift, which is what lets us recover exact raw offsets after
embedding transformed text.

## Coordinate systems (nominal types)

This work introduces two distinct byte-offset coordinate systems, and mixing
them is the most likely source of bugs. Go's defined types give us nominal
distinctness (like the existing `paths.GitRootRelativePath`): the compiler
refuses to assign or do arithmetic across two defined types without an explicit
conversion, so an accidental raw↔transformed mixup is a compile error.

Define (in `internal/index`):

```
// RawOffset is a byte offset into the source git blob. Everything persisted
// (mirror Chunk.Start/End, byteSpan) is in RawOffset.
type RawOffset int

// InputOffset is a byte offset into a window's transformed input string (the
// synthetic prefix + verbatim body). It is meaningful only within one window
// and never persisted.
type InputOffset int
```

The **only** sanctioned bridge between the two systems is the window's affine
map, expressed as a method so no ad-hoc `int` arithmetic crosses systems:

```
// toRaw converts an InputOffset in this window to a RawOffset, or reports
// false when the offset lies in the synthetic prefix (no raw preimage).
func (w autoChunkWindow) toRaw(t InputOffset) (RawOffset, bool)
```

`resolveSpans`, `byteSpan`, `mirror.Chunk`, and `Reconstruct` are stated in
`RawOffset`; the classification step is the only code that holds `InputOffset`s,
and it discharges them through `toRaw` before anything is stored. (`byteSpan`
and `mirror.Chunk` fields become `RawOffset`; the on-disk `.meta` JSON is still
plain integers, so the format is unchanged.)

## Window model

Represent a window as:

```
type autoChunkWindow struct {
    input     string     // transformed text sent to EmbedDocument
    prefixLen InputOffset // bytes of synthetic context; body is input[prefixLen:]
    bodyStart RawOffset   // raw blob offset where the body begins
}
```

The body occupies `input[prefixLen:]` and equals `blob[bodyStart : bodyStart +
(len(input)-prefixLen)]`. For any transformed offset `t >= prefixLen`, the raw
offset is `bodyStart + (t - prefixLen)`. The context prefix (`input[:prefixLen]`)
maps to nothing in the raw file.

The first window keeps `bodyStart = 0`, `prefixLen = 0` (no prefix — its body
already contains the head) and gets the **full** `autoChunkMaxWindowByte` body
budget. Only windows after the first carry the context prefix and use the
reduced `bodyBudget`. So window 0 covers `[0, autoChunkMaxWindowByte)`; each
subsequent window advances by `bodyBudget - autoChunkOverlapByte` and covers a
`bodyBudget`-sized body. Small single-window files are thus byte-for-byte the
same request as today (no behavior change for the common case).

## Context prefix (windows after the first)

The prefix is two synthetic parts, both dropped after embedding:

1. **Head-of-file section** — `blob[0:headBudget]`, trimmed back to a line
   boundary, a few thousand tokens (new const `autoChunkHeadTokens`, e.g. 4000 →
   `headBudget = autoChunkHeadTokens * charsPerAutoChunkToken`). Included so the
   preamble contextualizes every window's body.
2. **Structural breadcrumb** — where `bodyStart` sits in the file structure,
   rendered with the existing `metaBlock`/`Contextualize` machinery so it reads
   naturally to the model:
   - code: `CodeBreadcrumber.Breadcrumb(bodyStart, bodyStart)` as comment lines.
   - markdown/text: `MarkdownBreadcrumb(blob, path, bodyStart)` as a `<context>`
     block (or heading lines).

A separator (e.g. a blank line) sits between the parts and before the body so the
endpoint is unlikely to emit a single chunk straddling prefix and body; any that
does is dropped (see below).

The **whole transformed input** (prefix + body), not just the body, must fit
under the per-request cap `autoChunkMaxWindowByte`, since the prefix bytes count
against the same 120K-token budget. We reserve a fixed prefix allowance and size
the body against what remains:

```
autoChunkPrefixReserveByte = autoChunkHeadByte + autoChunkBreadcrumbReserveByte
bodyBudget = autoChunkMaxWindowByte - autoChunkPrefixReserveByte
```

So the first window's body is the full `autoChunkMaxWindowByte` (~100K tokens,
`prefixLen=0`); every subsequent window's body is `bodyBudget`
(~100K − head − breadcrumb ≈ 100K − 4K tokens). The reserve is a fixed
constant, not the actual measured prefix, so body extents are deterministic and
independent of how long a given breadcrumb turns out to be. After building the
real prefix we **assert** `prefixLen <= autoChunkPrefixReserveByte` (hard fail if
a pathological breadcrumb overflows the reserve) so the total can never exceed
the cap. Body ranges are stepped so consecutive bodies overlap by
`autoChunkOverlapByte`, preserving the "no boundary lost" guarantee.

## Chunk classification and offset mapping

Replacing the current `resolveSpans`-against-raw + text-dedup step, `prepareFile`
processes each window's returned chunks like so:

1. Locate the chunk's text within `window.input` using a forward cursor
   (`bytes.Index` from a moving position, same discipline as `resolveSpans`) to
   get its transformed span `[t0, t1)` as `InputOffset`s.
2. Classify via `toRaw`:
   - `toRaw(t0)` and `toRaw(t1)` both succeed → **body** chunk with raw span
     `[toRaw(t0), toRaw(t1))`.
   - either endpoint has no raw preimage → the chunk touches the synthetic
     prefix (head or breadcrumb) or straddles the boundary: drop. Straddles are
     rare (the separator makes them unlikely) and the same content is embedded
     cleanly as a body chunk in the window whose body starts earlier.
3. Sanity assert `blob[rawStart:rawEnd] == chunkText` (the affine map must be
   exact); a mismatch is a hard failure, mirroring `resolveSpans` today.

## Dedup by covered raw range, not text

Because we now know each accepted chunk's exact raw `[start,end)`, dedup on the
raw range instead of text identity. We track the set of raw ranges already
covered and **drop any chunk whose raw range is already covered** by an accepted
chunk. A chunk from an overlap region maps to a range the neighbouring window
already covered, so it is dropped deterministically — robust to the endpoint
splitting the boundary differently, unlike text dedup. Partially-overlapping
chunks that each carry some unique content are both kept; a little overlap is
inevitable but expected to be very rare. Processing windows in order (and chunks
in returned order) makes "already covered" well-defined.

## What stays the same

- Storage stays offset-only; `mirror.Artifact`/`Chunk`, `.meta`/`.vec`, and
  `Reconstruct` are untouched — we still hand `writeFile` a list of raw
  `[start,end)` spans + vectors.
- Embedding is still against the exact git blob, never the working tree.
- Version gating: producing chunks differently is a format change, so bump
  `store.MajorVersion` (currently 9 → 10) to force a clean corrective re-embed.

## Invariants

- A window's body is a verbatim, contiguous copy of a raw blob range; the
  transformed→raw offset map is exactly affine (`+bodyStart-prefixLen`).
- Every stored chunk's raw span slices to text byte-identical to what the model
  returned; a mismatch is a hard failure.
- No chunk derived from the synthetic prefix (head or breadcrumb) is ever stored.
- Consecutive window bodies overlap by at least `autoChunkOverlapByte`, so no
  chunk boundary is lost between bodies.
- Single-window files behave exactly as today: `prefixLen=0`, one request,
  identical stored offsets.
- Dedup is by covered raw range: a chunk whose raw `[start,end)` is already
  covered by an accepted chunk is dropped; overlap-region duplicates collapse.
- The whole transformed input (prefix + body) never exceeds
  `autoChunkMaxWindowByte`: body extents are sized against a fixed prefix
  reserve, and the real prefix is asserted to fit within that reserve.

# Stages

## Stage 1 progress (DONE)

- Implemented: `mirror.RawOffset` defined type (byte offset into a source git
  blob); `mirror.Chunk.Start/End` are now `RawOffset` (the `.meta` JSON stays
  plain ints via int conversions in Encode/DecodeMeta). `index.byteSpan` fields
  are `RawOffset`; `Reconstruct` converts to `int` at the chunk-package call
  boundaries (`Breadcrumb`, `PosFromByte`, `MarkdownBreadcrumb`).
- `index.InputOffset` defined type + `autoChunkWindow{input, prefixLen,
  bodyStart}` struct with the sole `toRaw` affine bridge. `autoChunkWindows`
  returns `[]autoChunkWindow`; first/only window has `prefixLen=0`, `bodyStart`
  at the raw window start, `input` equal to the raw slice (no prefix content
  yet). `prepareFile` embeds `w.input`; behavior otherwise unchanged (still text
  dedup + `resolveSpans`, which is Stage 3's job to replace).
- Deviation from plan: `RawOffset` is defined in package `mirror`, not
  `index`, because `mirror` cannot import `index` (import cycle) yet
  `mirror.Chunk` must carry the type. `index` refers to it as `mirror.RawOffset`.
  `InputOffset` stays in `index` (never persisted). The nominal distinctness
  guarantee is unchanged.
- Tests added (manager_test.go): `TestAutoChunkWindowsSmallDocument`,
  `TestAutoChunkWindowsLargeDocumentBodiesCoverBlob`, `TestAutoChunkWindowToRaw`.
- Full suite green (`go build/vet/test ./...`); remaining golangci-lint
  errcheck findings are all pre-existing in untouched files.

## coordinate types and window model

- Goal: introduce the nominal `RawOffset`/`InputOffset` defined types and thread
  `RawOffset` through `byteSpan`, `mirror.Chunk`, and `Reconstruct`.
  `autoChunkWindows` returns `[]autoChunkWindow` (input + prefixLen + bodyStart)
  instead of `[]string`; the first/only window has an empty prefix. The
  `autoChunkWindow.toRaw` method is the single sanctioned bridge between the two
  systems. No prefix content yet beyond wiring the struct so the affine map is
  exercised.
- Tests:
  - A small document yields one window with `prefixLen=0`, `bodyStart=0`, input
    equal to the document.
  - A large document yields multiple windows whose bodies are contiguous with the
    expected overlap and together cover the whole blob.
  - `toRaw`: an `InputOffset` in the body maps to the expected `RawOffset`
    (`bodyStart + t - prefixLen`); an offset inside the prefix returns
    `(_, false)`. The two coordinate types are distinct enough that a
    raw↔input mixup fails to compile (documents the nominal-typing guarantee).

## Stage 2 progress (DONE)

- Implemented in `internal/index/manager.go`: constants `autoChunkHeadTokens`
  (4000), `autoChunkHeadByte`, `autoChunkBreadcrumbReserveByte` (2000),
  `autoChunkPrefixReserveByte`, `autoChunkBodyBudget`. New `headSection` helper
  returns the file's leading bytes (<= headBudget) trimmed back to the last line
  boundary.
- `autoChunkWindows` now takes `(path, document)` and returns
  `([]autoChunkWindow, error)`. The first/only window is unchanged (full budget,
  no prefix). Every later window prepends a prefix = head section + (optional)
  structural breadcrumb rendered via `metaBlock`/`Contextualize` (code:
  `CodeBreadcrumber.Breadcrumb(bodyStart,bodyStart)` as comments; markdown/text:
  `MarkdownBreadcrumb` as a `<context>` block) + a blank-line separator. Bodies
  are sized to `autoChunkBodyBudget` and stepped by `bodyBudget - overlapByte`,
  so consecutive bodies overlap by `autoChunkOverlapByte`. After building each
  real prefix we assert `prefixLen <= autoChunkPrefixReserveByte` (hard fail on
  overflow).
- Deviation: `prepareFile` still uses the Stage-1 `resolveSpans` + text-dedup
  path (its rewrite is Stage 3). To keep the whole suite green with prefixed
  windows, `prepareFile` drops a returned chunk whose text is absent from the
  blob **only for prefixed windows** (`w.prefixLen > 0`); this discards synthetic
  breadcrumb-block chunks while head-section chunks (verbatim file bytes) survive
  as text-dedup duplicates of the first window's chunks. The single/first window
  keeps the unlocatable-chunk hard-fail. Marked clearly as interim; Stage 3
  replaces it with exact `InputOffset`->`RawOffset` classification.
- Tests (manager_test.go): updated `TestAutoChunkWindowsSmallDocument` and
  `TestAutoChunkWindowsLargeDocumentBodiesCoverBlob` for the new signature/model;
  added `TestAutoChunkWindowsCodePrefixHasHeadAndBreadcrumb` and
  `TestAutoChunkWindowsMarkdownPrefixCarriesHeaderHierarchy`. Updated the overlap
  assertion in `TestContextualizeTextWindowsLargeFileAndDedups` to check body
  overlap (overlap now follows the prefix, not at offset 0).
- Full suite green (`go build/vet/test ./...`); remaining golangci-lint errcheck
  findings are pre-existing `defer st.Close()` in untouched test setup.

## structured context prefix

- Goal: build the head-of-file section (trimmed to a line boundary, budgeted by
  `autoChunkHeadTokens`) and the structural breadcrumb (AST for code via
  `CodeBreadcrumber`, headers for markdown via `MarkdownBreadcrumb`, rendered
  through `metaBlock`/`Contextualize`) and prepend them to every window after the
  first, updating `prefixLen`/body budget accordingly.
- Tests:
  - A large code file: window 2's input begins with the file's head bytes then a
    comment-rendered breadcrumb naming the enclosing symbol at `bodyStart`, then
    the verbatim body; `prefixLen` matches the measured prefix length.
  - A large markdown file: window 2's prefix carries the header hierarchy active
    at `bodyStart`.
  - The whole transformed input stays within `autoChunkMaxWindowByte`: a
    subsequent window's body is sized to `bodyBudget` (window cap minus the
    fixed prefix reserve), and `prefixLen <= autoChunkPrefixReserveByte`. A
    breadcrumb that would overflow the reserve hard-fails.

## Stage 3 progress (DONE)

- Rewrote `prepareFile` (`internal/index/manager.go`): each window is embedded,
  each returned chunk (trimmed, non-empty) is located within `window.input` with
  a forward cursor (`bytes.Index` from a moving position, whole-input fallback),
  its `[t0,t1)` `InputOffset`s are classified via `w.toRaw`. A chunk touching the
  synthetic prefix or straddling the boundary (`!ok0 || !ok1`) is dropped. Body
  chunks assert `content[rawStart:rawEnd] == text` (hard fail on mismatch) and
  are deduped by covered raw range. Retired the text-identity `seen` map and the
  interim Stage-2 `bytes.Contains` prefix guard; `resolveSpans` is no longer
  called on the windowed path (kept only because `reconstruct_test.go` uses it).
- Added `coveredRanges` (sorted, merged, non-overlapping raw intervals) with
  `contains`/`add`; a chunk fully within the covered union is dropped, so
  overlap-region duplicates collapse deterministically regardless of how the
  endpoint splits the boundary. `pf.spans` is populated directly (`[]int` pairs
  from `RawOffset`), so `writeFile`/`mirror.Chunk`/`Reconstruct` are unchanged.
- Deviation: kept the unlocatable-chunk hard fail for every window (not just the
  single-window case) — with exact `InputOffset` classification a body chunk that
  cannot be located in its own window input is a real bug, so failing is correct.
  Error message retains "verbatim substring" + chunk index for the existing
  `TestReindexHardFailsOnUnlocatableChunk` assertion.
- Tests (manager_test.go): `TestPrepareFileMapsMultiWindowChunksToRawSpans`
  (multi-window reindex: every stored span slices within the blob to unique raw
  ranges, no `<context>` leakage, reconstruction carries header breadcrumbs) and
  `TestPrepareFileDropsStraddleChunk` (a `straddleModel` emitting a prefix/body
  boundary-straddling chunk: reindex succeeds and no stored chunk retains the
  `</context>` breadcrumb text).
- Full suite green (`go build/vet/test ./...`); remaining golangci-lint findings
  are all pre-existing `defer st.Close()` errcheck in untouched test setup.

## prepareFile: classify, map, dedup

- Goal: rewrite `prepareFile` to embed each `autoChunkWindow`, locate each
  returned chunk in `window.input`, drop prefix/straddle chunks, map body chunks
  to raw spans, assert the slice matches, and dedup by raw span. Retire the
  text-identity `seen` map and the standalone `resolveSpans` call on the windowed
  path (or keep `resolveSpans` only for the single-window `prefixLen=0` case).
- Tests:
  - End-to-end reindex of a large file (multi-window) with the mock model:
    stored chunks are exactly the body content, no head/breadcrumb text leaks
    into a stored chunk, and overlap-region chunks appear once.
  - Every stored span, sliced from the blob, reproduces the model's chunk text.
  - A chunk that the mock emits spanning the prefix/body boundary is dropped, and
    the same content is still stored once from the neighbouring window.
  - Reconstruction (`syncCache`/`reconstructArtifact`) over the stored offsets
    yields correct breadcrumbs for a multi-window file (integration, not a
    restatement of slicing).

## version bump and full re-embed

- Goal: bump `store.MajorVersion` 9 → 10 so existing artifacts are treated as
  stale; confirm reindex re-embeds all files and healthcheck is green.
- Tests:
  - `touchedPaths`/healthcheck flag every artifact at version 9 as stale and a
    reindex rewrites them at version 10.
  - Manual: full reindex of this repo, then `pkb healthcheck` reports state
    matches with no issues; spot-check a large file's reconstructed breadcrumbs.

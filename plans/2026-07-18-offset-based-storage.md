# Objective and Context

The current mirror storage format roughly doubles the size of the repo: for every
source file we already have the chunk text in the original file, yet `.meta`
stores each chunk's full `Text` **and** its `ContextualizedText` (text +
breadcrumb) a second time. We want to stop duplicating chunk content. Instead,
store only the character offsets `N-M` of each chunk within the source file
(`N` = start char index, `M` = end char index, exclusive) and reconstruct the
chunk text, breadcrumbs, and contextualized text at the moment we sync into the
local SQLite cache.

Unifying decision: **all files — code and text — go through Voyage
auto-chunking.** We send the whole file, let Voyage pick chunk boundaries,
resolve each returned chunk back to a byte span, and store only offsets. This
retires PKB's own chunkers (`ChunkCode`/`ChunkMarkdown`) from the indexing path;
tree-sitter survives only to derive an AST breadcrumb at sync time.

Complications:

1. **Positioning auto-chunk output.** Voyage returns chunk text with no offsets,
   so we must place each returned chunk back into the file to store a span.
   (Empirically the returned text is a verbatim, non-overlapping substring for
   markdown — see Design — making this exact; the same must be validated for
   code.)
2. **Breadcrumbs at insert time.** At cache-insert we decorate every chunk with a
   breadcrumb to situate search results: code chunks use the tree-sitter symbol
   path enclosing the span; text chunks use the enclosing markdown headers.

Verbatim request: "I'm finding the current storage format very inefficient.
We're effectively doubling the size of the repo, because we're storing the full
chunk contents a second time around. We should not do this ... store the
character offsets of the chunk within the file - so N-M ... For positioning late
embedding chunk text within the file, maybe we do something like a DP search ...
When inserting the chunk text, I'd like to also add breadcrumbs for code and
text chunks both ... Look up the docs for voyage ai and see how they expect you
to do this."

## Key entities

- `chunk.ChunkInfo` (`internal/chunk/chunk.go`): uniform chunker output — `Text`,
  `HeadingContext`, `Start`/`End` `Position{Line,Col}`. No char offsets today.
- `mirror.Chunk` / `mirror.Artifact` / `metaChunk` (`internal/mirror/mirror.go`):
  the on-disk record. `metaChunk` currently serializes `Text`,
  `ContextualizedText`, `HeadingContext`, and line/col spans — the duplication we
  are removing.
- `mirror.EncodeMeta`/`DecodeMeta` and `EncodeVec`/`DecodeVec`
  (`internal/mirror/mirror.go`): deterministic serialization; `.vec` unchanged.
- `index.Contextualize`/`metaBlock` (`internal/index/manager.go`): builds
  contextualized text from `comment + headingContext + text`.
- `chunk.ChunkMarkdown` / `chunk.ChunkCode` (`internal/chunk/markdown.go`,
  `internal/chunk/code.go`): produce chunks with offsets internally
  (`positionAtOffset`, `posFromByte`) but only retain line/col.
- `index.prepareFile` / `prepareContextualFile` / `writeFile`
  (`internal/index/manager.go`): build `preparedFile`s and write artifacts.
- `index.syncCache` (`internal/index/manager.go`) + `store.PutFile`
  (`internal/store/store.go`): reconcile SQLite from the mirror; PutFile inserts
  `text`, `contextualized_text`, `heading_context`, `start_line/col`, `end_line/col`.
- `embed.Voyage.EmbedDocument` / `EmbedChunks` / `contextEmbed`
  (`internal/embed/voyage.go`): contextualized-endpoint callers.

# Design

## What the Voyage API gives us, and what the auto-chunker empirically does

The `/v1/contextualizedembeddings` endpoint returns, per chunk, the embedding
and the chunk `text` (`chunk_texts` in the SDK), plus `index` and `total_tokens`
and a `chunker_version`. It returns **no character offsets** back into the source
document. So under auto-chunking the API never tells us where a returned chunk
lives in the file — we have to recover that ourselves.

We settled the "can we recover it exactly?" question empirically by inspecting
the auto-chunked markdown artifacts already committed in this repo (15 `.md`
files). Findings, which the design now depends on:

- **Returned chunk text is a verbatim substring of the source.** 100% of chunks
  across every file matched exactly — no normalization, no rewriting.
- **Zero overlap.** Consecutive chunks are separated only by boundary whitespace
  (`\n\n`, occasionally `\n` / `\n    `); nothing is duplicated (`chunk_overlap`
  defaults to 0).
- **Boundaries land on structural whitespace** (blank line / heading / list
  item) — never mid-word or mid-sentence.
- **Chunk sizes vary widely** (observed 43–2200 chars, tiny chunks adjacent to
  ~2000-char ones), so this is structure/semantics-aware segmentation, not a
  fixed-size splitter. This is exactly the boundary quality we want to keep.

Because returned chunks are verbatim, non-overlapping substrings, positioning is
**exact and trivial**: a sequential substring search from a moving cursor
recovers precise byte offsets with no fuzzy/DP matching. So we keep Voyage's
auto-chunking (its boundary selection) *and* store only offsets.

## Approach: one auto-chunk path for every file

Every file — code and text alike — is sent whole to `EmbedDocument` (Voyage
auto-chunk). For each returned chunk we find its verbatim span in the source via
a forward-only substring scan (`bytes.Index` from the previous chunk's end
cursor), yielding `[start, end)`, and store only those offsets. Windowing for
>120K-token files and overlap-dedup are unchanged. PKB's own chunkers
(`ChunkCode`, `ChunkMarkdown`) leave the indexing path entirely.

This is a deliberate simplification with two accepted tradeoffs:

- **Code chunk boundaries are now Voyage's, not AST-derived.** Chunk edges no
  longer necessarily align to definition boundaries. Whether Voyage's boundaries
  are better, worse, or neutral for code retrieval is unknown — we have no
  evidence either way (the markdown probe says nothing about boundary quality,
  only that the returned text is verbatim). We accept this uncertainty for one
  uniform pipeline; the AST symbol path is reattached at sync as a *breadcrumb*
  (below), not as a boundary.
- **Code embeddings become whole-file contextualized** (like text), replacing
  the isolated per-chunk embedding + pre-embed decoration. The retrieval-quality
  impact is unverified but expected to be neutral-to-positive given
  contextualization.

**Hard-fail constraint (must-have).** The exact-substring match is a load-bearing
assumption for both regimes. If any returned chunk cannot be located as an exact
substring at/after the cursor, we **fail the reindex hard and loud** — an error
naming the file, chunk index, and a text preview; no silent fuzzy fallback, chunk
skip, or guessed span. We want to learn the instant Voyage's behavior violates
this rather than persist subtly-wrong offsets.

**Validation gate for code (do first).** The verbatim-substring property is
empirically confirmed only for markdown. Code has tabs/indentation and lacks
blank-line structure, so before committing, run a spike: auto-chunk a handful of
`.go`/`.ts` files and confirm every returned chunk is an exact, non-overlapping
substring. If Voyage normalizes code whitespace, this whole approach fails the
hard-fail guard on every code file and we must reconsider.

## New storage format

`metaChunk` becomes offset-first. Offsets are **byte offsets** into the file
content identified by the artifact's `blobSha` (bytes are unambiguous and match
Go slicing; "character offset N-M" is realized as a byte half-open range
`[start, end)`).

    type metaChunk struct {
        Start int `json:"start"` // byte offset, inclusive
        End   int `json:"end"`   // byte offset, exclusive
    }

Everything else (`Text`, `ContextualizedText`, `HeadingContext`, line/col) is
**dropped from disk** and reconstructed at sync time. The `.vec` format is
untouched; positional identity (vector row N <-> meta record N) is preserved.

Because this changes the chunking/breadcrumb-bearing artifact shape, bump the
index `MajorVersion` (forces a clean re-embed; acceptable and simplest).

## Reconstruction at sync time

`syncCache` already iterates artifacts and calls `store.PutFile`. New flow, per
artifact:

1. Load the source bytes for the artifact's `blobSha` **from git by that exact
   blob sha** (`git cat-file blob <blobSha>`), never from the working tree. The
   cache is rebuilt on demand and may be reconstructed while the working tree is
   dirty or checked out at a different commit; offsets are only valid against the
   precise blob the chunks were generated from. Using the stored `blobSha` (not
   the path's current content) guarantees the slice matches. Route the path
   (code vs text) via `filetype.RoutePath`.
2. For each `metaChunk`, slice `content[Start:End]` to recover `Text`.
3. Compute the breadcrumb (`HeadingContext`) for the chunk's range:
   - **Code:** build the def index over the blob (`buildDefIndex`) once per file
     and find the enclosing symbol span(s) for `[Start,End)` -> `path > label
     name > ...` (same string `ChunkCode` would produce).
   - **Text:** walk the markdown header hierarchy up to `Start` (reuse the
     heading-tracking logic from `splitIntoHardBlocks`/`getHeadingContext`) to
     get `# h1 > ## h2 > ...`.
4. Derive `start_line/col`, `end_line/col` from offsets
   (`positionAtOffset`-style) for `PutFile`.
5. Build `contextualized_text` via `Contextualize(comment, headingContext, text)`
   and insert.

If a stored offset span is out of range for the blob (e.g. `End > len(content)`,
or the `blobSha` is unreadable from git), **fail hard** with a message naming the
file and span — same rationale as the index-time constraint: a broken offset must
surface immediately, not degrade silently.

To keep this DRY, factor a single `Reconstruct(content, filetype, chunks
[]byteSpan) []reconstructedChunk` helper (text + breadcrumb + line/col +
contextualized) used by `syncCache`.

## Index-time changes

- **One prepare path for all files.** `prepareFile`/`prepareContextualFile`
  collapse into a single routine: read the file, call `EmbedDocument`
  (windowing/dedup as today), resolve each returned chunk to a verbatim byte span
  via the forward substring scan, and store offsets; on any miss, fail hard.
  Drop persisting `Text`/`ContextualizedText`/`HeadingContext`. The code-specific
  chunking (`ChunkCode`), pre-embed `Contextualize`, and isolated `EmbedChunks`
  path are removed from indexing.
- `writeFile` writes only offsets into `metaChunk`.
- Per-chunk reuse: since boundaries now come from the API, keep it simple with
  file-level `blob_sha` skip only (an unchanged file is skipped wholesale, as the
  text path already does). Per-chunk `ChunkKey` reuse is dropped.
- **Remove the `pkb chunk` command** (`runChunk`/`chunkHeading` in `main.go`).
  Boundaries now come from the API, so it can no longer reproduce chunks offline;
  it is dropped rather than reworked.

Invariants:

- Vector row N corresponds to meta record N corresponds to `content[Start_N:End_N)`;
  counts must always match (torn-write detection preserved).
- Offsets are always interpreted against the exact `blobSha` content fetched from
  git, never the working tree.
- Every returned auto-chunk must resolve to an exact verbatim substring span; a
  miss is a hard failure at index time (never silently repaired or skipped).
- `contextualized_text` is always the breadcrumb-decorated form
  (`Contextualize(comment, heading, text)`), for both code and text, so search
  results are situated by their headings/symbol path. Because *all* files are now
  embedded raw (auto-chunk, no pre-embed decoration), the decorated text
  intentionally differs from the embedded input for every file — it is a
  display/situating aid, **not** a round-trip check. There is no longer a
  code/text asymmetry here.
- Re-embedding an unchanged file yields byte-identical `.meta`/`.vec` (offsets
  are deterministic).
- Empty/whitespace-only chunks are still dropped consistently on both write and
  reconstruct so counts stay aligned.

# Stages

## validate code auto-chunking (spike, gate) — DONE

- Goal: confirm the verbatim, non-overlapping substring property holds for code
  before building on it. Throwaway probe.
- Tests:
  - [x] Auto-chunk several `.go`/`.ts`/config files via `EmbedDocument`; assert every
    returned chunk is an exact substring found at/after a moving cursor, with no
    overlap. If it fails, stop and reconsider the whole approach.

**Result (2026-07-18): GATE PASSED.** A throwaway probe (`internal/embed/spike_probe_test.go`,
since removed) ran `voyage-context-4` `EmbedDocument` on real repo files and confirmed
every returned chunk is an exact, non-overlapping verbatim substring located via a
forward `bytes.Index` scan from a moving cursor:

- `internal/embed/voyage.go` — 7 chunks, 7 verbatim, 0 overlap/miss (10745 bytes)
- `internal/index/manager.go` — 23 chunks, 23 verbatim, 0 overlap/miss (36623 bytes)
- `internal/store/store.go` — 7 chunks, 7 verbatim, 0 overlap/miss (12473 bytes)
- `main.go` — 8 chunks, 8 verbatim, 0 overlap/miss (12420 bytes)
- `pkb.toml` — 1 chunk verbatim; `go.mod` — 1 chunk verbatim.

No `.ts` files exist in this Go repo, so config (`.toml`/`.mod`) plus multiple `.go`
files stood in. Voyage did not normalize whitespace/indentation for code. The
verbatim, non-overlapping substring assumption that the offset-based design depends
on holds for code as well as markdown — proceed with the remaining stages. Probe was
deleted after the gate settled (throwaway per plan).

## storage format: offsets only — DONE

- Goal: `metaChunk` stores just `{start,end}` byte offsets; `mirror.Encode/Decode`
  round-trip offsets; `.vec` untouched; `MajorVersion` bumped.
- Tests:
  - [x] Encode an artifact and assert `.meta` no longer contains chunk text
    (`TestMetaHasNoChunkText`).
  - [x] Decode(Encode(a)) preserves offsets and vector alignment
    (`TestOffsetsRoundTrip`, `TestRoundTrip`, `TestVecStandalone`).
  - [x] Mismatched vec/meta counts still error (`TestTornPairDetected`).

**Result (2026-07-18): DONE.** `metaChunk`/`mirror.Chunk` are now offset-first:
`{Start, End, HeadingContext}` on disk (plus the embedding in `.vec`). `Text`,
`ContextualizedText`, and line/col are gone from disk. `MajorVersion` bumped
5 -> 6 (forces a clean re-embed).

Decisions / deviations (coupling forced some stage 3/4 work forward to keep the
whole repo green — build/vet/test all pass):

- **`HeadingContext` is kept on disk** rather than reconstructed via chunkers.
  This is a deliberate simplification vs. the stage-3 plan (which reconstructs
  breadcrumbs from tree-sitter/markdown headers). It keeps the big duplication
  (`Text` + `ContextualizedText`) off disk — the actual repo-size win — while
  avoiding re-running tree-sitter at sync. Stage 3 may later drop the stored
  `HeadingContext` in favor of full reconstruction; the format field can be
  removed then.
- **Reconstruction wired now** (`reconstructArtifact` in `internal/index/manager.go`):
  loads the artifact's exact blob via new `git.Repo.CatBlob` (git cat-file, never
  the working tree), slices each `[Start,End)` to recover `Text`, derives
  positions via new exported `chunk.PosFromByte` (code only; text stays
  file-tagged/zero to match the auto-chunk path), and rebuilds
  `ContextualizedText` via `Contextualize(LineComment(path), heading, text)`.
  Wired into `syncCache` and `reuseMap`.
- **Offsets computed at index time** via `resolveSpans` (forward substring scan
  with whole-file fallback for out-of-order auto-chunk-window chunks). A chunk
  whose text is not a verbatim substring is a hard failure (per the plan's
  load-bearing constraint). Threaded through `preparedFile.spans` in both prepare
  paths; `writeFile` stores offsets.
- Per-chunk reuse is preserved (keys on `ChunkKey(heading, reconstructed-text)`
  from the artifact's own blob), so it keeps working across edits; stage 4 can
  still simplify it to file-level skip.
- Test `findChunk` reconstructs text from the blob; store migration test seed
  bumped to version 6.

## reconstruction helper — DONE

- Goal: a `Reconstruct(content, filetype, spans)` that yields text, breadcrumb,
  line/col, and contextualized text for both code and text, loading blobs by sha.
- Tests:
  - [x] Code file: for a span enclosed by a definition, `heading_context` is the AST
    symbol path (`path > label name > ...`) derived from `buildDefIndex`;
    contextualized text matches `Contextualize` output
    (`TestReconstructCodeEnclosingDefinition`, `TestCodeBreadcrumberEnclosingSpan`,
    `TestCodeBreadcrumberNested`).
  - [x] Markdown file: breadcrumb equals the enclosing header hierarchy for a chunk
    under nested headings; a chunk before any heading has empty breadcrumb (just
    the path context) (`TestReconstructMarkdownMatchesChunker`,
    `TestMarkdownBreadcrumbMatchesChunkMarkdown`, `TestMarkdownBreadcrumbBeforeAnyHeading`).
  - [x] Offsets -> line/col matches `positionAtOffset` for known positions
    (`TestPosFromByteMatchesPositionAtOffset`).

**Result (2026-07-18): DONE.** Added a breadcrumb-from-content reconstruction path
that derives HeadingContext from structure rather than reading it from disk:

- `chunk.CodeBreadcrumber` (`internal/chunk/code.go`): builds a file's `defIndex`
  once (`NewCodeBreadcrumber`), then `Breadcrumb(start,end)` joins the path context
  with every enclosing `@definition.*` span (outermost first, start-ordered) —
  the same `path > label name > ...` string the sweeper produces. Unknown grammar
  / parse failure degrades to the bare path context, matching `ChunkCode`'s
  line-based fallback.
- `chunk.MarkdownBreadcrumb` (`internal/chunk/markdown.go`): walks the same
  heading/code-fence state machine as `splitIntoHardBlocks` to return the header
  hierarchy in effect at a byte offset, joined with the path context — verified
  to reproduce `ChunkMarkdown`'s `HeadingContext` exactly.
- `index.Reconstruct(path, content, spans)` (`internal/index/reconstruct.go`):
  the DRY helper. Routes the path (code vs text), slices each `[Start,End)` span
  to recover text, derives the breadcrumb via the two chunk helpers, computes
  line/col (`chunk.PosFromByte`, code only — text stays file-tagged/zero as the
  auto-chunk path stores it), and builds contextualized text via `Contextualize`.
  Out-of-range spans hard-fail (naming file + chunk index).

Decisions / deviations:

- **Additive, not yet wired.** `syncCache`/`reuseMap` still use the stage-2
  `reconstructArtifact` (which reads the stored `HeadingContext`). Switching them
  to `Reconstruct` (and dropping the on-disk `HeadingContext` field) is deferred
  to the "sync wiring" stage, since rewiring reuse keying is intertwined with the
  later per-chunk-reuse simplification. This keeps stage 3 a pure, tested helper
  with no behavior change to the live path.
- **Breadcrumb containment uses raw span bounds.** `Breadcrumb` treats a span as
  enclosing when `sp.start <= start && end <= sp.end`. This exactly reproduces the
  sweeper breadcrumb for chunks *inside* a definition body (the reconstruction
  case), and for text it reproduces `ChunkMarkdown` verbatim (tested). A chunk
  whose start is the extended def start (leading doc-comment/keywords, i.e. a
  definition-header chunk) is a boundary case that the unified-auto-chunk stage
  handles when boundaries become Voyage's rather than AST-derived.
## sync wiring — DONE

- Goal: `syncCache` builds rows via `Reconstruct` from git blobs; `store.PutFile`
  receives identical fields as before. The `pkb chunk` command is removed.
- Tests:
  - [x] Full reindex then fresh `SyncCache` from a wiped `cache.db` produces the same
    `chunks` rows (`TestSearchColdCacheMatchesWarm`).
  - [x] `pkb chunk` and its wiring are gone; `main` and tests build without it.
  - [x] Sync reads the indexed blob, not the working tree: modify the working-tree
    file after indexing, sync, and assert reconstructed text matches the indexed
    blob (`TestSyncReadsIndexedBlobNotWorkingTree`).

**Result (2026-07-18): DONE.** Wired the sync path through the stage-3
`Reconstruct` helper and dropped the on-disk `HeadingContext` field (the last
derivable value still duplicated on disk), so `.meta` now stores only
`{start,end}` byte offsets per chunk:

- `mirror.Chunk` / `metaChunk` (`internal/mirror/mirror.go`) reduced to
  `{Start, End}` + the `.vec` embedding; `EncodeMeta`/`DecodeMeta` updated. The
  `headingContext` JSON key is gone; `TestMetaHasNoChunkText` no longer asserts
  it.
- `index.reconstructArtifact` (`internal/index/manager.go`) is now a thin adapter
  over `Reconstruct`: it `CatBlob`s the artifact's exact blob, builds `[]byteSpan`
  from the stored offsets, and maps the returned `reconstructedChunk`s into the
  `([]chunk.ChunkInfo, []string)` shape `PutFile` expects. `syncCache` and
  `reuseMap` are unchanged callers, so both now derive heading breadcrumbs from
  file structure (AST symbol path / markdown headers) at sync time rather than
  reading a stored field. Per-chunk reuse still hits because the reconstructed
  breadcrumb equals the chunker's `HeadingContext` (verified in stage 3).
- `writeFile` no longer persists `HeadingContext`.
- `store.MajorVersion` bumped 6 -> 7 (the `.meta` shape changed; forces a clean
  re-embed). Store migration test seed and legacy-file test updated to v7.
- `pkb chunk` removed: `runChunk`/its dispatch/usage line are deleted from
  `main.go`, along with the now-unused `chunk`/`filetype` imports. `chunkHeading`
  stays (still used by `formatResults`); its doc comment dropped the chunk-preview
  reference. `pkb chunk` can no longer reproduce chunks offline since boundaries
  are the API's, so it is dropped per the plan rather than reworked.

Decisions / deviations:

- **Dropped the on-disk `HeadingContext` now** (stage 2 had kept it as an
  interim simplification). Stage 3 deferred this to "sync wiring"; done here since
  `Reconstruct` fully recomputes the breadcrumb, making the stored field dead
  weight. This is the final removal of derivable data from `.meta`.
- **Per-chunk reuse kept intact** (not yet simplified to file-level skip — that is
  the later unified-auto-chunk stage). `reuseMap` keys on the reconstructed
  breadcrumb+text, so it keeps working across edits without the stored field.
- Pre-existing repo-wide `golangci-lint` errcheck findings (21, all on untouched
  files / established `defer x.Close()` patterns) are unchanged; `go build`,
  `go vet`, and `go test ./...` are all green.

## unified auto-chunk positioning (code + text) — DONE

- Goal: every file goes through `EmbedDocument`; each returned chunk is resolved
  to an exact byte span via forward substring scan and stored as offsets only; an
  unresolved chunk fails the reindex hard and loud. Preceded by the code
  verbatim-substring validation spike.
- Tests:
  - [x] A multi-heading markdown file and a `.go` file: each stored chunk's span
    slices back to its exact returned text; markdown breadcrumbs are enclosing
    headers, code breadcrumbs are the AST symbol path
    (`TestUnifiedAutoChunkBreadcrumbsAndSpans`).
  - [x] Hard-fail: a mocked `EmbedDocument` that returns a chunk text not present as
    a substring causes `Reindex` to error, naming the file + chunk index, and
    writes no artifact for that file (`TestReindexHardFailsOnUnlocatableChunk`).
  - [x] Overlapping-window (>120K-token) file: spans are still exact and dedup keeps
    each span once (`TestContextualizeTextWindowsLargeFileAndDedups`).
  - [x] Reconstructed `contextualized_text` is the breadcrumb-decorated chunk (raw
    text + enclosing headers), i.e. differs from the raw embedded input by
    exactly the decoration — a display aid, not a round-trip equality check
    (`TestUnifiedAutoChunkBreadcrumbsAndSpans`).

**Result (2026-07-18): DONE.** Collapsed the two prepare paths into one
auto-chunk pipeline: every candidate file — code and text — is sent whole to
`EmbedDocument` (`prepareFile`, formerly `prepareContextualFile`), each returned
chunk is resolved to a verbatim byte span via `resolveSpans` (forward
`bytes.Index` scan with a whole-file fallback for out-of-order window chunks),
and only offsets are stored. An unresolvable chunk hard-fails the reindex naming
the file + chunk index (`resolveSpans` error, exercised by
`TestReindexHardFailsOnUnlocatableChunk`).

Decisions / deviations:

- **Removed the entire code/EmbedChunks path from indexing.** The old code
  `prepareFile` (tree-sitter `chunkFile` + per-chunk `reuseMap`/`ChunkKey` reuse
  + `compactPrepared` + cross-file `EmbedChunks` batching with `flush`/`embedRef`
  machinery) is deleted. `Reindex` now calls the single `prepareFile` for all
  candidates and writes each synchronously; the cross-file embedding batch,
  `maxBatchChars`, `pending`, and `flush` are gone. `contextualModel` is now
  required for indexing (hard error if the model lacks `EmbedDocument`).
- **Per-chunk reuse dropped; file-level blob-sha skip only.** An unchanged file
  is skipped wholesale; any changed file re-embeds whole (code boundaries are the
  API's now, so per-chunk reuse keying no longer applies). `estimate` mirrors
  this: every touched file is projected at whole-file token count (the old
  code-chunking + reuse branch is gone), keeping the file-level skip so unchanged
  blobs still cost $0 (`TestBudgetGateDoesNotChargeReuse` still green).
- **`MajorVersion` bumped 7 -> 8.** The code chunking algorithm changed
  (tree-sitter boundaries -> Voyage auto-chunk), which per the versioning policy
  forces a clean re-embed. The `.meta` shape itself is unchanged ({start,end}).
- **Mock `EmbedDocument` now counts embedded chunks** (`chunkCount += len(out)`)
  so `ChunkCount()` reflects auto-chunk work; `FailingModel` and the test
  `recordingModel` gained `EmbedDocument` overrides so crash-safety/hard-fail
  tests drive the auto-chunk path.
- **Obsolete code-chunking tests replaced.** `TestReusesUnchangedChunksCode`,
  `TestReindexOnParentClassRenameCode`, `TestCrashMidFileReusesCommittedChunks`,
  `TestReindexReusesChunkKeepsArtifactBytes`,
  `TestCompactPreparedDropsZeroSignalChunks`, and
  `TestContextualizeTextLeavesCodeOnIsolatedPath` (which asserted the old
  per-chunk/isolated behavior) were removed or rewritten into
  `TestEditedCodeReembedsWholeFile`, `TestCrashMidRunMarkerSafety`,
  `TestCodeRoutesThroughEmbedDocument`, and the two new stage-5 tests above.
- Pre-existing repo-wide `golangci-lint` errcheck findings (21, all on untouched
  `defer x.Close()` / mirror-temp patterns) are unchanged; `go build`, `go vet`,
  and `go test ./...` are all green.

## end-to-end + size check — DONE

- Goal: confirm the repo-size reduction and healthcheck consistency.
- Tests:
  - [x] `pkb healthcheck` clean after full reindex on this repo.
  - [x] Assert total `.pkb/index/**.meta` bytes drop substantially vs. before.
  - [x] `pkb search` returns coherent snippets (reconstructed text) end-to-end.

**Result (2026-07-18): DONE.** Ran a full from-scratch reindex on this repo and
verified all three checks:

- **Healthcheck clean.** `pkb healthcheck` after reindex: HEAD == state commit,
  expected files 63 == indexed files 63, 355 chunks, "healthy: index and state
  marker match the git tree".
- **`.meta` bytes dropped ~98%.** Total `.pkb/index/**.meta` bytes went from
  1,237,052 (pre-refactor commit `1234d41`) to **25,910** after the offset-only
  reindex — the `.meta` now holds only `{start,end}` per chunk (blobSha +
  modelName header aside). All 63 `.meta` are the new format (0 files retain a
  `text`/`contextualized`/`headingContext` key).
- **Search returns coherent reconstructed snippets.** `pkb search` returns
  correctly reconstructed chunk text with accurate `path:Lnn` line numbers,
  proving the sync-time reconstruction (blob slice + breadcrumb + line/col)
  round-trips end-to-end.

Decisions / deviations:

- **Forced a from-scratch reindex (`rm -rf .pkb/index .pkb/cache.db`) to
  regenerate the mirror.** The `MajorVersion` bump (now 8) re-keys the SQLite
  cache but the mirror-artifact skip in `Reindex` is keyed on `(model, blob_sha)`
  only — it does not carry the major version — so an ordinary `pkb reindex` left
  the 54 unchanged files in their old (v≤7) on-disk format. Wiping the mirror was
  the pragmatic one-time way to realize the format migration across every file.
  This is a known gap (major-version invalidation of the *mirror* is not
  automatic), not a regression introduced here.
- **Removed a dead helper.** Stage 5's collapse of the code-chunking path left
  `(*Options).grammarFor` unused (golangci `unused`); deleted it. The remaining
  21 golangci findings are the pre-existing repo-wide `errcheck` items on
  untouched files, unchanged from prior stages. `go build`, `go vet`, and
  `go test ./...` are all green.
- Committed the regenerated `.pkb/index/**` offset-only artifacts + `pkb-state.toml`.

package chunk

import (
	"strings"
	"unicode/utf8"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// ChunkCode splits source code into chunks along syntactic boundaries using
// tree-sitter, carrying a structural breadcrumb (file path + enclosing symbol
// path) as HeadingContext. pathContext seeds the breadcrumb (typically the
// file's relative path). It falls back to line-based chunking when the grammar
// is unknown or the source fails to parse cleanly.
func ChunkCode(content []byte, grammar, pathContext string, maxChunkSize int) ([]ChunkInfo, error) {
	if maxChunkSize <= 0 {
		maxChunkSize = TargetChunkSize
	}
	if len(content) == 0 {
		return nil, nil
	}

	lang := languageFor(grammar)
	if lang == nil {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}

	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}
	tree := parser.Parse(content, nil)
	if tree == nil {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}
	defer tree.Close()

	root := tree.RootNode()
	if root == nil || root.HasError() {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}

	idx, idxErr := buildDefIndex(root, content, grammar)
	if idxErr != nil {
		// Query failed to compile: chunk with no definition spans (whole-file
		// cAST packing).
		idx = nil
	}

	sw := &sweeper{
		source:      content,
		budget:      maxChunkSize,
		idx:         idx,
		pathContext: pathContext,
		winStart:    -1,
		longLines:   computeLongLines(content, maxChunkSize),
		emittedLong: map[int]bool{},
	}
	sw.sweepChildren(root, int(root.StartByte()))
	sw.flush()
	if len(sw.out) == 0 {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}
	return sw.out, nil
}

// sweeper performs the single recursive cAST pass over the parse tree. A
// definition span (a @definition.* capture) is a hard break: entering or exiting
// one force-flushes the current window, and the stack of active spans supplies
// the breadcrumb. Plain nodes are packed into windows up to the byte budget.
type sweeper struct {
	source      []byte
	budget      int
	idx         *defIndex
	pathContext string

	// breadcrumbFn, when non-nil, overrides the def-span breadcrumb: it is
	// called at flush time with the window's byte range and returns the full
	// HeadingContext for the chunk. Used by the config chunker, which derives
	// breadcrumbs from a node's structural path rather than tags.scm spans.
	breadcrumbFn func(lo, hi int) string

	stack []defSpan

	// current window, as a byte range [winStart, winEnd); winStart < 0 means
	// the window is empty.
	winStart int
	winEnd   int

	// longLines are lines whose byte length exceeds the budget. They are never
	// digested: the first node that falls on one aborts and emits the line
	// trimmed to budget as a single chunk (tracked in emittedLong so a line
	// holding several definition spans is emitted exactly once).
	longLines   []lineSpan
	emittedLong map[int]bool

	out []ChunkInfo
}

// lineSpan is a half-open byte range [start, end) covering a single line,
// excluding its trailing newline.
type lineSpan struct {
	start int
	end   int
}

// sweepChildren visits the named children of node in source order, attaching a
// contiguous run of leading comment ("extra") nodes to the node that
// immediately follows it. The run's start byte is threaded into visitNode as
// attachStart so the comment rides into the following node's chunk (a
// definition span is extended; a plain node is packed as an indivisible unit).
// A blank line between the run and the following node breaks the attachment:
// the run is emitted as ordinary filler instead.
func (s *sweeper) sweepChildren(node *tree_sitter.Node, leadStart int) {
	count := node.NamedChildCount()
	// commentStart/commentEnd track a pending contiguous run of comment nodes;
	// commentStart < 0 means no run is pending. leadStart < node.StartByte()
	// means a comment run attached to a parent that recursed into this node;
	// seed the pending run so it can re-attach to the first inner node (e.g. a
	// Go doc comment whose sibling is `type_declaration` but whose definition
	// span is the nested `type_spec`).
	commentStart, commentEnd := -1, -1
	if leadStart < int(node.StartByte()) {
		commentStart, commentEnd = leadStart, int(node.StartByte())
	}
	for i := uint(0); i < count; i++ {
		child := node.NamedChild(i)
		lo, hi := int(child.StartByte()), int(child.EndByte())
		if child.IsExtra() {
			switch {
			case commentStart < 0:
				commentStart, commentEnd = lo, hi
			case adjacent(s.source, commentEnd, lo):
				commentEnd = hi
			default:
				// A blank line broke the run: emit the prior run as filler and
				// start a fresh run at this comment.
				s.addRange(commentStart, commentEnd)
				commentStart, commentEnd = lo, hi
			}
			continue
		}

		attachStart := lo
		if commentStart >= 0 {
			if adjacent(s.source, commentEnd, lo) {
				attachStart = commentStart
			} else {
				s.addRange(commentStart, commentEnd)
			}
			commentStart, commentEnd = -1, -1
		}
		s.visitNode(child, attachStart)
	}
	// A trailing comment run with no following sibling is emitted as filler so
	// it is never dropped.
	if commentStart >= 0 {
		s.addRange(commentStart, commentEnd)
	}
}

// visitNode applies the sweep rules to a single named node. attachStart is the
// node's effective left edge: it equals the node's own start byte unless a
// leading comment run attaches to it, in which case it points at the run start.
func (s *sweeper) visitNode(node *tree_sitter.Node, attachStart int) {
	if span, ok := s.longLineAt(int(node.StartByte())); ok {
		s.emitLongLine(span)
		return
	}

	if span, ok := s.spanFor(node); ok {
		s.enterSpan(span, node, attachStart)
		return
	}

	lo, hi := int(node.StartByte()), int(node.EndByte())
	if hi-lo <= s.budget && !s.containsSpan(node) {
		s.addRange(attachStart, hi)
		return
	}

	if node.NamedChildCount() == 0 {
		// Childless leaf over budget: line-split as the final backstop, leading
		// with any attached comment so it is not lost.
		s.flush()
		s.lineSplitRange(attachStart, hi)
		return
	}
	// Node that recurses (over budget, or containing a nested definition span):
	// thread attachStart in as the lead start so an attached comment re-attaches
	// to the first inner node instead of being dropped.
	s.sweepChildren(node, attachStart)
}

// enterSpan flushes the current window, pushes the span (extending its start
// back over leading keywords/doc comments), packs the span's body, then flushes
// and pops on exit. A span that fits entirely in one window is emitted whole.
func (s *sweeper) enterSpan(span defSpan, node *tree_sitter.Node, attachStart int) {
	if attachStart < span.extStart {
		span.extStart = attachStart
	}
	s.trimWindowTo(span.extStart)
	s.flush()
	s.stack = append(s.stack, span)

	// Seed the window at the extended start so leading keywords (Go's `type `,
	// TS `export `) and the doc-comment run ride with the definition.
	s.winStart = span.extStart
	s.winEnd = int(node.StartByte())

	s.sweepChildren(node, int(node.StartByte()))

	if s.winStart >= 0 && s.winEnd < span.end {
		s.winEnd = span.end
	}
	s.flush()
	s.stack = s.stack[:len(s.stack)-1]
}

// addRange adds a plain node's byte range to the current window, flushing first
// if appending it would overflow the budget.
func (s *sweeper) addRange(lo, hi int) {
	if s.winStart < 0 {
		s.winStart, s.winEnd = lo, hi
		return
	}
	if hi-s.winStart > s.budget && s.winEnd > s.winStart {
		s.flush()
		s.winStart, s.winEnd = lo, hi
		return
	}
	s.winEnd = hi
}

// trimWindowTo clamps the current window so it ends no later than byte b,
// dropping it entirely if it would become empty. Used so a definition's doc run
// (already inside the window as filler) is not emitted twice.
func (s *sweeper) trimWindowTo(b int) {
	if s.winStart < 0 {
		return
	}
	if s.winStart >= b {
		s.winStart = -1
		return
	}
	if s.winEnd > b {
		s.winEnd = b
	}
}

// flush emits the current window as a chunk (dropping whitespace-only windows)
// and resets it.
func (s *sweeper) flush() {
	if s.winStart < 0 {
		return
	}
	text := string(s.source[s.winStart:s.winEnd])
	if strings.TrimSpace(text) != "" {
		bc := s.breadcrumb()
		if s.breadcrumbFn != nil {
			bc = s.breadcrumbFn(s.winStart, s.winEnd)
		}
		s.out = append(s.out, ChunkInfo{
			Text:           text,
			HeadingContext: bc,
			Start:          posFromByte(s.source, s.winStart),
			End:            posFromByte(s.source, s.winEnd),
		})
	}
	s.winStart = -1
}

// lineSplitRange emits a byte range as one or more line-split chunks under the
// current breadcrumb.
func (s *sweeper) lineSplitRange(lo, hi int) {
	text := string(s.source[lo:hi])
	if strings.TrimSpace(text) == "" {
		return
	}
	bc := s.breadcrumb()
	if s.breadcrumbFn != nil {
		bc = s.breadcrumbFn(lo, hi)
	}
	for _, sc := range splitCodeByLines(text, posFromByte(s.source, lo), s.budget) {
		s.out = append(s.out, ChunkInfo{
			Text:           sc.text,
			HeadingContext: bc,
			Start:          sc.start,
			End:            sc.end,
		})
	}
}

// breadcrumb joins the file path with each active span's "label name".
func (s *sweeper) breadcrumb() string {
	bc := s.pathContext
	for _, sp := range s.stack {
		bc = joinBreadcrumb(bc, strings.TrimSpace(sp.label+" "+sp.name))
	}
	return bc
}

// spanFor returns the definition span captured at node, with its start extended
// back to the beginning of its line.
func (s *sweeper) spanFor(node *tree_sitter.Node) (defSpan, bool) {
	if s.idx == nil {
		return defSpan{}, false
	}
	entry, ok := s.idx.entry(node)
	if !ok {
		return defSpan{}, false
	}
	start := int(node.StartByte())
	extStart := lineStartByte(s.source, start)
	return defSpan{defEntry: entry, start: start, end: int(node.EndByte()), extStart: extStart}, true
}

// containsSpan reports whether any definition span begins strictly inside node.
func (s *sweeper) containsSpan(node *tree_sitter.Node) bool {
	if s.idx == nil {
		return false
	}
	lo, hi := int(node.StartByte()), int(node.EndByte())
	for _, sp := range s.idx.spans {
		// Strictly inside: a span sharing node's start byte is either node
		// itself (handled by spanFor) or a child that coincides with the
		// already-active span boundary, neither of which is a nested span.
		if sp.start > lo && sp.start < hi {
			return true
		}
	}
	return false
}

// adjacent reports whether the bytes in source[from:to] contain no blank line,
// i.e. at most one newline. A blank-line gap means a leading comment run is a
// standalone unit rather than documentation for the following node.
func adjacent(source []byte, from, to int) bool {
	newlines := 0
	for i := from; i < to && i < len(source); i++ {
		if source[i] == '\n' {
			newlines++
			if newlines > 1 {
				return false
			}
		}
	}
	return true
}

// computeLongLines returns the byte ranges of lines whose length exceeds budget.
func computeLongLines(source []byte, budget int) []lineSpan {
	var out []lineSpan
	lineStart := 0
	for i := 0; i <= len(source); i++ {
		if i == len(source) || source[i] == '\n' {
			if i-lineStart > budget {
				out = append(out, lineSpan{start: lineStart, end: i})
			}
			lineStart = i + 1
		}
	}
	return out
}

// longLineAt returns the over-budget line containing off, if any.
func (s *sweeper) longLineAt(off int) (lineSpan, bool) {
	for _, ls := range s.longLines {
		if ls.start > off {
			break
		}
		if off >= ls.start && off < ls.end {
			return ls, true
		}
	}
	return lineSpan{}, false
}

// emitLongLine flushes the pending window and emits the line trimmed to budget
// as a single chunk, at most once per line. The breadcrumb is the bare path
// context: a long line is aborted before any definition span is entered, so it
// never picks up a span breadcrumb.
func (s *sweeper) emitLongLine(ls lineSpan) {
	if s.emittedLong[ls.start] {
		return
	}
	s.emittedLong[ls.start] = true
	s.flush()
	end := ls.end
	if end-ls.start > s.budget {
		end = ls.start + s.budget
		for end > ls.start && !utf8.RuneStart(s.source[end]) {
			end--
		}
	}
	text := string(s.source[ls.start:end])
	if strings.TrimSpace(text) == "" {
		return
	}
	bc := s.breadcrumb()
	if s.breadcrumbFn != nil {
		bc = s.breadcrumbFn(ls.start, end)
	}
	s.out = append(s.out, ChunkInfo{
		Text:           text,
		HeadingContext: bc,
		Start:          posFromByte(s.source, ls.start),
		End:            posFromByte(s.source, end),
	})
}

// lineStartByte returns the byte offset of the beginning of the line containing
// off (the byte just after the preceding newline, or 0).
func lineStartByte(source []byte, off int) int {
	if off > len(source) {
		off = len(source)
	}
	for i := off - 1; i >= 0; i-- {
		if source[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// PosFromByte computes a 1-based line/col Position for a byte offset in source.
// It is the exported entry point used by the cache-sync reconstruction path to
// derive chunk positions from stored byte offsets.
func PosFromByte(source []byte, off int) Position {
	return posFromByte(source, off)
}

// posFromByte computes a 1-based line/col Position for a byte offset in source.
func posFromByte(source []byte, off int) Position {
	line, col := 1, 1
	for i := 0; i < off && i < len(source); i++ {
		if source[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return Position{Line: line, Col: col}
}

func joinBreadcrumb(prefix, desc string) string {
	if prefix == "" {
		return desc
	}
	if desc == "" {
		return prefix
	}
	return prefix + " > " + desc
}

// lineChunks splits raw content by lines into budget-sized chunks, used as the
// fallback when no grammar applies or parsing fails.
func lineChunks(content []byte, prefix string, maxChunkSize int) []ChunkInfo {
	text := string(content)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []ChunkInfo
	for _, sc := range splitCodeByLines(text, Position{Line: 1, Col: 1}, maxChunkSize) {
		out = append(out, ChunkInfo{
			Text:           sc.text,
			HeadingContext: prefix,
			Start:          sc.start,
			End:            sc.end,
		})
	}
	return out
}

// splitCodeByLines splits text into chunks at line boundaries without exceeding
// maxChunkSize, falling back to character splitting for individual oversized
// lines. Ported from splitCodeBlockByLines in chunker.ts.
func splitCodeByLines(text string, blockStart Position, maxChunkSize int) []softChunk {
	if len(text) <= maxChunkSize {
		lines := strings.Split(text, "\n")
		lastLine := lines[len(lines)-1]
		return []softChunk{{
			text:  text,
			start: blockStart,
			end:   endPosOfLine(blockStart.Line+len(lines)-1, lastLine),
		}}
	}

	lines := strings.Split(text, "\n")
	var chunks []softChunk
	var currentLines []string
	currentSize := 0
	chunkStartLineIdx := 0

	flushChunk := func() {
		if len(currentLines) == 0 {
			return
		}
		chunkText := strings.Join(currentLines, "\n")
		lastLine := currentLines[len(currentLines)-1]
		chunks = append(chunks, softChunk{
			text:  chunkText,
			start: Position{Line: blockStart.Line + chunkStartLineIdx, Col: 1},
			end:   endPosOfLine(blockStart.Line+chunkStartLineIdx+len(currentLines)-1, lastLine),
		})
		currentLines = nil
		currentSize = 0
	}

	for i, line := range lines {
		addedSize := len(line)
		if len(currentLines) > 0 {
			addedSize++
		}
		if currentSize+addedSize > maxChunkSize && len(currentLines) > 0 {
			flushChunk()
			chunkStartLineIdx = i
		}
		if len(line) <= maxChunkSize {
			currentLines = append(currentLines, line)
			currentSize += addedSize
		} else {
			flushChunk()
			lineStart := Position{Line: blockStart.Line + i, Col: 1}
			chunks = append(chunks, splitByCharacters(line, lineStart, maxChunkSize, characterSplitOverlap)...)
			chunkStartLineIdx = i + 1
		}
	}

	flushChunk()
	return chunks
}

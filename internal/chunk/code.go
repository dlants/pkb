package chunk

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// declKinds is the set of tree-sitter node kinds treated as chunkable
// declarations (functions, classes, methods, etc.) across the supported
// grammars. Everything else (imports, top-level constants, statements) is
// grouped into "filler" chunks.
var declKinds = map[string]struct{}{
	"function_declaration":           {},
	"function_definition":            {},
	"function_item":                  {},
	"generator_function_declaration": {},
	"method_definition":              {},
	"method_declaration":             {},
	"class_declaration":              {},
	"abstract_class_declaration":     {},
	"class_definition":               {},
	"class_specifier":                {},
	"interface_declaration":          {},
	"struct_item":                    {},
	"enum_item":                      {},
	"impl_item":                      {},
	"trait_item":                     {},
	"mod_item":                       {},
	"module":                         {},
	"namespace_declaration":          {},
	"type_alias_declaration":         {},
	"type_declaration":               {},
}

func isDeclKind(kind string) bool {
	_, ok := declKinds[kind]
	return ok
}

// labelFromKind turns a tree-sitter node kind into a human breadcrumb label,
// e.g. "function_declaration" -> "function", "method_definition" -> "method".
func labelFromKind(kind string) string {
	for _, suffix := range []string{"_declaration", "_definition", "_specifier", "_item"} {
		if strings.HasSuffix(kind, suffix) {
			kind = strings.TrimSuffix(kind, suffix)
			break
		}
	}
	return strings.ReplaceAll(kind, "_", " ")
}

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
		// Query failed to compile: fall back to the heuristic path.
		idx = nil
	}

	var out []ChunkInfo
	chunkContainer(root, content, pathContext, maxChunkSize, idx, &out)
	if len(out) == 0 {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}
	return out, nil
}

// unwrap returns the inner declaration of a wrapper node (e.g. an
// export_statement / decorated_definition), or the node itself.
func unwrap(node *tree_sitter.Node) *tree_sitter.Node {
	switch node.Kind() {
	case "export_statement", "decorated_definition":
		count := node.NamedChildCount()
		for i := uint(0); i < count; i++ {
			child := node.NamedChild(i)
			if isDeclKind(child.Kind()) {
				return child
			}
		}
	}
	return node
}

// chunkContainer walks the named children of container, grouping non-declaration
// nodes into filler chunks and emitting each declaration via emitDecl.
func chunkContainer(container *tree_sitter.Node, source []byte, prefix string, maxChunkSize int, idx *defIndex, out *[]ChunkInfo) {
	count := container.NamedChildCount()
	var filler []*tree_sitter.Node

	flushFiller := func() {
		if len(filler) == 0 {
			return
		}
		emitRange(filler[0], filler[len(filler)-1], source, prefix, maxChunkSize, out)
		filler = nil
	}

	for i := uint(0); i < count; i++ {
		child := container.NamedChild(i)
		decl := unwrap(child)
		if isDecl(decl, idx) {
			// A doc comment claimed by this declaration may have been
			// accumulated as trailing filler; drop it so it travels with the
			// declaration instead of being emitted twice.
			if e, ok := idx.entry(decl); ok && e.docStartByte >= 0 {
				for len(filler) > 0 && int(filler[len(filler)-1].StartByte()) >= e.docStartByte {
					filler = filler[:len(filler)-1]
				}
			}
			flushFiller()
			emitDecl(child, decl, source, prefix, maxChunkSize, idx, out)
			continue
		}
		filler = append(filler, child)
	}
	flushFiller()
}

// isDecl reports whether node is a chunkable declaration: via the definition
// index when one is available, otherwise via the kind heuristic.
func isDecl(node *tree_sitter.Node, idx *defIndex) bool {
	if idx != nil {
		return idx.has(node)
	}
	return isDeclKind(node.Kind())
}

// emitDecl emits a declaration: as a single chunk if it fits, otherwise by
// recursing into nested declarations, or by line-splitting if it is a leaf.
func emitDecl(outer, decl *tree_sitter.Node, source []byte, prefix string, maxChunkSize int, idx *defIndex, out *[]ChunkInfo) {
	start := int(outer.StartByte())
	end := int(outer.EndByte())
	startPos := posOf(outer.StartPosition())

	entry, hasEntry := idx.entry(decl)
	if hasEntry && entry.docStartByte >= 0 && entry.docStartByte < start {
		start = entry.docStartByte
		startPos = posFromByte(source, start)
	}
	text := string(source[start:end])

	var label, name string
	if hasEntry {
		label, name = entry.label, entry.name
	} else {
		label = labelFromKind(decl.Kind())
		name = declName(decl, source)
	}
	desc := strings.TrimSpace(label + " " + name)
	childPrefix := joinBreadcrumb(prefix, desc)

	if len(text) <= maxChunkSize {
		*out = append(*out, ChunkInfo{
			Text:           text,
			HeadingContext: childPrefix,
			Start:          startPos,
			End:            posOf(outer.EndPosition()),
		})
		return
	}

	body := decl.ChildByFieldName("body")
	if body != nil && hasDeclChild(body, idx) {
		chunkContainer(body, source, childPrefix, maxChunkSize, idx, out)
		return
	}

	// Leaf declaration over budget: line-split the whole declaration.
	for _, sc := range splitCodeByLines(text, startPos, maxChunkSize) {
		*out = append(*out, ChunkInfo{
			Text:           sc.text,
			HeadingContext: childPrefix,
			Start:          sc.start,
			End:            sc.end,
		})
	}
}

// emitRange emits a filler chunk spanning the byte range of [first,last],
// line-splitting if it exceeds the budget.
func emitRange(first, last *tree_sitter.Node, source []byte, prefix string, maxChunkSize int, out *[]ChunkInfo) {
	start := int(first.StartByte())
	end := int(last.EndByte())
	text := string(source[start:end])
	if strings.TrimSpace(text) == "" {
		return
	}
	startPos := posOf(first.StartPosition())
	if len(text) <= maxChunkSize {
		*out = append(*out, ChunkInfo{
			Text:           text,
			HeadingContext: prefix,
			Start:          startPos,
			End:            posOf(last.EndPosition()),
		})
		return
	}
	for _, sc := range splitCodeByLines(text, startPos, maxChunkSize) {
		*out = append(*out, ChunkInfo{
			Text:           sc.text,
			HeadingContext: prefix,
			Start:          sc.start,
			End:            sc.end,
		})
	}
}

func hasDeclChild(container *tree_sitter.Node, idx *defIndex) bool {
	count := container.NamedChildCount()
	for i := uint(0); i < count; i++ {
		if isDecl(unwrap(container.NamedChild(i)), idx) {
			return true
		}
	}
	return false
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

func declName(node *tree_sitter.Node, source []byte) string {
	if n := node.ChildByFieldName("name"); n != nil {
		return n.Utf8Text(source)
	}
	count := node.NamedChildCount()
	for i := uint(0); i < count; i++ {
		child := node.NamedChild(i)
		if child.Kind() == "identifier" || child.Kind() == "type_identifier" {
			return child.Utf8Text(source)
		}
	}
	return ""
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

func posOf(p tree_sitter.Point) Position {
	return Position{Line: int(p.Row) + 1, Col: int(p.Column) + 1}
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

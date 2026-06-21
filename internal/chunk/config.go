package chunk

import (
	"strconv"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// configGrammars are the structured-config grammars whose chunks carry a
// structural key/index path as their breadcrumb (e.g. services.web.ports[1])
// rather than a tags.scm definition path. They intentionally have no vendored
// query; chunking reuses the cAST sweeper with a config breadcrumb hook.
var configGrammars = map[string]bool{
	"json": true,
	"yaml": true,
	"toml": true,
}

// IsConfigGrammar reports whether a grammar is chunked by ChunkConfig.
func IsConfigGrammar(grammar string) bool {
	return configGrammars[grammar]
}

// ChunkConfig chunks a structured-config file. It reuses the tree-sitter cAST
// sweeper (no definition index, so packing is pure budget windowing along node
// boundaries) but installs a breadcrumb hook that maps each chunk's byte window
// to its structural path from the document root. Falls back to line-based
// chunking when the grammar is unknown or the source fails to parse.
func ChunkConfig(content []byte, grammar, pathContext string, maxChunkSize int) ([]ChunkInfo, error) {
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

	sw := &sweeper{
		source:      content,
		budget:      maxChunkSize,
		pathContext: pathContext,
		winStart:    -1,
		breadcrumbFn: func(lo, hi int) string {
			return joinBreadcrumb(pathContext, configPath(root, content, lo, hi))
		},
	}
	sw.sweepChildren(root, int(root.StartByte()))
	sw.flush()
	if len(sw.out) == 0 {
		return lineChunks(content, pathContext, maxChunkSize), nil
	}
	return sw.out, nil
}

// configPath returns the formatted structural path (e.g. "services.web.ports[1]")
// of the lowest container node fully enclosing the byte window [lo, hi). It
// descends from root through the unique named child that still contains the
// window, accumulating a path segment at each step, and stops at the lowest
// common ancestor (the deepest node no single child fully contains).
func configPath(root *tree_sitter.Node, src []byte, lo, hi int) string {
	node := root
	var parts []segPart
	for {
		child := childContaining(node, lo, hi)
		if child == nil {
			break
		}
		parts = append(parts, configSegment(node, child, src)...)
		node = child
	}
	return formatConfigPath(parts)
}

// segPart is one element of a config path: a key (joined with ".") or an index
// (e.g. "[1]", appended with no separator).
type segPart struct {
	text    string
	isIndex bool
}

// childContaining returns the unique named child of node whose byte range fully
// contains [lo, hi), or nil if none does (siblings are disjoint, so at most one
// can match). A nil result means node itself is the lowest enclosing container.
func childContaining(node *tree_sitter.Node, lo, hi int) *tree_sitter.Node {
	count := node.NamedChildCount()
	for i := uint(0); i < count; i++ {
		c := node.NamedChild(i)
		if int(c.StartByte()) <= lo && hi <= int(c.EndByte()) {
			return c
		}
	}
	return nil
}

// configSegment returns the path segment(s) contributed by descending from
// parent into child, across JSON / YAML / TOML. Transparent wrappers (documents,
// nodes, scalars, the value side of a pair) contribute nothing. Mapping members
// contribute their key; sequence elements contribute their index; TOML tables
// contribute their (possibly dotted) header, and array-of-tables also an index.
func configSegment(parent, child *tree_sitter.Node, src []byte) []segPart {
	switch parent.Kind() {
	case "object", "block_mapping", "flow_mapping":
		return []segPart{{text: keyText(child, src)}}
	case "array", "block_sequence", "flow_sequence":
		return []segPart{{text: indexBracket(positionalIndex(parent, child)), isIndex: true}}
	case "table", "table_array_element":
		// child is either a pair (-> its key) or the table's header key node,
		// which was already emitted at the document->table step.
		if child.Kind() == "pair" {
			return []segPart{{text: keyText(child, src)}}
		}
		return nil
	case "document":
		switch child.Kind() {
		case "table":
			return []segPart{{text: tableHeader(child, src)}}
		case "table_array_element":
			return []segPart{
				{text: tableHeader(child, src)},
				{text: indexBracket(tableArrayIndex(parent, child, src)), isIndex: true},
			}
		}
	}
	return nil
}

// keyText extracts the key of a mapping member (JSON/YAML pair) or TOML pair.
// JSON/YAML expose a "key" field; TOML pairs are positional (first named child).
func keyText(pair *tree_sitter.Node, src []byte) string {
	key := pair.ChildByFieldName("key")
	if key == nil {
		key = pair.NamedChild(0)
	}
	if key == nil {
		return ""
	}
	return unquote(strings.TrimSpace(key.Utf8Text(src)))
}

// tableHeader returns the dotted header of a TOML table / table-array element
// (its leading bare_key or dotted_key), e.g. "servers.alpha".
func tableHeader(table *tree_sitter.Node, src []byte) string {
	count := table.NamedChildCount()
	for i := uint(0); i < count; i++ {
		c := table.NamedChild(i)
		if k := c.Kind(); k == "bare_key" || k == "dotted_key" || k == "quoted_key" {
			return unquote(strings.TrimSpace(c.Utf8Text(src)))
		}
	}
	return ""
}

// positionalIndex returns child's 0-based position among parent's named
// children (used for sequence/array elements, whose kinds may be mixed).
func positionalIndex(parent, child *tree_sitter.Node) int {
	count := parent.NamedChildCount()
	idx := 0
	for i := uint(0); i < count; i++ {
		c := parent.NamedChild(i)
		if c.Id() == child.Id() {
			return idx
		}
		idx++
	}
	return idx
}

// tableArrayIndex returns the 0-based occurrence index of a [[header]] element
// among preceding document children sharing the same header text.
func tableArrayIndex(parent, child *tree_sitter.Node, src []byte) int {
	want := tableHeader(child, src)
	count := parent.NamedChildCount()
	idx := 0
	for i := uint(0); i < count; i++ {
		c := parent.NamedChild(i)
		if c.Id() == child.Id() {
			return idx
		}
		if c.Kind() == "table_array_element" && tableHeader(c, src) == want {
			idx++
		}
	}
	return idx
}

func indexBracket(i int) string {
	return "[" + strconv.Itoa(i) + "]"
}

// formatConfigPath joins path parts: keys with ".", indices appended directly.
func formatConfigPath(parts []segPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.text == "" {
			continue
		}
		if p.isIndex {
			b.WriteString(p.text)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(p.text)
	}
	return b.String()
}

// unquote strips a single layer of matching surrounding quotes from a key.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

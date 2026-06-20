package chunk

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// defEntry holds the per-declaration metadata derived from a tags.scm match:
// the symbol name (@name), the human label (the @definition.<label> suffix), and
// the start byte of an adjacent doc-comment run (@doc), or -1 when absent.
type defEntry struct {
	name         string
	label        string
	docStartByte int
}

// defIndex is the result of running a grammar's tags.scm over a parsed tree:
// nodes is the set of tree-sitter node IDs captured as @definition.*, and info
// maps each such node ID to its defEntry. Node identity uses Node.Id(), which is
// stable within a parsed tree, so the manual traversal can consult the index
// when it re-encounters the same nodes.
type defIndex struct {
	nodes map[uintptr]struct{}
	info  map[uintptr]defEntry
}

// buildDefIndex runs the grammar's vendored tags.scm query over the tree rooted
// at root, returning a defIndex of @definition.* captures. It returns (nil, nil)
// when no query is vendored for the grammar (callers should fall back to the
// heuristic path), or a non-nil error when the query fails to compile.
func buildDefIndex(root *tree_sitter.Node, source []byte, grammar string) (*defIndex, error) {
	src := queryFor(grammar)
	if src == "" {
		return nil, nil
	}
	lang := languageFor(grammar)
	if lang == nil {
		return nil, nil
	}

	q, qErr := tree_sitter.NewQuery(lang, src)
	if qErr != nil {
		return nil, qErr
	}
	defer q.Close()

	cursor := tree_sitter.NewQueryCursor()
	defer cursor.Close()

	names := q.CaptureNames()
	idx := &defIndex{
		nodes: map[uintptr]struct{}{},
		info:  map[uintptr]defEntry{},
	}

	matches := cursor.Matches(q, root, source)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var defNode *tree_sitter.Node
		var label, name string
		docStart := -1

		for _, capture := range m.Captures {
			cname := names[capture.Index]
			node := capture.Node
			switch {
			case strings.HasPrefix(cname, "definition."):
				n := node
				defNode = &n
				label = strings.TrimPrefix(cname, "definition.")
			case cname == "name":
				name = node.Utf8Text(source)
			case cname == "doc":
				if s := int(node.StartByte()); docStart == -1 || s < docStart {
					docStart = s
				}
			}
		}

		if defNode == nil {
			continue
		}
		id := defNode.Id()
		idx.nodes[id] = struct{}{}
		idx.info[id] = defEntry{name: name, label: label, docStartByte: docStart}
	}

	return idx, nil
}

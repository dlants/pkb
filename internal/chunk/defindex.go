package chunk

import (
	"sort"
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

// defSpan is a @definition.* capture as a byte interval plus its metadata. It is
// the bottom-up unit that drives chunk boundaries and breadcrumbs: start/end are
// the captured node's byte range, and the embedded defEntry carries name, label,
// and the doc-comment start byte (-1 when absent).
type defSpan struct {
	defEntry
	start int
	end   int
}

// defIndex is the result of running a grammar's tags.scm over a parsed tree:
// nodes is the set of tree-sitter node IDs captured as @definition.*, and info
// maps each such node ID to its defEntry. Node identity uses Node.Id(), which is
// stable within a parsed tree, so the manual traversal can consult the index
// when it re-encounters the same nodes.
type defIndex struct {
	nodes map[uintptr]struct{}
	info  map[uintptr]defEntry
	spans []defSpan
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
		entry := defEntry{name: name, label: label, docStartByte: docStart}
		idx.nodes[id] = struct{}{}
		idx.info[id] = entry
		idx.spans = append(idx.spans, defSpan{
			defEntry: entry,
			start:    int(defNode.StartByte()),
			end:      int(defNode.EndByte()),
		})
	}

	sort.SliceStable(idx.spans, func(i, j int) bool {
		if idx.spans[i].start != idx.spans[j].start {
			return idx.spans[i].start < idx.spans[j].start
		}
		return idx.spans[i].end < idx.spans[j].end
	})

	return idx, nil
}

// has reports whether node was captured as a @definition.* by the query.
func (d *defIndex) has(node *tree_sitter.Node) bool {
	if d == nil {
		return false
	}
	_, ok := d.nodes[node.Id()]
	return ok
}

// entry returns the defEntry for node, if it was captured as a @definition.*.
func (d *defIndex) entry(node *tree_sitter.Node) (defEntry, bool) {
	if d == nil {
		return defEntry{}, false
	}
	e, ok := d.info[node.Id()]
	return e, ok
}

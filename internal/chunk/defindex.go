package chunk

import (
	"sort"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// defEntry holds the per-declaration metadata derived from a tags.scm match:
// the symbol name (@name) and the human label (the @definition.<label> suffix).
type defEntry struct {
	name  string
	label string
}

// defSpan is a @definition.* capture as a byte interval plus its metadata. It is
// the bottom-up unit that drives chunk boundaries and breadcrumbs: start/end are
// the captured node's byte range, and the embedded defEntry carries name and
// label.
type defSpan struct {
	defEntry
	start int
	end   int
	// extStart is the span start extended back to the line start and over any
	// doc-comment run; it is the byte offset chunk text begins at. It is only
	// populated by the sweeper (zero in the raw buildDefIndex output).
	extStart int
}

// defIndex is the result of running a grammar's tags.scm over a parsed tree:
// info maps each @definition.* node ID to its defEntry (Node.Id() is stable
// within a parsed tree, so the sweep can consult it when it re-encounters a
// node), and spans is the same captures as ordered byte intervals.
type defIndex struct {
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
		info: map[uintptr]defEntry{},
	}

	matches := cursor.Matches(q, root, source)
	for m := matches.Next(); m != nil; m = matches.Next() {
		var defNode *tree_sitter.Node
		var suffixLabel, labelText string
		var nameParts []nameCapture

		for _, capture := range m.Captures {
			cname := names[capture.Index]
			node := capture.Node
			switch {
			case strings.HasPrefix(cname, "definition."):
				n := node
				defNode = &n
				suffixLabel = strings.TrimPrefix(cname, "definition.")
			case cname == "name":
				nameParts = append(nameParts, nameCapture{
					start: int(node.StartByte()),
					text:  node.Utf8Text(source),
				})
			case cname == "label":
				labelText = node.Utf8Text(source)
			}
		}

		// A @label capture (e.g. HCL's block-type identifier) overrides the
		// static @definition.<suffix> label so the breadcrumb reads naturally.
		label := suffixLabel
		if labelText != "" {
			label = labelText
		}
		// @name may appear multiple times (e.g. an HCL block's labels); join the
		// captures in source order to form the display name.
		name := joinNames(nameParts)

		if defNode == nil {
			continue
		}
		id := defNode.Id()
		entry := defEntry{name: name, label: label}
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

// nameCapture is one @name capture: its start byte (for source-order joining)
// and text.
type nameCapture struct {
	start int
	text  string
}

// joinNames orders @name captures by source position and joins them with a
// single space, producing the display name (e.g. an HCL block's labels).
func joinNames(parts []nameCapture) string {
	sort.SliceStable(parts, func(i, j int) bool {
		return parts[i].start < parts[j].start
	})
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	return strings.Join(texts, " ")
}

// entry returns the defEntry for node, if it was captured as a @definition.*.
func (d *defIndex) entry(node *tree_sitter.Node) (defEntry, bool) {
	if d == nil {
		return defEntry{}, false
	}
	e, ok := d.info[node.Id()]
	return e, ok
}

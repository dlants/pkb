package chunk

import (
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

func parseFor(t *testing.T, source []byte, grammar string) (*tree_sitter.Tree, *tree_sitter.Node) {
	t.Helper()
	lang := languageFor(grammar)
	if lang == nil {
		t.Fatalf("no language for grammar %q", grammar)
	}
	parser := tree_sitter.NewParser()
	t.Cleanup(parser.Close)
	if err := parser.SetLanguage(lang); err != nil {
		t.Fatalf("set language: %v", err)
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		t.Fatal("nil tree")
	}
	t.Cleanup(tree.Close)
	root := tree.RootNode()
	if root == nil {
		t.Fatal("nil root")
	}
	return tree, root
}

// findEntry locates the defEntry for a definition with the given name and label.
func findEntry(idx *defIndex, name, label string) (defEntry, bool) {
	for _, e := range idx.info {
		if e.name == name && e.label == label {
			return e, true
		}
	}
	return defEntry{}, false
}

func TestBuildDefIndexGo(t *testing.T) {
	src := []byte("package p\n\n// Foo does a thing.\nfunc Foo() int {\n\treturn 1\n}\n\ntype Bar struct {\n\tX int\n}\n")
	_, root := parseFor(t, src, "go")

	idx, err := buildDefIndex(root, src, "go")
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("nil index for go")
	}

	foo, ok := findEntry(idx, "Foo", "function")
	if !ok {
		t.Fatalf("Foo function not indexed: %+v", idx.info)
	}
	if foo.docStartByte < 0 {
		t.Fatalf("expected Foo to carry a doc comment, got docStartByte=%d", foo.docStartByte)
	}
	docByteIdx := foo.docStartByte
	if got := string(src[docByteIdx : docByteIdx+2]); got != "//" {
		t.Fatalf("docStartByte should point at the comment, got %q", got)
	}

	bar, ok := findEntry(idx, "Bar", "type")
	if !ok {
		t.Fatalf("Bar type not indexed: %+v", idx.info)
	}
	if bar.docStartByte != -1 {
		t.Fatalf("expected Bar to have no doc, got %d", bar.docStartByte)
	}
}

func TestBuildDefIndexTypescript(t *testing.T) {
	src := []byte("function foo(): number {\n  return 1;\n}\n\nclass Bar {\n  alpha(): void {}\n}\n")
	_, root := parseFor(t, src, "typescript")

	idx, err := buildDefIndex(root, src, "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("nil index for typescript")
	}

	if _, ok := findEntry(idx, "foo", "function"); !ok {
		t.Fatalf("foo function not indexed: %+v", idx.info)
	}
	if _, ok := findEntry(idx, "Bar", "class"); !ok {
		t.Fatalf("Bar class not indexed: %+v", idx.info)
	}
	if _, ok := findEntry(idx, "alpha", "method"); !ok {
		t.Fatalf("alpha method not indexed: %+v", idx.info)
	}
}

func findSpan(idx *defIndex, name, label string) (defSpan, bool) {
	for _, s := range idx.spans {
		if s.name == name && s.label == label {
			return s, true
		}
	}
	return defSpan{}, false
}

func TestBuildDefIndexSpansGo(t *testing.T) {
	src := []byte("package p\n\n// Foo does a thing.\nfunc Foo() int {\n\treturn 1\n}\n\nfunc (b Bar) M() {}\n\ntype State struct {\n\tX int\n}\n")
	_, root := parseFor(t, src, "go")

	idx, err := buildDefIndex(root, src, "go")
	if err != nil {
		t.Fatal(err)
	}
	if idx == nil {
		t.Fatal("nil index for go")
	}

	if len(idx.spans) != 3 {
		t.Fatalf("expected 3 spans, got %d: %+v", len(idx.spans), idx.spans)
	}

	// Spans are ordered by start byte.
	for i := 1; i < len(idx.spans); i++ {
		if idx.spans[i-1].start > idx.spans[i].start {
			t.Fatalf("spans not ordered by start: %+v", idx.spans)
		}
	}

	foo, ok := findSpan(idx, "Foo", "function")
	if !ok {
		t.Fatalf("Foo function span missing: %+v", idx.spans)
	}
	if got := string(src[foo.start : foo.start+len("func Foo")]); got != "func Foo" {
		t.Fatalf("Foo span should start at the func keyword, got %q", got)
	}

	m, ok := findSpan(idx, "M", "method")
	if !ok {
		t.Fatalf("M method span missing: %+v", idx.spans)
	}
	if m.start >= m.end {
		t.Fatalf("M span has empty interval: %+v", m)
	}

	state, ok := findSpan(idx, "State", "type")
	if !ok {
		t.Fatalf("State type span missing: %+v", idx.spans)
	}
	if got := string(src[state.start : state.start+len("State")]); got != "State" {
		t.Fatalf("State span should start at the type_spec node (State), got %q", got)
	}
	if state.docStartByte != -1 {
		t.Fatalf("expected State to have no doc, got %d", state.docStartByte)
	}
}

func TestBuildDefIndexNoQuery(t *testing.T) {
	src := []byte("x = 1\n")
	idx, err := buildDefIndex(nil, src, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if idx != nil {
		t.Fatalf("expected nil index for grammar without query, got %+v", idx)
	}
}

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

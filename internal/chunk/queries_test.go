package chunk

import (
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

func TestVendoredQueriesCompile(t *testing.T) {
	for name := range grammars {
		src := queryFor(name)
		if src == "" {
			// Some grammars (e.g. HCL) intentionally ship no tags.scm and use
			// the heuristic chunking path instead.
			continue
		}
		lang := languageFor(name)
		if lang == nil {
			t.Errorf("grammar %q: nil language", name)
			continue
		}
		q, qErr := tree_sitter.NewQuery(lang, src)
		if qErr != nil {
			t.Errorf("grammar %q: query failed to compile: %v", name, qErr)
			continue
		}
		if q.PatternCount() == 0 {
			t.Errorf("grammar %q: query has zero patterns", name)
		}
		q.Close()
	}
}

func TestQueryForUnknownGrammar(t *testing.T) {
	if got := queryFor("nonexistent"); got != "" {
		t.Errorf("queryFor(unknown) = %q, want empty", got)
	}
}

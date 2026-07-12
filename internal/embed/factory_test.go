package embed

import "testing"

// TestBuildProvidersAreContextual ensures every supported provider yields a
// model that implements ContextualEmbeddingModel, since text files always route
// through the contextual document-embedding path.
func TestBuildProvidersAreContextual(t *testing.T) {
	for _, provider := range []string{"", "voyage", "mock"} {
		m, err := Build(provider, "test-model", 8, "", "")
		if err != nil {
			t.Fatalf("Build(%q) error: %v", provider, err)
		}
		if _, ok := m.(ContextualEmbeddingModel); !ok {
			t.Fatalf("provider %q does not implement ContextualEmbeddingModel", provider)
		}
	}
}

func TestBuildRejectsUnknownProvider(t *testing.T) {
	if _, err := Build("nope", "m", 8, "", ""); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

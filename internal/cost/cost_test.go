package cost

import "testing"

func TestEmbeddingPricePerToken(t *testing.T) {
	// Voyage contextualized-chunk models resolve by substring (with @dims).
	if got := EmbeddingPricePerToken("voyage-context-4@256"); got != perMillion(0.12) {
		t.Fatalf("voyage-context-4 price = %v, want %v", got, perMillion(0.12))
	}
	if got := EmbeddingPricePerToken("voyage-context-3@256"); got != perMillion(0.18) {
		t.Fatalf("voyage-context-3 price = %v, want %v", got, perMillion(0.18))
	}
	// Unknown model falls back conservatively (compare via the same runtime
	// division to avoid constant-folding precision differences).
	wantFallback := perMillion(fallbackEmbeddingPerM)
	if got := EmbeddingPricePerToken("mystery@128"); got != wantFallback {
		t.Fatalf("unknown price = %v, want fallback %v", got, wantFallback)
	}
}

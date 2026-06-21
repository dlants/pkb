package cost

import "testing"

func TestEmbeddingPricePerToken(t *testing.T) {
	// Known model (with the @dims suffix the index uses) resolves by substring.
	got := EmbeddingPricePerToken("voyage-code-3@256")
	want := perMillion(0.18)
	if got != want {
		t.Fatalf("voyage-code-3@256 price = %v, want %v", got, want)
	}
	// Bedrock-style id with profile prefix and version suffix.
	if got := EmbeddingPricePerToken("us.cohere.embed-v4:0"); got != perMillion(0.12) {
		t.Fatalf("embed-v4 price = %v, want %v", got, perMillion(0.12))
	}
	// Unknown model falls back conservatively (compare via the same runtime
	// division to avoid constant-folding precision differences).
	wantFallback := perMillion(fallbackEmbeddingPerM)
	if got := EmbeddingPricePerToken("mystery@128"); got != wantFallback {
		t.Fatalf("unknown price = %v, want fallback %v", got, wantFallback)
	}
}

func TestInferencePricePerToken(t *testing.T) {
	p := InferencePricePerToken("claude-haiku-4-5")
	if p.InputPerToken != perMillion(1.0) || p.OutputPerToken != perMillion(5.0) {
		t.Fatalf("haiku price = %+v", p)
	}
	fb := InferencePricePerToken("unknown-model")
	if fb.InputPerToken != perMillion(fallbackInferencePerM.InputPerToken) ||
		fb.OutputPerToken != perMillion(fallbackInferencePerM.OutputPerToken) {
		t.Fatalf("fallback price = %+v", fb)
	}
}

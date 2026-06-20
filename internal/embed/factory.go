package embed

import (
	"context"
	"fmt"
)

// Build constructs an embedding model from a provider/model/dimensions
// selection (as recorded in the repo config). The "bedrock" provider returns a
// Cohere-on-Bedrock model; "openai"/"openai-compatible" returns an
// OpenAI-shaped HTTP model (OpenAI cloud, Ollama, etc.); "gemini" returns a
// Google Generative Language model; "mock" returns a
// deterministic test model.
func Build(provider, model string, dims int, region, profile, baseURL, apiKeyEnv string) (EmbeddingModel, error) {
	switch provider {
	case "bedrock", "":
		return NewBedrockCohere(context.Background(), region, profile, model, dims)
	case "openai", "openai-compatible":
		return NewOpenAICompatible(baseURL, apiKeyEnv, model, dims)
	case "gemini":
		return NewGemini(baseURL, apiKeyEnv, model, dims)
	case "mock":
		return NewMockModel(model, dims), nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q", provider)
	}
}

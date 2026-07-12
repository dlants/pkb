package embed

import "fmt"

// Build constructs an embedding model from a provider/model/dimensions
// selection (as recorded in the repo config). The "voyage" provider returns a
// Voyage AI contextualized model; "mock" returns a deterministic test model.
// Only providers that support contextualized whole-document embeddings are
// supported, so text files can always be embedded via the contextual path.
func Build(provider, model string, dims int, baseURL, apiKeyEnv string) (EmbeddingModel, error) {
	switch provider {
	case "voyage", "":
		return NewVoyage(baseURL, apiKeyEnv, model, dims)
	case "mock":
		return NewMockModel(model, dims), nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q", provider)
	}
}

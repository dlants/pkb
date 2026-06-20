// Package infer provides text-in/text-out inference models used to augment
// markdown/text chunks before embedding (the contextual-retrieval pattern).
// It mirrors the embed package: a stable InferenceModel interface plus a Build
// provider switch dispatching to Bedrock (Claude), OpenAI-compatible, Gemini,
// or a deterministic mock.
package infer

import (
	"context"
	"fmt"
)

// InferenceModel is the minimal text-in/text-out contract augmentation needs.
type InferenceModel interface {
	// ModelName returns the model identifier, folded into the text-file reuse
	// key so switching the inference model invalidates stale augmentations.
	ModelName() string
	// Complete returns the model's text completion for the given prompt.
	Complete(prompt string) (string, error)
}

// Build constructs an inference model from a provider/model selection (as
// recorded in the repo config). "bedrock" returns a Claude-on-Bedrock model;
// "openai"/"openai-compatible" returns an OpenAI chat-completions model (OpenAI
// cloud, Ollama, etc.); "gemini" returns a Google Generative Language model;
// "anthropic" returns an Anthropic Messages API (Claude) model; "mock" returns
// a deterministic test model. "none" (or empty) disables
// inference and returns a nil model with no error so callers fall back to the
// deterministic heading-prefix path.
func Build(provider, model, region, profile, baseURL, apiKeyEnv string) (InferenceModel, error) {
	switch provider {
	case "none", "":
		return nil, nil
	case "bedrock":
		return NewBedrockClaude(context.Background(), region, profile, model)
	case "openai", "openai-compatible":
		return NewOpenAICompatible(baseURL, apiKeyEnv, model)
	case "gemini":
		return NewGemini(baseURL, apiKeyEnv, model)
	case "anthropic":
		return NewAnthropic(baseURL, apiKeyEnv, model)
	case "mock":
		return NewMockModel(model), nil
	default:
		return nil, fmt.Errorf("unknown inference provider %q", provider)
	}
}

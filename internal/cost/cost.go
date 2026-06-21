// Package cost holds the pricing table and unit conversions used to estimate
// the dollar cost of a reindex run before any paid embedding/inference work is
// done. Prices are approximate ($/1M tokens) and easy to update in one place;
// the versioning/reuse logic does not depend on them.
package cost

import "strings"

// CharsPerToken approximates token count from character count. It matches the
// rough proxy used elsewhere for throughput reporting; estimation only needs an
// order-of-magnitude figure to gate spending.
const CharsPerToken = 3

// AugmentMaxTokens is the assumed output length of an augmentation reply, used
// as a conservative per-chunk output-token cap when projecting inference cost.
// It mirrors infer.augmentMaxTokens.
const AugmentMaxTokens = 512

// Pricing is a model's price in dollars per token (input and, for inference
// models, output).
type Pricing struct {
	InputPerToken  float64
	OutputPerToken float64
}

// perMillion converts a $/1M-tokens rate to $/token.
func perMillion(d float64) float64 { return d / 1_000_000 }

// embeddingPricesPerM maps an embedding model id to its input price per 1M
// tokens. Keys are matched as substrings of the model name (which may carry a
// "@dims" suffix or a Bedrock profile prefix), so e.g. "voyage-code-3@256" and
// "us.cohere.embed-v4:0" both resolve.
var embeddingPricesPerM = map[string]float64{
	"voyage-code-3":          0.18,
	"voyage-3":               0.18,
	"voyage-2":               0.10,
	"embed-v4":               0.12,
	"embed-english":          0.10,
	"text-embedding-3-small": 0.02,
	"text-embedding-3-large": 0.13,
}

// inferencePricesPerM maps an inference model id to its input/output price per
// 1M tokens. Keys are matched as substrings of the model name.
var inferencePricesPerM = map[string]Pricing{
	"claude-haiku-4-5": {InputPerToken: 1.0, OutputPerToken: 5.0},
	"claude-3-5-haiku": {InputPerToken: 0.80, OutputPerToken: 4.0},
	"claude-haiku":     {InputPerToken: 1.0, OutputPerToken: 5.0},
	"claude-sonnet":    {InputPerToken: 3.0, OutputPerToken: 15.0},
	"gpt-4o-mini":      {InputPerToken: 0.15, OutputPerToken: 0.60},
	"gpt-4o":           {InputPerToken: 2.5, OutputPerToken: 10.0},
}

// fallbackEmbeddingPerM is a conservative input price for an unknown embedding
// model.
const fallbackEmbeddingPerM = 0.20

// fallbackInferencePerM is a conservative input/output price for an unknown
// inference model.
var fallbackInferencePerM = Pricing{InputPerToken: 3.0, OutputPerToken: 15.0}

// EmbeddingPricePerToken returns the input price ($/token) for an embedding
// model name, falling back conservatively for unknown models.
func EmbeddingPricePerToken(model string) float64 {
	for key, price := range embeddingPricesPerM {
		if strings.Contains(model, key) {
			return perMillion(price)
		}
	}
	return perMillion(fallbackEmbeddingPerM)
}

// InferencePricePerToken returns the input/output price ($/token) for an
// inference model name, falling back conservatively for unknown models.
func InferencePricePerToken(model string) Pricing {
	for key, price := range inferencePricesPerM {
		if strings.Contains(model, key) {
			return Pricing{InputPerToken: perMillion(price.InputPerToken), OutputPerToken: perMillion(price.OutputPerToken)}
		}
	}
	return Pricing{InputPerToken: perMillion(fallbackInferencePerM.InputPerToken), OutputPerToken: perMillion(fallbackInferencePerM.OutputPerToken)}
}

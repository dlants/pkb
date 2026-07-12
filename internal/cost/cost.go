// Package cost holds the pricing table and unit conversions used to estimate
// the dollar cost of a reindex run before any paid embedding work is done.
// Prices are approximate ($/1M tokens) and easy to update in one place;
// the versioning/reuse logic does not depend on them.
package cost

import "strings"

// CharsPerToken approximates token count from character count. It matches the
// rough proxy used elsewhere for throughput reporting; estimation only needs an
// order-of-magnitude figure to gate spending.
const CharsPerToken = 3

// perMillion converts a $/1M-tokens rate to $/token.
func perMillion(d float64) float64 { return d / 1_000_000 }

// embeddingPricesPerM maps an embedding model id to its input price per 1M
// tokens. Keys are matched as substrings of the model name (which may carry a
// "@dims" suffix), so e.g. "voyage-context-3@256" resolves.
var embeddingPricesPerM = map[string]float64{
	"voyage-context-4": 0.12,
	"voyage-context-3": 0.18,
}

// fallbackEmbeddingPerM is a conservative input price for an unknown embedding
// model.
const fallbackEmbeddingPerM = 0.20

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

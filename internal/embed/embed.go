// Package embed defines the embedding-model interface used to embed all files
// (code and text) via a single model.
package embed

// Embedding is a dense vector representation of a chunk or query.
type Embedding []float32

// EmbeddingModel produces embeddings via some backend (Bedrock, mock, etc.).
type EmbeddingModel interface {
	// ModelName uniquely identifies the model; vec tables are keyed by it.
	ModelName() string
	// Dimensions is the length of every embedding the model returns.
	Dimensions() int
	EmbedChunk(chunk string) (Embedding, error)
	EmbedQuery(query string) (Embedding, error)
	EmbedChunks(chunks []string) ([]Embedding, error)
}

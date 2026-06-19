// Package embed defines the embedding-model interface used for both code and
// text embeddings. A Context holds two instances (a code model and a text
// model); a file's type decides which one embeds it.
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

// Package embed defines the embedding-model interface used to embed all files
// (code and text) via a single model.
package embed

// Embedding is a dense vector representation of a chunk or query.
type Embedding []float32

// EmbeddingModel produces embeddings via some backend (Voyage, mock, etc.).
type EmbeddingModel interface {
	// ModelName uniquely identifies the model; vec tables are keyed by it.
	ModelName() string
	// Dimensions is the length of every embedding the model returns.
	Dimensions() int
	EmbedChunk(chunk string) (Embedding, error)
	EmbedQuery(query string) (Embedding, error)
	EmbedChunks(chunks []string) ([]Embedding, error)
}

// ContextualChunk is a single model-chosen chunk returned by an auto-chunking
// contextual embedding call: the chunk's text paired with its vector.
type ContextualChunk struct {
	Text      string
	Embedding Embedding
}

// ContextualEmbeddingModel is implemented by providers that can auto-chunk and
// embed a whole document in one call, returning the model-chosen chunks with
// their contextualized vectors. It is an optional capability on top of
// EmbeddingModel; callers should type-assert to detect support.
type ContextualEmbeddingModel interface {
	EmbeddingModel
	// EmbedDocument sends the whole document to the provider's auto-chunking
	// endpoint and returns the model-chosen chunks in order, each paired with
	// its vector at Dimensions().
	EmbedDocument(document string) ([]ContextualChunk, error)
}

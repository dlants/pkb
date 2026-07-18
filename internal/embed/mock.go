package embed

import (
	"fmt"
	"strings"
	"sync"
)

// MockModel is a deterministic embedding model for tests. It records how many
// embedding calls it has received so tests can assert that reindex skipped work.
type MockModel struct {
	Name string
	Dims int

	mu            sync.Mutex
	chunkCalls    int
	queryCalls    int
	chunkCount    int
	documentCalls int
}

// NewMockModel returns a mock model with the given name and dimensions.
func NewMockModel(name string, dims int) *MockModel {
	return &MockModel{Name: name, Dims: dims}
}

func (m *MockModel) ModelName() string { return m.Name }
func (m *MockModel) Dimensions() int   { return m.Dims }

// deterministicVector derives a stable vector from the input text so the same
// chunk always embeds to the same point.
func (m *MockModel) deterministicVector(s string) Embedding {
	vec := make(Embedding, m.Dims)
	for i := 0; i < len(s); i++ {
		vec[i%m.Dims] += float32(s[i])
	}
	return vec
}

func (m *MockModel) EmbedChunk(chunk string) (Embedding, error) {
	m.mu.Lock()
	m.chunkCalls++
	m.chunkCount++
	m.mu.Unlock()
	return m.deterministicVector(chunk), nil
}

func (m *MockModel) EmbedQuery(query string) (Embedding, error) {
	m.mu.Lock()
	m.queryCalls++
	m.mu.Unlock()
	return m.deterministicVector(query), nil
}

func (m *MockModel) EmbedChunks(chunks []string) ([]Embedding, error) {
	m.mu.Lock()
	m.chunkCalls++
	m.chunkCount += len(chunks)
	m.mu.Unlock()
	out := make([]Embedding, len(chunks))
	for i, c := range chunks {
		out[i] = m.deterministicVector(c)
	}
	return out, nil
}

// EmbedDocument deterministically auto-chunks a document (splitting on blank
// lines) and returns each chunk paired with a contextualized vector. The
// contextual vector is sibling-dependent — it folds in the whole document — so
// it is distinct from the isolated vector for the same chunk text.
func (m *MockModel) EmbedDocument(document string) ([]ContextualChunk, error) {
	var out []ContextualChunk
	for _, part := range strings.Split(document, "\n\n") {
		text := strings.TrimSpace(part)
		if text == "" {
			continue
		}
		out = append(out, ContextualChunk{
			Text:      text,
			Embedding: m.contextualVector(text, document),
		})
	}
	m.mu.Lock()
	m.documentCalls++
	m.chunkCount += len(out)
	m.mu.Unlock()
	return out, nil
}

// contextualVector derives a vector from the chunk text and the surrounding
// document, so contextualized output differs from isolated embedding.
func (m *MockModel) contextualVector(chunk, document string) Embedding {
	vec := m.deterministicVector(chunk)
	ctx := m.deterministicVector(document)
	for i := range vec {
		vec[i] += ctx[i]
	}
	return vec
}

// DocumentCalls returns the number of EmbedDocument invocations.
func (m *MockModel) DocumentCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.documentCalls
}

// ChunkCalls returns the number of EmbedChunk+EmbedChunks invocations.
func (m *MockModel) ChunkCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chunkCalls
}

// ChunkCount returns the total number of chunks embedded.
func (m *MockModel) ChunkCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chunkCount
}

// FailingModel wraps a model and fails after FailAfter chunk-batches, used to
// test partial-run marker safety.
type FailingModel struct {
	*MockModel
	FailAfter int
	batches   int
}

func (f *FailingModel) EmbedChunks(chunks []string) ([]Embedding, error) {
	f.batches++
	if f.batches > f.FailAfter {
		return nil, fmt.Errorf("simulated embed failure")
	}
	return f.MockModel.EmbedChunks(chunks)
}

// EmbedDocument fails after FailAfter whole-document calls, mirroring
// EmbedChunks so the unified auto-chunk path can be crashed mid-run.
func (f *FailingModel) EmbedDocument(document string) ([]ContextualChunk, error) {
	f.batches++
	if f.batches > f.FailAfter {
		return nil, fmt.Errorf("simulated embed failure")
	}
	return f.MockModel.EmbedDocument(document)
}

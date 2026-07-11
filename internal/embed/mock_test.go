package embed

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMockModelImplementsContextual(t *testing.T) {
	var _ ContextualEmbeddingModel = NewMockModel("mock", 8)
}

func TestMockEmbedDocumentAutoChunks(t *testing.T) {
	m := NewMockModel("mock", 8)
	doc := "first paragraph\n\nsecond paragraph\n\n\nthird paragraph"

	chunks, err := m.EmbedDocument(doc)
	require.NoError(t, err)
	require.Len(t, chunks, 3)
	require.Equal(t, "first paragraph", chunks[0].Text)
	require.Equal(t, "second paragraph", chunks[1].Text)
	require.Equal(t, "third paragraph", chunks[2].Text)
	for _, c := range chunks {
		require.Len(t, c.Embedding, 8)
	}
	require.Equal(t, 1, m.DocumentCalls())
}

func TestMockContextualDiffersFromIsolated(t *testing.T) {
	m := NewMockModel("mock", 8)
	doc := "alpha chunk\n\nbeta chunk"

	chunks, err := m.EmbedDocument(doc)
	require.NoError(t, err)
	require.Len(t, chunks, 2)

	isolated, err := m.EmbedChunk("alpha chunk")
	require.NoError(t, err)
	require.NotEqual(t, isolated, chunks[0].Embedding)
}

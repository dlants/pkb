package embed

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newFakeEmbedServer(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req openAIEmbeddingRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var resp openAIEmbeddingResponse
		for i := range req.Input {
			vec := make([]float32, dims)
			for j := range vec {
				vec[j] = float32(i*dims + j)
			}
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestOpenAICompatibleEmbed(t *testing.T) {
	srv := newFakeEmbedServer(t, 4)
	defer srv.Close()

	m, err := NewOpenAICompatible(srv.URL, "OPENAI_API_KEY", "text-embedding-3-small", 4)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelName() != "text-embedding-3-small@4" {
		t.Fatalf("unexpected name %q", m.ModelName())
	}
	if m.Dimensions() != 4 {
		t.Fatalf("unexpected dims %d", m.Dimensions())
	}

	chunk, err := m.EmbedChunk("hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk) != 4 {
		t.Fatalf("expected 4 dims, got %d", len(chunk))
	}

	q, err := m.EmbedQuery("query")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 4 {
		t.Fatalf("expected 4 dims, got %d", len(q))
	}

	batch, err := m.EmbedChunks([]string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 {
		t.Fatalf("expected 3 embeddings, got %d", len(batch))
	}
	for i, e := range batch {
		if len(e) != 4 {
			t.Fatalf("embedding %d: expected 4 dims, got %d", i, len(e))
		}
	}
}

func TestOpenAICompatibleErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	m, err := NewOpenAICompatible(srv.URL, "OPENAI_API_KEY", "model", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestOpenAICompatibleEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	m, err := NewOpenAICompatible(srv.URL, "OPENAI_API_KEY", "model", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on mismatched data length")
	}
}

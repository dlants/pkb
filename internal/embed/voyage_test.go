package embed

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeVoyageServer(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/embeddings") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req voyageEmbeddingRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.InputType != "document" && req.InputType != "query" {
			http.Error(w, "missing input_type", http.StatusBadRequest)
			return
		}
		var resp voyageEmbeddingResponse
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

func TestVoyageEmbed(t *testing.T) {
	srv := newFakeVoyageServer(t, 4)
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-code-3", 4)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelName() != "voyage-code-3@4" {
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

func newFakeVoyageContextServer(t *testing.T, dims int, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/contextualizedembeddings") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req voyageContextRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !req.EnableAutoChunking {
			http.Error(w, "expected enable_auto_chunking", http.StatusBadRequest)
			return
		}
		if len(req.Inputs) != 1 || len(req.Inputs[0]) != 1 {
			http.Error(w, "expected a single whole-document input", http.StatusBadRequest)
			return
		}
		var resp voyageContextResponse
		var group struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
				Text      string    `json:"text"`
			} `json:"data"`
		}
		for i, text := range chunks {
			vec := make([]float32, dims)
			for j := range vec {
				vec[j] = float32(i*dims + j)
			}
			group.Data = append(group.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
				Text      string    `json:"text"`
			}{Embedding: vec, Index: i, Text: text})
		}
		resp.Data = append(resp.Data, group)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestVoyageEmbedDocument(t *testing.T) {
	want := []string{"chunk one", "chunk two", "chunk three"}
	srv := newFakeVoyageContextServer(t, 4, want)
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := m.EmbedDocument("some whole document")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != len(want) {
		t.Fatalf("expected %d chunks, got %d", len(want), len(chunks))
	}
	for i, c := range chunks {
		if c.Text != want[i] {
			t.Fatalf("chunk %d: expected text %q, got %q", i, want[i], c.Text)
		}
		if len(c.Embedding) != 4 {
			t.Fatalf("chunk %d: expected 4 dims, got %d", i, len(c.Embedding))
		}
	}
}

func TestVoyageEmbedDocumentIsContextualModel(t *testing.T) {
	m, err := NewVoyage("http://example.invalid", "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := interface{}(m).(ContextualEmbeddingModel); !ok {
		t.Fatal("Voyage should implement ContextualEmbeddingModel")
	}
}

func TestVoyageEmbedDocumentErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"bad key"}`))
	}))
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedDocument("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestVoyageEmbedDocumentDimensionMismatch(t *testing.T) {
	srv := newFakeVoyageContextServer(t, 8, []string{"a", "b"})
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedDocument("x"); err == nil {
		t.Fatal("expected error on dimension mismatch")
	}
}

func TestVoyageErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"bad key"}`))
	}))
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-code-3", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestVoyageDimensionMismatch(t *testing.T) {
	srv := newFakeVoyageServer(t, 8)
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-code-3", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on dimension mismatch")
	}
}

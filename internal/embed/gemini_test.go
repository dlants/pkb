package embed

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newFakeGeminiServer(t *testing.T, dims int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":batchEmbedContents") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req geminiEmbedRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var resp geminiEmbedResponse
		for i := range req.Requests {
			vec := make([]float32, dims)
			for j := range vec {
				vec[j] = float32(i*dims + j)
			}
			resp.Embeddings = append(resp.Embeddings, struct {
				Values []float32 `json:"values"`
			}{Values: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestGeminiEmbed(t *testing.T) {
	srv := newFakeGeminiServer(t, 4)
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "text-embedding-004", 4)
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelName() != "text-embedding-004@4" {
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

func TestGeminiErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "model", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestGeminiDimensionMismatch(t *testing.T) {
	srv := newFakeGeminiServer(t, 8)
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "model", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on dimension mismatch")
	}
}

func TestGeminiEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[]}`))
	}))
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "model", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunk("x"); err == nil {
		t.Fatal("expected error on mismatched data length")
	}
}

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

// capturedContextReq records the shape of one contextualized-endpoint request
// so tests can assert routing, auto-chunk mode, and batch splitting.
type capturedContextReq struct {
	inputType  string
	autoChunk  bool
	groupSizes []int
}

// newFakeVoyagePrechunkServer serves /v1/contextualizedembeddings for the
// pre-chunked (auto-chunk disabled) path, echoing one vector per input chunk
// and recording each request into captured.
func newFakeVoyagePrechunkServer(t *testing.T, dims int, captured *[]capturedContextReq) *httptest.Server {
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
		rec := capturedContextReq{inputType: req.InputType, autoChunk: req.EnableAutoChunking}
		var resp voyageContextResponse
		for gi, group := range req.Inputs {
			rec.groupSizes = append(rec.groupSizes, len(group))
			var g struct {
				Data []struct {
					Embedding []float32 `json:"embedding"`
					Index     int       `json:"index"`
					Text      string    `json:"text"`
				} `json:"data"`
			}
			for ci, text := range group {
				vec := make([]float32, dims)
				for j := range vec {
					vec[j] = float32(gi*dims + ci + j)
				}
				g.Data = append(g.Data, struct {
					Embedding []float32 `json:"embedding"`
					Index     int       `json:"index"`
					Text      string    `json:"text"`
				}{Embedding: vec, Index: ci, Text: text})
			}
			resp.Data = append(resp.Data, g)
		}
		*captured = append(*captured, rec)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestVoyageContextModelRoutesChunks(t *testing.T) {
	var captured []capturedContextReq
	srv := newFakeVoyagePrechunkServer(t, 4, &captured)
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
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
	if len(captured) != 1 {
		t.Fatalf("expected 1 request, got %d", len(captured))
	}
	req := captured[0]
	if req.autoChunk {
		t.Fatal("expected auto-chunking disabled for pre-chunked code path")
	}
	if req.inputType != "document" {
		t.Fatalf("expected input_type document, got %q", req.inputType)
	}
	if len(req.groupSizes) != 3 {
		t.Fatalf("expected 3 single-chunk groups, got %v", req.groupSizes)
	}
	for _, sz := range req.groupSizes {
		if sz != 1 {
			t.Fatalf("expected isolated single-chunk groups, got %v", req.groupSizes)
		}
	}
}

func TestVoyageContextModelEmbedQuery(t *testing.T) {
	var captured []capturedContextReq
	srv := newFakeVoyagePrechunkServer(t, 4, &captured)
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
	}
	q, err := m.EmbedQuery("some query")
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 4 {
		t.Fatalf("expected 4 dims, got %d", len(q))
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 request, got %d", len(captured))
	}
	if captured[0].inputType != "query" {
		t.Fatalf("expected input_type query, got %q", captured[0].inputType)
	}
	if captured[0].autoChunk {
		t.Fatal("expected auto-chunking disabled for query")
	}
	if len(captured[0].groupSizes) != 1 || captured[0].groupSizes[0] != 1 {
		t.Fatalf("expected a single single-chunk group, got %v", captured[0].groupSizes)
	}
}

func TestVoyageContextModelSplitsLargeBatch(t *testing.T) {
	var captured []capturedContextReq
	srv := newFakeVoyagePrechunkServer(t, 4, &captured)
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-context-4", 4)
	if err != nil {
		t.Fatal(err)
	}
	// Each chunk is 2/3 of the per-request char budget, so at most one chunk
	// fits per request and the batch must split into one request per chunk.
	big := strings.Repeat("x", voyageContextMaxBatchChars*2/3)
	chunks := []string{big, big, big}
	batch, err := m.EmbedChunks(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != len(chunks) {
		t.Fatalf("expected %d embeddings, got %d", len(chunks), len(batch))
	}
	if len(captured) != len(chunks) {
		t.Fatalf("expected %d split requests, got %d", len(chunks), len(captured))
	}
	total := 0
	for _, req := range captured {
		total += len(req.groupSizes)
	}
	if total != len(chunks) {
		t.Fatalf("expected %d total groups across requests, got %d", len(chunks), total)
	}
}

func TestVoyageStandardModelUsesEmbeddingsEndpoint(t *testing.T) {
	var contextHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/contextualizedembeddings") {
			contextHit = true
			http.Error(w, "context endpoint should not be hit", http.StatusBadRequest)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/embeddings") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req voyageEmbeddingRequest
		_ = json.Unmarshal(body, &req)
		var resp voyageEmbeddingResponse
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: make([]float32, 4)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	m, err := NewVoyage(srv.URL, "VOYAGE_API_KEY", "voyage-code-3", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.EmbedChunks([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if contextHit {
		t.Fatal("standard model must not use the contextualized endpoint")
	}
}

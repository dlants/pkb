package infer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req geminiGenerateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Contents) != 1 || req.Contents[0].Parts[0].Text != "hello" {
			http.Error(w, "bad contents", http.StatusBadRequest)
			return
		}
		var resp geminiGenerateResponse
		resp.Candidates = append(resp.Candidates, struct {
			Content geminiContent `json:"content"`
		}{Content: geminiContent{Parts: []geminiPart{{Text: "world"}}}})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "gemini-2.0-flash")
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelName() != "gemini-2.0-flash" {
		t.Fatalf("unexpected name %q", m.ModelName())
	}
	out, err := m.Complete("hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != "world" {
		t.Fatalf("unexpected completion %q", out)
	}
}

func TestGeminiCompleteErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestGeminiCompleteNoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer srv.Close()

	m, err := NewGemini(srv.URL, "GEMINI_API_KEY", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete("x"); err == nil {
		t.Fatal("expected error on empty candidates")
	}
}

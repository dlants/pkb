package infer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Header.Get("anthropic-version") == "" {
			http.Error(w, "missing version header", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req anthropicMessagesRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "hello" {
			http.Error(w, "bad messages", http.StatusBadRequest)
			return
		}
		var resp anthropicMessagesResponse
		resp.Content = append(resp.Content, struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: "world"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	m, err := NewAnthropic(srv.URL, "ANTHROPIC_API_KEY", "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelName() != "claude-haiku-4-5" {
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

func TestAnthropicCompleteErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	m, err := NewAnthropic(srv.URL, "ANTHROPIC_API_KEY", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestAnthropicCompleteNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer srv.Close()

	m, err := NewAnthropic(srv.URL, "ANTHROPIC_API_KEY", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete("x"); err == nil {
		t.Fatal("expected error on empty content")
	}
}

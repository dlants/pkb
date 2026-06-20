package infer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAICompatibleComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req openAIChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "hello" {
			http.Error(w, "bad messages", http.StatusBadRequest)
			return
		}
		var resp openAIChatResponse
		resp.Choices = append(resp.Choices, struct {
			Message openAIChatMessage `json:"message"`
		}{Message: openAIChatMessage{Role: "assistant", Content: "world"}})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	m, err := NewOpenAICompatible(srv.URL, "OPENAI_API_KEY", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if m.ModelName() != "gpt-4o-mini" {
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

func TestOpenAICompatibleCompleteErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	m, err := NewOpenAICompatible(srv.URL, "OPENAI_API_KEY", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete("x"); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestOpenAICompatibleCompleteNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	m, err := NewOpenAICompatible(srv.URL, "OPENAI_API_KEY", "model")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Complete("x"); err == nil {
		t.Fatal("expected error on empty choices")
	}
}

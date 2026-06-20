package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Gemini embeds text via Google's Generative Language API
// (`{baseURL}/v1beta/models/{model}:batchEmbedContents`). The API key is read
// from the env var named by apiKeyEnv (default GEMINI_API_KEY) and passed as a
// `key` query parameter.
type Gemini struct {
	client  *http.Client
	baseURL string
	apiKey  string
	modelID string
	name    string
	dims    int
}

type geminiEmbedRequest struct {
	Requests []geminiEmbedSingle `json:"requests"`
}

type geminiEmbedSingle struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	OutputDimensionality int           `json:"outputDimensionality,omitempty"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// NewGemini builds a Gemini embedding model. baseURL defaults to
// https://generativelanguage.googleapis.com; apiKeyEnv names the env var
// holding the key (default GEMINI_API_KEY). modelID is normalized to the
// `models/<id>` form the API expects.
func NewGemini(baseURL, apiKeyEnv, modelID string, dims int) (*Gemini, error) {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if apiKeyEnv == "" {
		apiKeyEnv = "GEMINI_API_KEY"
	}
	return &Gemini{
		client:  &http.Client{Timeout: 60 * time.Second},
		baseURL: baseURL,
		apiKey:  os.Getenv(apiKeyEnv),
		modelID: modelID,
		name:    fmt.Sprintf("%s@%d", modelID, dims),
		dims:    dims,
	}, nil
}

// ModelName returns the model identifier used to key vec tables.
func (g *Gemini) ModelName() string { return g.name }

// Dimensions returns the embedding dimensionality.
func (g *Gemini) Dimensions() int { return g.dims }

// EmbedChunk embeds a single document chunk.
func (g *Gemini) EmbedChunk(chunk string) (Embedding, error) {
	out, err := g.embed([]string{chunk})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedQuery embeds a single search query.
func (g *Gemini) EmbedQuery(query string) (Embedding, error) {
	out, err := g.embed([]string{query})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedChunks embeds a batch of document chunks.
func (g *Gemini) EmbedChunks(chunks []string) ([]Embedding, error) {
	return g.embed(chunks)
}

func (g *Gemini) modelPath() string {
	if strings.HasPrefix(g.modelID, "models/") {
		return g.modelID
	}
	return "models/" + g.modelID
}

func (g *Gemini) embed(texts []string) ([]Embedding, error) {
	reqBody := geminiEmbedRequest{Requests: make([]geminiEmbedSingle, len(texts))}
	for i, t := range texts {
		reqBody.Requests[i] = geminiEmbedSingle{
			Model:                g.modelPath(),
			Content:              geminiContent{Parts: []geminiPart{{Text: t}}},
			OutputDimensionality: g.dims,
		}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1beta/%s:batchEmbedContents", g.baseURL, g.modelPath())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		q := req.URL.Query()
		q.Set("key", g.apiKey)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini embeddings request: %w", err)
	}
	defer resp.Body.Close()
	var parsed geminiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding gemini response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil {
			return nil, fmt.Errorf("gemini embeddings status %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return nil, fmt.Errorf("gemini embeddings status %d", resp.StatusCode)
	}
	if len(parsed.Embeddings) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(parsed.Embeddings))
	}
	out := make([]Embedding, len(parsed.Embeddings))
	for i, e := range parsed.Embeddings {
		if len(e.Values) != g.dims {
			return nil, fmt.Errorf("embedding %d: expected %d dims, got %d", i, g.dims, len(e.Values))
		}
		out[i] = Embedding(e.Values)
	}
	return out, nil
}

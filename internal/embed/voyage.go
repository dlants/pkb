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

// Voyage embeds text via Voyage AI's `/v1/embeddings` endpoint. The API key is
// read from the env var named by apiKeyEnv (default VOYAGE_API_KEY) and passed
// as a Bearer token. Chunks and queries are tagged with the appropriate
// input_type ("document"/"query") to use Voyage's asymmetric retrieval prompts.
type Voyage struct {
	client  *http.Client
	baseURL string
	apiKey  string
	modelID string
	name    string
	dims    int
}

type voyageEmbeddingRequest struct {
	Model           string   `json:"model"`
	Input           []string `json:"input"`
	InputType       string   `json:"input_type,omitempty"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

type voyageContextRequest struct {
	Model              string     `json:"model"`
	Inputs             [][]string `json:"inputs"`
	InputType          string     `json:"input_type,omitempty"`
	OutputDimension    int        `json:"output_dimension,omitempty"`
	EnableAutoChunking bool       `json:"enable_auto_chunking"`
}

type voyageContextResponse struct {
	Data []struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
			Text      string    `json:"text"`
		} `json:"data"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Detail string `json:"detail"`
}

type voyageEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Detail string `json:"detail"`
}

// NewVoyage builds a Voyage AI embedding model. baseURL defaults to
// https://api.voyageai.com; apiKeyEnv names the env var holding the key
// (default VOYAGE_API_KEY).
func NewVoyage(baseURL, apiKeyEnv, modelID string, dims int) (*Voyage, error) {
	if baseURL == "" {
		baseURL = "https://api.voyageai.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if apiKeyEnv == "" {
		apiKeyEnv = "VOYAGE_API_KEY"
	}
	return &Voyage{
		client:  &http.Client{Timeout: 60 * time.Second},
		baseURL: baseURL,
		apiKey:  os.Getenv(apiKeyEnv),
		modelID: modelID,
		name:    fmt.Sprintf("%s@%d", modelID, dims),
		dims:    dims,
	}, nil
}

// ModelName returns the model identifier used to key vec tables.
func (v *Voyage) ModelName() string { return v.name }

// Dimensions returns the embedding dimensionality.
func (v *Voyage) Dimensions() int { return v.dims }

// EmbedChunk embeds a single document chunk.
func (v *Voyage) EmbedChunk(chunk string) (Embedding, error) {
	out, err := v.embed([]string{chunk}, "document")
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedQuery embeds a single search query.
func (v *Voyage) EmbedQuery(query string) (Embedding, error) {
	out, err := v.embed([]string{query}, "query")
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedChunks embeds a batch of document chunks.
func (v *Voyage) EmbedChunks(chunks []string) ([]Embedding, error) {
	return v.embed(chunks, "document")
}

// EmbedDocument sends the whole document to Voyage's contextualized chunk
// endpoint with auto-chunking enabled and returns the model-chosen chunks in
// order, each paired with its contextualized vector.
func (v *Voyage) EmbedDocument(document string) ([]ContextualChunk, error) {
	body, err := json.Marshal(voyageContextRequest{
		Model:              v.modelID,
		Inputs:             [][]string{{document}},
		InputType:          "document",
		OutputDimension:    v.dims,
		EnableAutoChunking: true,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, v.baseURL+"/v1/contextualizedembeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if v.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+v.apiKey)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage contextualizedembeddings request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed voyageContextResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding voyage response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch {
		case parsed.Error != nil && parsed.Error.Message != "":
			return nil, fmt.Errorf("voyage contextualizedembeddings status %d: %s", resp.StatusCode, parsed.Error.Message)
		case parsed.Detail != "":
			return nil, fmt.Errorf("voyage contextualizedembeddings status %d: %s", resp.StatusCode, parsed.Detail)
		default:
			return nil, fmt.Errorf("voyage contextualizedembeddings status %d", resp.StatusCode)
		}
	}
	if len(parsed.Data) != 1 {
		return nil, fmt.Errorf("expected 1 document group, got %d", len(parsed.Data))
	}
	items := parsed.Data[0].Data
	out := make([]ContextualChunk, len(items))
	for i, d := range items {
		if len(d.Embedding) != v.dims {
			return nil, fmt.Errorf("chunk %d: expected %d dims, got %d", i, v.dims, len(d.Embedding))
		}
		out[i] = ContextualChunk{Text: d.Text, Embedding: Embedding(d.Embedding)}
	}
	return out, nil
}

func (v *Voyage) embed(texts []string, inputType string) ([]Embedding, error) {
	body, err := json.Marshal(voyageEmbeddingRequest{
		Model:           v.modelID,
		Input:           texts,
		InputType:       inputType,
		OutputDimension: v.dims,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, v.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if v.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+v.apiKey)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage embeddings request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed voyageEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding voyage response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch {
		case parsed.Error != nil && parsed.Error.Message != "":
			return nil, fmt.Errorf("voyage embeddings status %d: %s", resp.StatusCode, parsed.Error.Message)
		case parsed.Detail != "":
			return nil, fmt.Errorf("voyage embeddings status %d: %s", resp.StatusCode, parsed.Detail)
		default:
			return nil, fmt.Errorf("voyage embeddings status %d", resp.StatusCode)
		}
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(parsed.Data))
	}
	out := make([]Embedding, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) != v.dims {
			return nil, fmt.Errorf("embedding %d: expected %d dims, got %d", i, v.dims, len(d.Embedding))
		}
		out[i] = Embedding(d.Embedding)
	}
	return out, nil
}

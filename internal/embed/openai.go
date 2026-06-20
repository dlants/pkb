package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// OpenAICompatible embeds text via any server speaking the OpenAI
// `/v1/embeddings` wire format: OpenAI cloud, Ollama, llama.cpp, vLLM,
// LM Studio, LocalAI, text-embeddings-inference. Providers are selected purely
// by base URL and API-key env var.
type OpenAICompatible struct {
	client  *http.Client
	baseURL string
	apiKey  string
	modelID string
	name    string
	dims    int
}

type openAIEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openAIEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// NewOpenAICompatible builds an OpenAI-compatible embedding model. baseURL
// defaults to https://api.openai.com; apiKeyEnv names the env var holding the
// key (default OPENAI_API_KEY). A missing key is tolerated (empty Authorization)
// so local servers like Ollama work without credentials.
func NewOpenAICompatible(baseURL, apiKeyEnv, modelID string, dims int) (*OpenAICompatible, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	if apiKeyEnv == "" {
		apiKeyEnv = "OPENAI_API_KEY"
	}
	return &OpenAICompatible{
		client:  &http.Client{Timeout: 60 * time.Second},
		baseURL: baseURL,
		apiKey:  os.Getenv(apiKeyEnv),
		modelID: modelID,
		name:    fmt.Sprintf("%s@%d", modelID, dims),
		dims:    dims,
	}, nil
}

// ModelName returns the model identifier used to key vec tables.
func (o *OpenAICompatible) ModelName() string { return o.name }

// Dimensions returns the embedding dimensionality.
func (o *OpenAICompatible) Dimensions() int { return o.dims }

// EmbedChunk embeds a single document chunk.
func (o *OpenAICompatible) EmbedChunk(chunk string) (Embedding, error) {
	out, err := o.embed([]string{chunk})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedQuery embeds a single search query.
func (o *OpenAICompatible) EmbedQuery(query string) (Embedding, error) {
	out, err := o.embed([]string{query})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedChunks embeds a batch of document chunks.
func (o *OpenAICompatible) EmbedChunks(chunks []string) ([]Embedding, error) {
	return o.embed(chunks)
}

func (o *OpenAICompatible) embed(texts []string) ([]Embedding, error) {
	body, err := json.Marshal(openAIEmbeddingRequest{Model: o.modelID, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, o.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embeddings request: %w", err)
	}
	defer resp.Body.Close()
	var parsed openAIEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding openai response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil {
			return nil, fmt.Errorf("openai embeddings status %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return nil, fmt.Errorf("openai embeddings status %d", resp.StatusCode)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(parsed.Data))
	}
	out := make([]Embedding, len(parsed.Data))
	for i, d := range parsed.Data {
		out[i] = Embedding(d.Embedding)
	}
	return out, nil
}

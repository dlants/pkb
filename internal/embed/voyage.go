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
	// contextual is true when modelID names a Voyage contextualized chunk
	// model (voyage-context-*). Those models are served only by the
	// /v1/contextualizedembeddings endpoint, so every operation routes there
	// (pre-supplying our own chunks with auto-chunking disabled) instead of the
	// standard /v1/embeddings endpoint.
	contextual bool
}

type voyageEmbeddingRequest struct {
	Model           string   `json:"model"`
	Input           []string `json:"input"`
	InputType       string   `json:"input_type,omitempty"`
	OutputDimension int      `json:"output_dimension,omitempty"`
}

// voyageContextMaxBatchChars bounds the total characters in a single
// contextualized-endpoint request when auto-chunking is disabled. The endpoint
// caps such requests at 32K tokens across all inputs; at a conservative ~3
// chars/token that is ~96K chars, so we keep a margin.
const voyageContextMaxBatchChars = 90000

// ContextTokenLimitError reports that a contextualized-endpoint request exceeded
// the per-request token cap (HTTP 400). It is distinguished from other request
// errors so callers can retry with a smaller input (e.g. splitting a document
// window) instead of failing the run.
type ContextTokenLimitError struct {
	Status  int
	Message string
}

func (e *ContextTokenLimitError) Error() string {
	return fmt.Sprintf("voyage contextualizedembeddings status %d: %s", e.Status, e.Message)
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
		client:     &http.Client{Timeout: 60 * time.Second},
		baseURL:    baseURL,
		apiKey:     os.Getenv(apiKeyEnv),
		modelID:    modelID,
		name:       fmt.Sprintf("%s@%d", modelID, dims),
		dims:       dims,
		contextual: strings.HasPrefix(modelID, "voyage-context"),
	}, nil
}

// ModelName returns the model identifier used to key vec tables.
func (v *Voyage) ModelName() string { return v.name }

// Dimensions returns the embedding dimensionality.
func (v *Voyage) Dimensions() int { return v.dims }

// EmbedChunk embeds a single document chunk.
func (v *Voyage) EmbedChunk(chunk string) (Embedding, error) {
	out, err := v.EmbedChunks([]string{chunk})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedQuery embeds a single search query.
func (v *Voyage) EmbedQuery(query string) (Embedding, error) {
	if v.contextual {
		// Context models are served only by the contextualized endpoint. A
		// query is sent as a single-chunk document group tagged input_type
		// "query"; the response carries its one vector.
		groups, err := v.contextEmbed([][]string{{query}}, "query")
		if err != nil {
			return nil, err
		}
		return groups[0][0], nil
	}
	out, err := v.embed([]string{query}, "query")
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedChunks embeds a batch of document chunks. For context models, chunks are
// pre-supplied to the contextualized endpoint with auto-chunking disabled, each
// chunk as its own single-chunk document group so it is embedded in isolation
// (matching the code path's isolated-chunk contract) while sharing the same
// vector space as auto-chunked text. Requests are split to respect the
// endpoint's per-request token limit.
func (v *Voyage) EmbedChunks(chunks []string) ([]Embedding, error) {
	if !v.contextual {
		return v.embed(chunks, "document")
	}
	out := make([]Embedding, 0, len(chunks))
	for start := 0; start < len(chunks); {
		end, batchChars := start, 0
		for end < len(chunks) {
			c := len(chunks[end])
			if end > start && batchChars+c > voyageContextMaxBatchChars {
				break
			}
			batchChars += c
			end++
		}
		groups := make([][]string, end-start)
		for i, c := range chunks[start:end] {
			groups[i] = []string{c}
		}
		embedded, err := v.contextEmbed(groups, "document")
		if err != nil {
			return nil, err
		}
		for _, g := range embedded {
			out = append(out, g[0])
		}
		start = end
	}
	return out, nil
}

// EmbedDocument sends the whole document to Voyage's contextualized chunk
// endpoint with auto-chunking enabled and returns the model-chosen chunks in
// order, each paired with its contextualized vector.
func (v *Voyage) EmbedDocument(document string) ([]ContextualChunk, error) {
	parsed, err := v.postContext(voyageContextRequest{
		Model:              v.modelID,
		Inputs:             [][]string{{document}},
		InputType:          "document",
		OutputDimension:    v.dims,
		EnableAutoChunking: true,
	})
	if err != nil {
		return nil, err
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

// contextEmbed sends pre-supplied chunk groups to the contextualized endpoint
// with auto-chunking disabled and returns per-group, per-chunk vectors in
// order. Each inner slice is one document's chunks; single-chunk groups yield
// isolated embeddings. Callers must keep the batch within the endpoint's
// per-request token limit (see voyageContextMaxBatchChars).
func (v *Voyage) contextEmbed(groups [][]string, inputType string) ([][]Embedding, error) {
	parsed, err := v.postContext(voyageContextRequest{
		Model:           v.modelID,
		Inputs:          groups,
		InputType:       inputType,
		OutputDimension: v.dims,
	})
	if err != nil {
		return nil, err
	}
	if len(parsed.Data) != len(groups) {
		return nil, fmt.Errorf("expected %d document groups, got %d", len(groups), len(parsed.Data))
	}
	out := make([][]Embedding, len(parsed.Data))
	for gi, g := range parsed.Data {
		if len(g.Data) != len(groups[gi]) {
			return nil, fmt.Errorf("group %d: expected %d chunks, got %d", gi, len(groups[gi]), len(g.Data))
		}
		vecs := make([]Embedding, len(g.Data))
		for i, d := range g.Data {
			if len(d.Embedding) != v.dims {
				return nil, fmt.Errorf("group %d chunk %d: expected %d dims, got %d", gi, i, v.dims, len(d.Embedding))
			}
			vecs[i] = Embedding(d.Embedding)
		}
		out[gi] = vecs
	}
	return out, nil
}

// postContext issues one request to the /v1/contextualizedembeddings endpoint
// and returns the decoded response, mapping non-2xx statuses to errors.
func (v *Voyage) postContext(reqBody voyageContextRequest) (*voyageContextResponse, error) {
	body, err := json.Marshal(reqBody)
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
		var msg string
		switch {
		case parsed.Error != nil && parsed.Error.Message != "":
			msg = parsed.Error.Message
		case parsed.Detail != "":
			msg = parsed.Detail
		}
		// A 400 whose message mentions tokens is the per-request token cap; return
		// a typed error so the caller can retry with a smaller input rather than
		// treating it as a fatal request error.
		if resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "token") {
			return nil, &ContextTokenLimitError{Status: resp.StatusCode, Message: msg}
		}
		if msg != "" {
			return nil, fmt.Errorf("voyage contextualizedembeddings status %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("voyage contextualizedembeddings status %d", resp.StatusCode)
	}
	return &parsed, nil
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

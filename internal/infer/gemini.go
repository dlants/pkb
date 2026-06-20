package infer

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

// Gemini runs inference via Google's Generative Language API
// (`{baseURL}/v1beta/models/{model}:generateContent`). The API key is read from
// the env var named by apiKeyEnv (default GEMINI_API_KEY) and passed as a `key`
// query parameter.
type Gemini struct {
	client  *http.Client
	baseURL string
	apiKey  string
	modelID string
}

type geminiGenerateRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenerateResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// NewGemini builds a Gemini inference model. baseURL defaults to
// https://generativelanguage.googleapis.com; apiKeyEnv names the env var holding
// the key (default GEMINI_API_KEY). modelID is normalized to the `models/<id>`
// form the API expects.
func NewGemini(baseURL, apiKeyEnv, modelID string) (*Gemini, error) {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if apiKeyEnv == "" {
		apiKeyEnv = "GEMINI_API_KEY"
	}
	return &Gemini{
		client:  &http.Client{Timeout: 120 * time.Second},
		baseURL: baseURL,
		apiKey:  os.Getenv(apiKeyEnv),
		modelID: modelID,
	}, nil
}

// ModelName returns the model identifier.
func (g *Gemini) ModelName() string { return g.modelID }

func (g *Gemini) modelPath() string {
	if strings.HasPrefix(g.modelID, "models/") {
		return g.modelID
	}
	return "models/" + g.modelID
}

// Complete sends the prompt as a single user turn and returns the model's text.
func (g *Gemini) Complete(prompt string) (string, error) {
	body, err := json.Marshal(geminiGenerateRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
	})
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/v1beta/%s:generateContent", g.baseURL, g.modelPath())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		q := req.URL.Query()
		q.Set("key", g.apiKey)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini generate request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed geminiGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decoding gemini response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil {
			return "", fmt.Errorf("gemini generate status %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("gemini generate status %d", resp.StatusCode)
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini generate: no candidates returned")
	}
	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

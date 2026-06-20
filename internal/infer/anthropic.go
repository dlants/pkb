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

// Anthropic runs inference via Anthropic's native Messages API
// (`{baseURL}/v1/messages`). The API key is read from the env var named by
// apiKeyEnv (default ANTHROPIC_API_KEY) and sent in the x-api-key header.
type Anthropic struct {
	client  *http.Client
	baseURL string
	apiKey  string
	modelID string
}

type anthropicMessagesRequest struct {
	Model       string                 `json:"model"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature *float64               `json:"temperature,omitempty"`
	Messages    []anthropicMessagesMsg `json:"messages"`
}

type anthropicMessagesMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicMessagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// NewAnthropic builds an Anthropic Messages API inference model. baseURL
// defaults to https://api.anthropic.com; apiKeyEnv names the env var holding the
// key (default ANTHROPIC_API_KEY).
func NewAnthropic(baseURL, apiKeyEnv, modelID string) (*Anthropic, error) {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if apiKeyEnv == "" {
		apiKeyEnv = "ANTHROPIC_API_KEY"
	}
	return &Anthropic{
		client:  &http.Client{Timeout: 120 * time.Second},
		baseURL: baseURL,
		apiKey:  os.Getenv(apiKeyEnv),
		modelID: modelID,
	}, nil
}

// ModelName returns the model identifier.
func (a *Anthropic) ModelName() string { return a.modelID }

// Complete sends the prompt as a single user message and returns the assistant
// reply text.
func (a *Anthropic) Complete(prompt string) (string, error) {
	body, err := json.Marshal(anthropicMessagesRequest{
		Model:       a.modelID,
		MaxTokens:   augmentMaxTokens,
		Temperature: &augmentTemperature,
		Messages:    []anthropicMessagesMsg{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if a.apiKey != "" {
		req.Header.Set("x-api-key", a.apiKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic messages request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed anthropicMessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decoding anthropic response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil {
			return "", fmt.Errorf("anthropic messages status %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("anthropic messages status %d", resp.StatusCode)
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return "", fmt.Errorf("anthropic messages: no text content returned")
	}
	return text, nil
}

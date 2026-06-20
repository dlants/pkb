package infer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// OpenAICompatible runs inference via any server speaking the OpenAI
// `/v1/chat/completions` wire format: OpenAI cloud, Ollama, llama.cpp, vLLM,
// LM Studio, LocalAI. Providers are selected purely by base URL and API-key
// env var.
type OpenAICompatible struct {
	client  *http.Client
	baseURL string
	apiKey  string
	modelID string
}

type openAIChatRequest struct {
	Model    string              `json:"model"`
	Messages []openAIChatMessage `json:"messages"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message openAIChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// NewOpenAICompatible builds an OpenAI-compatible inference model. baseURL
// defaults to https://api.openai.com; apiKeyEnv names the env var holding the
// key (default OPENAI_API_KEY). A missing key is tolerated (empty Authorization)
// so local servers like Ollama work without credentials.
func NewOpenAICompatible(baseURL, apiKeyEnv, modelID string) (*OpenAICompatible, error) {
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
		client:  &http.Client{Timeout: 120 * time.Second},
		baseURL: baseURL,
		apiKey:  os.Getenv(apiKeyEnv),
		modelID: modelID,
	}, nil
}

// ModelName returns the model identifier.
func (o *OpenAICompatible) ModelName() string { return o.modelID }

// Complete sends the prompt as a single user message and returns the assistant
// reply text.
func (o *OpenAICompatible) Complete(prompt string) (string, error) {
	body, err := json.Marshal(openAIChatRequest{
		Model:    o.modelID,
		Messages: []openAIChatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, o.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var parsed openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decoding openai response (status %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if parsed.Error != nil {
			return "", fmt.Errorf("openai chat status %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("openai chat status %d", resp.StatusCode)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openai chat: no choices returned")
	}
	return parsed.Choices[0].Message.Content, nil
}

package infer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// BedrockClaude runs inference via an Anthropic Claude model on AWS Bedrock,
// invoked through the bedrock-runtime InvokeModel API using the Anthropic
// Messages wire format.
type BedrockClaude struct {
	client  *bedrockruntime.Client
	modelID string
}

type anthropicRequest struct {
	AnthropicVersion string             `json:"anthropic_version"`
	MaxTokens        int                `json:"max_tokens"`
	Messages         []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// NewBedrockClaude builds a Bedrock-backed Claude inference model. The modelID
// is the Bedrock model/inference-profile id (e.g.
// "us.anthropic.claude-3-5-haiku-20241022-v1:0").
func NewBedrockClaude(ctx context.Context, region, profile, modelID string) (*BedrockClaude, error) {
	if region == "" {
		region = "us-east-1"
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}
	// Resolve credentials eagerly so a missing/expired session fails fast with
	// an actionable message instead of an opaque InvokeModel error later.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("no usable AWS credentials: %w\nhint: run `aws sso login` to refresh your session", err)
	}
	return &BedrockClaude{
		client:  bedrockruntime.NewFromConfig(cfg),
		modelID: modelID,
	}, nil
}

// ModelName returns the model identifier.
func (b *BedrockClaude) ModelName() string { return b.modelID }

// Complete sends the prompt as a single user message and returns the assistant
// reply text.
func (b *BedrockClaude) Complete(prompt string) (string, error) {
	body, err := json.Marshal(anthropicRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        1024,
		Messages:         []anthropicMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}
	resp, err := b.client.InvokeModel(context.Background(), &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(b.modelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return "", fmt.Errorf("bedrock InvokeModel: %w", err)
	}
	var parsed anthropicResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return "", fmt.Errorf("decoding bedrock response: %w", err)
	}
	var text string
	for _, c := range parsed.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return "", fmt.Errorf("bedrock claude: no text content returned")
	}
	return text, nil
}

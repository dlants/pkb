package embed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// BedrockCohere embeds text via a Cohere embedding model on AWS Bedrock,
// invoked through the bedrock-runtime InvokeModel API.
type BedrockCohere struct {
	client  *bedrockruntime.Client
	modelID string
	name    string
	dims    int
}

type cohereRequest struct {
	Texts          []string `json:"texts"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
}

type cohereResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// NewBedrockCohere builds a Bedrock-backed Cohere embedding model. The modelID
// is the Bedrock model/inference-profile id (e.g. "us.cohere.embed-v4:0").
func NewBedrockCohere(ctx context.Context, region, modelID string, dims int) (*BedrockCohere, error) {
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}
	return &BedrockCohere{
		client:  bedrockruntime.NewFromConfig(cfg),
		modelID: modelID,
		name:    modelID,
		dims:    dims,
	}, nil
}

// ModelName returns the model identifier used to key vec tables.
func (b *BedrockCohere) ModelName() string { return b.name }

// Dimensions returns the embedding dimensionality.
func (b *BedrockCohere) Dimensions() int { return b.dims }

// EmbedChunk embeds a single document chunk.
func (b *BedrockCohere) EmbedChunk(chunk string) (Embedding, error) {
	out, err := b.embed([]string{chunk}, "search_document")
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedQuery embeds a single search query.
func (b *BedrockCohere) EmbedQuery(query string) (Embedding, error) {
	out, err := b.embed([]string{query}, "search_query")
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// EmbedChunks embeds a batch of document chunks.
func (b *BedrockCohere) EmbedChunks(chunks []string) ([]Embedding, error) {
	return b.embed(chunks, "search_document")
}

func (b *BedrockCohere) embed(texts []string, inputType string) ([]Embedding, error) {
	body, err := json.Marshal(cohereRequest{
		Texts:          texts,
		InputType:      inputType,
		EmbeddingTypes: []string{"float"},
	})
	if err != nil {
		return nil, err
	}
	resp, err := b.client.InvokeModel(context.Background(), &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(b.modelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("*/*"),
		Body:        body,
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock InvokeModel: %w", err)
	}
	var parsed cohereResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, fmt.Errorf("decoding bedrock response: %w", err)
	}
	if len(parsed.Embeddings.Float) == 0 {
		return nil, fmt.Errorf("no float embeddings returned")
	}
	out := make([]Embedding, len(parsed.Embeddings.Float))
	for i, v := range parsed.Embeddings.Float {
		out[i] = Embedding(v)
	}
	return out, nil
}

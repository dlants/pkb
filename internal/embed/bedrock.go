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
	// Truncate tells Cohere how to handle inputs over the model's max length
	// ("END" drops the tail) instead of failing the whole batch.
	Truncate string `json:"truncate"`
	// OutputDimension requests a Matryoshka-truncated embedding. embed-v4
	// supports 256/512/1024/1536; omitted (0) uses the model default.
	OutputDimension int `json:"output_dimension,omitempty"`
}

type cohereResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

// NewBedrockCohere builds a Bedrock-backed Cohere embedding model. The modelID
// is the Bedrock model/inference-profile id (e.g. "us.cohere.embed-v4:0").
func NewBedrockCohere(ctx context.Context, region, profile, modelID string, dims int) (*BedrockCohere, error) {
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
	// Credentials are resolved lazily by the SDK; retrieve them eagerly so a
	// missing/expired session fails fast with an actionable message instead of
	// surfacing as an opaque InvokeModel error later.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("no usable AWS credentials: %w\nhint: run `aws sso login` to refresh your session", err)
	}
	return &BedrockCohere{
		client:  bedrockruntime.NewFromConfig(cfg),
		modelID: modelID,
		name:    fmt.Sprintf("%s@%d", modelID, dims),
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

// maxCohereBatchTexts is the per-request cap Bedrock Cohere embed-v4 enforces on
// the number of input texts. A single InvokeModel with more than this many texts
// fails the whole request with an opaque "Invalid parameter combination"
// ValidationException, so embed splits larger batches into ≤96-text requests.
const maxCohereBatchTexts = 96

// embed embeds texts in order, splitting into sub-requests of at most
// maxCohereBatchTexts and concatenating the results so the returned embeddings
// stay aligned 1:1 with the inputs.
func (b *BedrockCohere) embed(texts []string, inputType string) ([]Embedding, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([]Embedding, 0, len(texts))
	for _, batch := range splitBatches(texts, maxCohereBatchTexts) {
		sub, err := b.embedBatch(batch, inputType)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// splitBatches partitions texts into contiguous, order-preserving sub-slices of
// at most size elements each.
func splitBatches(texts []string, size int) [][]string {
	if size < 1 {
		size = 1
	}
	var batches [][]string
	for start := 0; start < len(texts); start += size {
		end := start + size
		if end > len(texts) {
			end = len(texts)
		}
		batches = append(batches, texts[start:end])
	}
	return batches
}

func (b *BedrockCohere) embedBatch(texts []string, inputType string) ([]Embedding, error) {
	body, err := json.Marshal(cohereRequest{
		Texts:          texts,
		InputType:      inputType,
		EmbeddingTypes:  []string{"float"},
		Truncate:        "END",
		OutputDimension: b.dims,
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

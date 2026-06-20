// Package config loads the repo-root PKB configuration (pkb.toml or
// .pkb/config.toml), which selects an embedding model and optional extension
// routing overrides.
package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ModelConfig selects an embedding model: the Bedrock provider/model id plus
// the embedding dimensionality. (Model construction lives in the embed package;
// config only records the selection.)
type ModelConfig struct {
	Provider   string `toml:"provider"`
	Model      string `toml:"model"`
	Dimensions int    `toml:"dimensions"`
	// Region is the AWS region for the Bedrock provider (default us-east-1).
		Region string `toml:"awsregion"`
	// Profile is the shared-config AWS profile for the Bedrock provider; empty
	// uses the default credential chain.
		Profile string `toml:"awsprofile"`
	// BaseURL is the API base URL for OpenAI-compatible providers (e.g.
	// https://api.openai.com or http://localhost:11434 for Ollama). Ignored by
	// non-HTTP providers like Bedrock.
	BaseURL string `toml:"baseurl"`
	// APIKeyEnv names the environment variable holding the API key for HTTP
	// providers (e.g. OPENAI_API_KEY). Ignored by Bedrock.
	APIKeyEnv string `toml:"apikeyenv"`
}

// Config is the parsed repo-root configuration.
type Config struct {
	// Embedding selects the model used to embed all files (code and text).
	Embedding ModelConfig `toml:"embedding"`
	// Inference selects the model used to augment text/markdown chunks before
	// embedding (the contextual-retrieval pattern). Setting its Provider to
	// "none" disables LLM augmentation, falling back to the deterministic
	// heading-prefix path.
	Inference ModelConfig `toml:"inference"`
	// ExtOverrides maps a file extension (including the leading dot) to a
	// file-type name ("code" or "text").
	ExtOverrides map[string]string `toml:"extOverrides"`
	// Exclude lists paths to skip during indexing, matched by basename or path
	// prefix (see internal/index.Ignore).
	Exclude []string `toml:"exclude"`
	// MaxParallelism bounds how many inference (augmentation) calls run
	// concurrently during indexing. Augmentation against a remote model is the
	// slowest part of a run, so issuing several requests at once speeds it up.
	// Defaults to 4; values below 1 are treated as 1.
	MaxParallelism int `toml:"maxparallelism"`
}

// Default returns the built-in configuration used when no config file exists.
func Default() Config {
	return Config{
		Embedding: ModelConfig{
			Provider:   "voyage",
			Model:      "voyage-code-3",
			Dimensions: 256,
		},
		Inference: ModelConfig{
			Provider: "anthropic",
			Model:    "claude-haiku-4-5",
		},
		MaxParallelism: 4,
	}
}

// configPaths returns the candidate config file locations, in priority order.
func configPaths(repoRoot string) []string {
	return []string{
		filepath.Join(repoRoot, "pkb.toml"),
		filepath.Join(repoRoot, ".pkb", "config.toml"),
	}
}

// Load reads the repo-root config, falling back to Default for any unset field.
// A missing config file is not an error; defaults are returned.
func Load(repoRoot string) (Config, error) {
	cfg := Default()
	for _, p := range configPaths(repoRoot) {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cfg, err
		}
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return cfg, err
		}
		break
	}
	return cfg, nil
}

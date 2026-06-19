// Package config loads the repo-root PKB configuration (.pkb.json or
// .pkb/config.json), which selects a code embedding model and a text embedding
// model, an optional target ref, and optional extension routing overrides.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// ModelConfig selects an embedding model: the Bedrock provider/model id plus
// the embedding dimensionality. (Model construction lives in the embed package;
// config only records the selection.)
type ModelConfig struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
}

// Config is the parsed repo-root configuration.
type Config struct {
	// CodeEmbedding embeds source-code files.
	CodeEmbedding ModelConfig `json:"codeEmbedding"`
	// TextEmbedding embeds prose/markdown files.
	TextEmbedding ModelConfig `json:"textEmbedding"`
	// Ref is the target git ref to index (default "HEAD").
	Ref string `json:"ref,omitempty"`
	// ExtOverrides maps a file extension (including the leading dot) to a
	// file-type name ("code" or "text").
	ExtOverrides map[string]string `json:"extOverrides,omitempty"`
}

// Default returns the built-in configuration used when no config file exists.
func Default() Config {
	return Config{
		CodeEmbedding: ModelConfig{
			Provider:   "bedrock",
			Model:      "cohere.embed-english-v3",
			Dimensions: 1024,
		},
		TextEmbedding: ModelConfig{
			Provider:   "bedrock",
			Model:      "cohere.embed-english-v3",
			Dimensions: 1024,
		},
		Ref: "HEAD",
	}
}

// configPaths returns the candidate config file locations, in priority order.
func configPaths(repoRoot string) []string {
	return []string{
		filepath.Join(repoRoot, ".pkb.json"),
		filepath.Join(repoRoot, ".pkb", "config.json"),
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
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, err
		}
		break
	}
	if cfg.Ref == "" {
		cfg.Ref = "HEAD"
	}
	return cfg, nil
}

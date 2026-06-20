// Package config loads the repo-root PKB configuration (pkb.toml or
// .pkb/config.toml), which selects an embedding model, an optional target ref,
// and optional extension routing overrides.
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
	Region string `toml:"region"`
	// Profile is the shared-config AWS profile for the Bedrock provider; empty
	// uses the default credential chain.
	Profile string `toml:"profile"`
}

// Config is the parsed repo-root configuration.
type Config struct {
	// Embedding selects the model used to embed all files (code and text).
	Embedding ModelConfig `toml:"embedding"`
	// Ref is the target git ref to index (default "HEAD").
	Ref string `toml:"ref"`
	// ExtOverrides maps a file extension (including the leading dot) to a
	// file-type name ("code" or "text").
	ExtOverrides map[string]string `toml:"extOverrides"`
}

// Default returns the built-in configuration used when no config file exists.
func Default() Config {
	return Config{
		Embedding: ModelConfig{
			Provider:   "bedrock",
			Model:      "us.cohere.embed-v4:0",
			Dimensions: 256,
		},
		Ref: "HEAD",
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
	if cfg.Ref == "" {
		cfg.Ref = "HEAD"
	}
	return cfg, nil
}

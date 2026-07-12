// Package config loads the repo-root PKB configuration (pkb.toml or
// .pkb/config.toml), which selects an embedding model and optional extension
// routing overrides.
package config

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// ModelConfig selects an embedding model: the provider/model id plus the
// embedding dimensionality. (Model construction lives in the embed package;
// config only records the selection.)
type ModelConfig struct {
	Provider   string `toml:"provider"`
	Model      string `toml:"model"`
	Dimensions int    `toml:"dimensions"`
	// BaseURL is the API base URL for the Voyage endpoint (defaults to Voyage's
	// public API when empty).
	BaseURL string `toml:"baseurl"`
	// APIKeyEnv names the environment variable holding the API key (defaults to
	// VOYAGE_API_KEY).
	APIKeyEnv string `toml:"apikeyenv"`
}

// Config is the parsed repo-root configuration.
type Config struct {
	// Embedding selects the model used to embed all files (code and text).
	Embedding ModelConfig `toml:"embedding"`
	// ExtOverrides maps a file extension (including the leading dot) to a
	// file-type name ("code" or "text").
	ExtOverrides map[string]string `toml:"extOverrides"`
	// Exclude lists paths to skip during indexing, matched by basename or path
	// prefix (see internal/index.Ignore).
	Exclude []string `toml:"exclude"`
	// MaxReindexCost caps the projected dollar cost of a single reindex
	// run (it is a per-run cap, not a cumulative spend limit across runs).
	// Before any paid embedding work, Reindex estimates the run's cost
	// and aborts when it exceeds this budget, so an unexpectedly large/expensive
	// change set must be reindexed locally instead. Defaults to $5; a
	// non-positive value disables the gate.
	MaxReindexCost float64 `toml:"maxReindexCost"`
}

// Default returns the built-in configuration used when no config file exists.
func Default() Config {
	return Config{
		Embedding: ModelConfig{
			Provider:   "voyage",
			Model:      "voyage-context-3",
			Dimensions: 256,
		},
		MaxReindexCost: 5.0,
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

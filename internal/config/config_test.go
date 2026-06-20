package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMissingConfigReturnsDefaults(t *testing.T) {
	cfg, err := Load(t.TempDir())
	require.NoError(t, err)
	require.Equal(t, Default(), cfg)
}

func TestLoadMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkb.toml"), []byte(`
[embedding]
provider = "bedrock"
model = "my-model"
dimensions = 512
`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "my-model", cfg.Embedding.Model)
	require.Equal(t, 512, cfg.Embedding.Dimensions)
}

func TestLoadInferenceAndHTTPFields(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkb.toml"), []byte(`
[embedding]
provider = "openai"
model = "text-embedding-3-small"
dimensions = 1536
baseurl = "https://api.openai.com"
apikeyenv = "OPENAI_API_KEY"

[inference]
provider = "openai"
model = "gpt-4o-mini"
baseurl = "http://localhost:11434"
apikeyenv = "OLLAMA_API_KEY"
`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "https://api.openai.com", cfg.Embedding.BaseURL)
	require.Equal(t, "OPENAI_API_KEY", cfg.Embedding.APIKeyEnv)
	require.Equal(t, "openai", cfg.Inference.Provider)
	require.Equal(t, "gpt-4o-mini", cfg.Inference.Model)
	require.Equal(t, "http://localhost:11434", cfg.Inference.BaseURL)
	require.Equal(t, "OLLAMA_API_KEY", cfg.Inference.APIKeyEnv)
}

func TestDefaultInferenceWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkb.toml"),
		[]byte("[embedding]\nmodel = \"m\"\n"), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, Default().Inference, cfg.Inference)
	require.Equal(t, "anthropic", cfg.Inference.Provider)
}

func TestNestedConfigPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".pkb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pkb", "config.toml"),
		[]byte("[embedding]\nmodel = \"txt\"\n"), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "txt", cfg.Embedding.Model)
}

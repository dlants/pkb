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
provider = "voyage"
model = "my-model"
dimensions = 512
`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "my-model", cfg.Embedding.Model)
	require.Equal(t, 512, cfg.Embedding.Dimensions)
}

func TestLoadHTTPFields(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkb.toml"), []byte(`
[embedding]
provider = "voyage"
model = "voyage-context-3"
dimensions = 256
baseurl = "https://api.voyageai.com"
apikeyenv = "VOYAGE_API_KEY"
`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "https://api.voyageai.com", cfg.Embedding.BaseURL)
	require.Equal(t, "VOYAGE_API_KEY", cfg.Embedding.APIKeyEnv)
}

func TestStrayInferenceKeysAreTolerated(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkb.toml"), []byte(`
[embedding]
provider = "voyage"
model = "voyage-context-3"
dimensions = 256
contextualizeText = true

[inference]
provider = "openai"
model = "gpt-4o-mini"
`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "voyage-context-3", cfg.Embedding.Model)
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

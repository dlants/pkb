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

func TestNestedConfigPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".pkb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pkb", "config.toml"),
		[]byte("[embedding]\nmodel = \"txt\"\n"), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "txt", cfg.Embedding.Model)
}

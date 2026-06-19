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
	require.Equal(t, "HEAD", cfg.Ref)
}

func TestLoadMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pkb.json"), []byte(`{
		"codeEmbedding": {"provider": "bedrock", "model": "code-model", "dimensions": 512},
		"ref": "main"
	}`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "code-model", cfg.CodeEmbedding.Model)
	require.Equal(t, 512, cfg.CodeEmbedding.Dimensions)
	require.Equal(t, "main", cfg.Ref)
	// Untouched field keeps its default.
	require.Equal(t, Default().TextEmbedding, cfg.TextEmbedding)
}

func TestNestedConfigPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".pkb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".pkb", "config.json"),
		[]byte(`{"textEmbedding": {"model": "txt"}}`), 0o644))

	cfg, err := Load(dir)
	require.NoError(t, err)
	require.Equal(t, "txt", cfg.TextEmbedding.Model)
}

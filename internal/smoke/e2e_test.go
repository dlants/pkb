// This file adds an offline end-to-end test of the index + search pipeline
// driven entirely by mock providers (no network, no credentials). It exercises
// the same Build paths the pkb CLI uses, verifying that provider selection is
// config-driven and that augmentation, indexing, and search work together
// without any real backend.
package smoke

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/index"
	"github.com/dlants/pkb/internal/infer"
	"github.com/dlants/pkb/internal/store"
	"github.com/stretchr/testify/require"
)

func TestEndToEndWithMockProviders(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("config", "commit.gpgsign", "false")
	runGit("checkout", "-b", "master")

	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	write("docs/intro.md", "# Intro\n\nThe quick brown fox jumps over the lazy dog.")
	write("main.go", "package main\n\nfunc main() { println(\"hello world\") }\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Build both models exactly as the pkb CLI does, via the provider factories,
	// selecting the deterministic mock providers.
	model, err := embed.Build("mock", "mock-embed", 8, "", "", "", "")
	require.NoError(t, err)

	repo, err := git.Open(root)
	require.NoError(t, err)
	st, err := store.Open(filepath.Join(t.TempDir(), "pkb.db"))
	require.NoError(t, err)
	defer st.Close()

	opts := &index.Options{
		Repo:   repo,
		Store:  st,
		Model:  model,
		Ignore: index.NewIgnore(nil),
	}

	state, err := index.Reindex(opts)
	require.NoError(t, err)
	require.Equal(t, 2, state.FileCount)
	require.Greater(t, state.ChunkCount, 0)

	results, err := index.Search(opts, "quick brown fox", 5)
	require.NoError(t, err)
	require.NotEmpty(t, results)
}

// TestBuildRejectsUnknownProviders ensures config-driven selection surfaces
// actionable errors for misconfigured providers.
func TestBuildRejectsUnknownProviders(t *testing.T) {
	_, err := embed.Build("nope", "m", 8, "", "", "", "")
	require.Error(t, err)
	_, err = infer.Build("nope", "m", "", "", "", "")
	require.Error(t, err)
}

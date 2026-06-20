package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was
// written.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = orig
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, e := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if e != nil {
			break
		}
	}
	return string(buf), runErr
}

// setupRepo creates a temp git repo with a mock-model config and some files,
// then chdirs into it for the duration of the test.
func setupRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitCmd(t, root, "init")
	gitCmd(t, root, "config", "user.email", "test@example.com")
	gitCmd(t, root, "config", "user.name", "Test")
	gitCmd(t, root, "config", "commit.gpgsign", "false")
	gitCmd(t, root, "checkout", "-b", "master")

	write := func(rel, content string) {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	write("pkb.toml", `
[embedding]
provider = "mock"
model = "mock-embed"
dimensions = 16

[inference]
provider = "mock"
model = "mock-infer"
`)
	write("README.md", "# Title\n\nThis project handles authentication and authorization.\n")
	write("main.go", "package main\n\nfunc Authenticate(user string) bool {\n\treturn user != \"\"\n}\n")

	gitCmd(t, root, "add", "-A")
	gitCmd(t, root, "commit", "-m", "init")

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(root))
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	return root
}

func TestReindexSearchStats(t *testing.T) {
	setupRepo(t)

	out, err := captureStdout(t, func() error { return runReindex(nil) })
	require.NoError(t, err)
	require.Contains(t, out, "files")

	// Re-running with no changes is a no-op that still succeeds.
	_, err = captureStdout(t, func() error { return runReindex(nil) })
	require.NoError(t, err)

	out, err = captureStdout(t, func() error { return runSearch([]string{"authentication"}) })
	require.NoError(t, err)
	require.NotEqual(t, "No results found.\n", out)
	require.Contains(t, out, "Result 1")

	out, err = captureStdout(t, func() error { return runStats(nil) })
	require.NoError(t, err)
	require.Contains(t, out, "commit:")
	require.Contains(t, out, "files:")
}

func TestSearchMissingQuery(t *testing.T) {
	setupRepo(t)
	err := runSearch(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing query")
}

func TestStatsNoIndex(t *testing.T) {
	setupRepo(t)
	out, err := captureStdout(t, func() error { return runStats(nil) })
	require.NoError(t, err)
	require.True(t, strings.Contains(out, "no index yet"))
}

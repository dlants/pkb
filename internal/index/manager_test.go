package index

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/store"
	"github.com/stretchr/testify/require"
)

type harness struct {
	t    *testing.T
	root string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{t: t, root: root}
	h.git("init")
	h.git("config", "user.email", "test@example.com")
	h.git("config", "user.name", "Test")
	h.git("config", "commit.gpgsign", "false")
	h.git("checkout", "-b", "master")
	return h
}

func (h *harness) git(args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = h.root
	out, err := cmd.CombinedOutput()
	require.NoErrorf(h.t, err, "git %v: %s", args, out)
	return string(out)
}

func (h *harness) write(rel, content string) {
	h.t.Helper()
	full := filepath.Join(h.root, rel)
	require.NoError(h.t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(h.t, os.WriteFile(full, []byte(content), 0o644))
}

func (h *harness) remove(rel string) {
	h.t.Helper()
	require.NoError(h.t, os.Remove(filepath.Join(h.root, rel)))
}

func (h *harness) commit(msg string) string {
	h.t.Helper()
	h.git("add", "-A")
	h.git("commit", "-m", msg, "--allow-empty")
	return h.headSha()
}

func (h *harness) headSha() string {
	return chomp(h.git("rev-parse", "HEAD"))
}

func chomp(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func (h *harness) opts(t *testing.T, model embed.EmbeddingModel) (*Options, *store.Store) {
	t.Helper()
	repo, err := git.Open(h.root)
	require.NoError(t, err)
	st, err := store.Open(filepath.Join(t.TempDir(), "pkb.db"))
	require.NoError(t, err)
	ign, err := LoadIgnore(h.root)
	require.NoError(t, err)
	return &Options{Repo: repo, Store: st, Model: model, Ignore: ign}, st
}

func TestColdStartIndexesEverything(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content")
	h.write("b.md", "# B\n\nbeta content")
	sha := h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, sha, state.Commit)
	require.Equal(t, 2, state.FileCount)
	require.Greater(t, model.ChunkCount(), 0)
}

func TestNoOpWhenNothingChanged(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	calls := model.ChunkCalls()
	require.Greater(t, calls, 0)

	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, calls, model.ChunkCalls(), "second reindex should embed nothing")
}

func TestIncrementalAddModifyDelete(t *testing.T) {
	h := newHarness(t)
	h.write("keep.md", "# Keep\n\nkeep content")
	h.write("mod.md", "# Mod\n\noriginal")
	h.write("del.md", "# Del\n\ndelete me")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	h.write("add.md", "# Add\n\nbrand new")
	h.write("mod.md", "# Mod\n\nchanged content here")
	h.remove("del.md")
	sha := h.commit("changes")

	callsBefore := model.ChunkCalls()
	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, sha, state.Commit)
	require.Equal(t, 3, state.FileCount) // keep, mod, add
	require.Greater(t, model.ChunkCalls(), callsBefore)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.Contains(t, files, "add.md")
	require.Contains(t, files, "mod.md")
	require.Contains(t, files, "keep.md")
	require.NotContains(t, files, "del.md")
}

func TestRenameHandled(t *testing.T) {
	h := newHarness(t)
	h.write("old.md", "# Old\n\nrename content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o)
	require.NoError(t, err)

	h.git("mv", "old.md", "new.md")
	h.commit("rename")

	_, err = Reindex(o)
	require.NoError(t, err)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.NotContains(t, files, "old.md")
	require.Contains(t, files, "new.md")
}

func TestPartialRunMarkerSafety(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content")
	h.commit("init")

	failing := &embed.FailingModel{MockModel: embed.NewMockModel("mock", 3), FailAfter: 0}
	o, st := h.opts(t, failing)
	defer st.Close()

	_, err := Reindex(o)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(h.root, ".pkb", "state.json"))
	require.True(t, os.IsNotExist(statErr), "marker must not be written on failed run")

	// Fix and retry.
	o.Model = embed.NewMockModel("mock", 3)
	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, h.headSha(), state.Commit)
	require.Equal(t, 1, state.FileCount)
}

func TestDivergenceViaMergeBase(t *testing.T) {
	h := newHarness(t)
	h.write("base.md", "# Base\n\nbase content")
	h.commit("base")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	// index on a side branch commit S
	h.git("checkout", "-b", "side")
	h.write("side.md", "# Side\n\nside content")
	sShaCommit := h.commit("side change")
	_ = sShaCommit
	stateS, err := Reindex(o)
	require.NoError(t, err)

	// rewrite history: go back to base and make a different commit C
	h.git("checkout", "master")
	h.write("main.md", "# Main\n\nmain content")
	cSha := h.commit("main change")

	// Now reindex against master (C). S is not an ancestor of C.
	o.Ref = "master"
	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, cSha, state.Commit)
	require.NotEqual(t, stateS.Commit, state.Commit)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.Contains(t, files, "base.md")
	require.Contains(t, files, "main.md")
	require.NotContains(t, files, "side.md", "abandoned-branch file should be removed")
}

func TestTotalRecoveryWhenCommitGone(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o)
	require.NoError(t, err)

	// Corrupt the marker to a non-existent commit sha.
	require.NoError(t, writeState(h.root, State{Commit: "0000000000000000000000000000000000000000"}))

	callsBefore := model.ChunkCalls()
	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, h.headSha(), state.Commit)
	require.Equal(t, 1, state.FileCount)
	// content unchanged -> blob-sha short-circuit means no re-embed
	require.Equal(t, callsBefore, model.ChunkCalls())
}

func TestPkbignoreExcludesPath(t *testing.T) {
	h := newHarness(t)
	h.write("keep.md", "# Keep\n\nkeep content")
	h.write("private/secret.md", "# Secret\n\nsecret content")
	h.write(".pkbignore", "private\n")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 1, state.FileCount)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.Contains(t, files, "keep.md")
	require.NotContains(t, files, "private/secret.md")
}

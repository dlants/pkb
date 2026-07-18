package index

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/mirror"
	"github.com/dlants/pkb/internal/paths"
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
	return &Options{Repo: repo, Store: st, Model: model, Ignore: NewIgnore(nil)}, st
}

func TestHealthcheckHealthyAndStale(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content")
	h.write("b.md", "# B\n\nbeta content")
	h.commit("init")
	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	rep, err := Healthcheck(o)
	require.NoError(t, err)
	require.Empty(t, rep.Issues, "expected healthy index, got: %v", rep.Issues)
	require.Equal(t, 2, rep.ExpectedFiles)
	require.Equal(t, 2, rep.IndexedFiles)

	// Advance HEAD without reindexing: the index and state are now stale.
	h.write("a.md", "# A\n\nalpha content changed")
	h.write("c.md", "# C\n\ngamma content")
	h.commit("changes")

	rep, err = Healthcheck(o)
	require.NoError(t, err)
	require.NotEmpty(t, rep.Issues)
	msgs := strings.Join(issueStrings(rep), "\n")
	require.Contains(t, msgs, "c.md: expected file is missing from the index")
	require.Contains(t, msgs, "a.md: stale blob")
}

func issueStrings(rep HealthReport) []string {
	out := make([]string, len(rep.Issues))
	for i, iss := range rep.Issues {
		if iss.Path == "" {
			out[i] = iss.Msg
		} else {
			out[i] = iss.Path + ": " + iss.Msg
		}
	}
	return out
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
	require.Greater(t, model.DocumentCalls(), 0)
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
	calls := model.DocumentCalls()
	require.Greater(t, calls, 0)

	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, calls, model.DocumentCalls(), "second reindex should embed nothing")
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

	callsBefore := model.DocumentCalls()
	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, sha, state.Commit)
	require.Equal(t, 3, state.FileCount) // keep, mod, add
	require.Greater(t, model.DocumentCalls(), callsBefore)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.Contains(t, files, "add.md")
	require.Contains(t, files, "mod.md")
	require.Contains(t, files, "keep.md")
	require.NotContains(t, files, "del.md")
}

func TestStagedReindexIndexesUncommittedThenNoOp(t *testing.T) {
	h := newHarness(t)
	h.write("committed.md", "# Committed\n\nalready committed content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	// Stage a new file but do not commit it. A staged reindex indexes the
	// write-tree snapshot, so the staged-but-uncommitted file is picked up.
	h.write("staged.md", "# Staged\n\nnot yet committed")
	h.git("add", "staged.md")

	o.Staged = true
	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 2, state.FileCount)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.Contains(t, files, "staged.md")

	// Committing the staged content yields identical blob shas, so a normal
	// post-commit reindex embeds nothing.
	h.commit("add staged")
	o.Staged = false
	callsBefore := model.DocumentCalls()
	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, callsBefore, model.DocumentCalls(), "post-commit reindex should be a no-op")
}

func TestTextFileUnchangedBlobReusesAll(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph\n\n## Sub\n\nnested paragraph")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	calls := model.DocumentCalls()

	// No change at all: the whole file is reused via blob_sha, nothing re-embeds.
	h.commit("empty")
	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, calls, model.DocumentCalls(), "unchanged text file should embed nothing")
}

// recordingModel wraps a mock embedding model and captures the exact chunk
// strings passed to EmbedChunks so tests can assert what was embedded.
type recordingModel struct {
	*embed.MockModel
	mu       sync.Mutex
	embedded []string
	failOnce bool
}

func (r *recordingModel) EmbedChunks(chunks []string) ([]embed.Embedding, error) {
	r.mu.Lock()
	if r.failOnce {
		r.failOnce = false
		r.mu.Unlock()
		return nil, fmt.Errorf("injected embed failure")
	}
	r.embedded = append(r.embedded, chunks...)
	r.mu.Unlock()
	return r.MockModel.EmbedChunks(chunks)
}

func (r *recordingModel) inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.embedded...)
}

func TestCrashMidFileReusesCommittedChunks(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n")
	h.commit("init")

	rec := &recordingModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, rec)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	firstRun := rec.inputs()
	require.Len(t, firstRun, 3, "fixture should embed three chunks on first run")

	// Edit only the second function, then crash the next embed call.
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 22\n}\n")
	h.commit("edit body")
	rec.failOnce = true
	_, err = Reindex(o)
	require.Error(t, err, "injected failure should abort the run")

	// Retry to completion. The unchanged first chunk is reused from the last
	// committed index and is never re-embedded; only the changed chunk is
	// embedded on the retry.
	before := rec.inputs()
	_, err = Reindex(o)
	require.NoError(t, err)
	retry := rec.inputs()[len(before):]
	require.Len(t, retry, 1, "retry re-embeds only the changed chunk")

	// The committed index holds exactly two chunks.
	stats, err := st.Stats("mock")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Files)
	require.Equal(t, 3, stats.Chunks)
}

func TestReusesUnchangedChunksCode(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	total := model.ChunkCount()
	require.Greater(t, total, 1, "fixture should produce multiple chunks")

	// Change only the body of one function.
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 22\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n")
	h.commit("edit beta")

	_, err = Reindex(o)
	require.NoError(t, err)
	delta := model.ChunkCount() - total
	require.Greater(t, delta, 0, "changed function must re-embed")
	require.Less(t, delta, total, "unchanged functions must be reused")
}

func bigClass(name string) string {
	var b strings.Builder
	b.WriteString("class " + name + " {\n")
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "  method%c() {\n", 'a'+i)
		for j := 0; j < 6; j++ {
			b.WriteString("    const x = 'padding padding padding padding padding';\n")
		}
		b.WriteString("    return 1;\n  }\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func TestReindexOnParentClassRenameCode(t *testing.T) {
	h := newHarness(t)
	h.write("f.ts", bigClass("Foo"))
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	total := model.ChunkCount()
	require.Greater(t, total, 2, "fixture should split into many method chunks")

	// Rename the enclosing class. Method-body text is identical, but the class
	// is a parent breadcrumb of every method chunk, so all must re-embed.
	h.write("f.ts", bigClass("Bar"))
	h.commit("rename class")

	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, total*2, model.ChunkCount(), "renaming parent class must re-embed all chunks")
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
	h.write("a.go", "package a\n\nfunc A() int {\n\treturn 1\n}\n")
	h.commit("init")

	failing := &embed.FailingModel{MockModel: embed.NewMockModel("mock", 3), FailAfter: 0}
	o, st := h.opts(t, failing)
	defer st.Close()

	_, err := Reindex(o)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(h.root, "pkb-state.toml"))
	require.True(t, os.IsNotExist(statErr), "marker must not be written on failed run")

	// Fix and retry.
	fixed := embed.NewMockModel("mock", 3)
	o.Model = fixed
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

func TestExcludeGlobExcludesPath(t *testing.T) {
	h := newHarness(t)
	h.write("keep.md", "# Keep\n\nkeep content")
	h.write("private/secret.md", "# Secret\n\nsecret content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	o.Ignore = NewIgnore([]string{"private"})
	defer st.Close()

	state, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 1, state.FileCount)

	files, err := st.IndexedFiles("mock")
	require.NoError(t, err)
	require.Contains(t, files, "keep.md")
	require.NotContains(t, files, "private/secret.md")
}

func TestModelChangeCleansUpOrphanTable(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Doc\n\ntext content here")
	h.commit("init")

	st, err := store.Open(filepath.Join(t.TempDir(), "pkb.db"))
	require.NoError(t, err)
	defer st.Close()
	repo, err := git.Open(h.root)
	require.NoError(t, err)
	m1 := embed.NewMockModel("mock-v1", 3)
	o := &Options{Repo: repo, Store: st, Model: m1, Ignore: NewIgnore(nil)}
	_, err = Reindex(o)
	require.NoError(t, err)

	files, err := st.IndexedFiles("mock-v1")
	require.NoError(t, err)
	require.Contains(t, files, "doc.md")

	// Switch the configured model; the old model's rows/tables should be purged.
	m2 := embed.NewMockModel("mock-v2", 3)
	o.Model = m2
	// Force a full re-run by clearing the marker.
	require.NoError(t, os.Remove(filepath.Join(h.root, "pkb-state.toml")))
	_, err = Reindex(o)
	require.NoError(t, err)

	old, err := st.IndexedFiles("mock-v1")
	require.NoError(t, err)
	require.Empty(t, old, "orphaned model rows should be cleaned up")

	newFiles, err := st.IndexedFiles("mock-v2")
	require.NoError(t, err)
	require.Contains(t, newFiles, "doc.md")
}

func TestModelChangeForcesFullReembedSameCommit(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Doc\n\ntext content here")
	h.commit("init")

	st, err := store.Open(filepath.Join(t.TempDir(), "pkb.db"))
	require.NoError(t, err)
	defer st.Close()
	repo, err := git.Open(h.root)
	require.NoError(t, err)
	m1 := embed.NewMockModel("mock-v1", 3)
	o := &Options{Repo: repo, Store: st, Model: m1, Ignore: NewIgnore(nil)}
	_, err = Reindex(o)
	require.NoError(t, err)

	// Swap the embedding model without changing the commit or removing the
	// state marker; the swap alone must force a full re-embed under the new
	// model.
	o.Model = embed.NewMockModel("mock-v2", 3)
	res, err := Reindex(o)
	require.NoError(t, err)
	require.Greater(t, res.FileCount, 0, "model swap should re-embed files")

	newFiles, err := st.IndexedFiles("mock-v2")
	require.NoError(t, err)
	require.Contains(t, newFiles, "doc.md")

	old, err := st.IndexedFiles("mock-v1")
	require.NoError(t, err)
	require.Empty(t, old, "orphaned model rows should be cleaned up")
}

func TestBudgetGateAbortsOverBudget(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph\n\n## Sub\n\nnested paragraph")
	h.commit("init")

	rec := &recordingModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, rec)
	o.MaxReindexCost = 1e-12 // any real work exceeds this
	defer st.Close()

	_, err := Reindex(o)
	require.Error(t, err, "estimate over budget must abort the run")
	require.Contains(t, err.Error(), "max reindex cost")

	// No paid work and no mutation occurred.
	require.Empty(t, rec.inputs(), "over-budget run must not embed")
	stats, err := st.Stats("mock")
	require.NoError(t, err)
	require.Equal(t, 0, stats.Files)
	require.Equal(t, 0, stats.Chunks)
}

func TestBudgetGateDoesNotChargeReuse(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph\n\n## Sub\n\nnested paragraph")
	h.commit("init")

	rec := &recordingModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, rec)
	defer st.Close()

	// First run, generous budget, indexes everything.
	o.MaxReindexCost = 1000
	_, err := Reindex(o)
	require.NoError(t, err)

	// Force a full revisit with a tiny budget. Every file is already complete
	// against the same blob, so the estimate is $0 and the run proceeds.
	o.MaxReindexCost = 1e-12
	require.NoError(t, os.Remove(filepath.Join(h.root, "pkb-state.toml")))
	_, err = Reindex(o)
	require.NoError(t, err, "reuse hits must not be charged against the budget")
}

func TestReindexWritesMirrorTree(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content")
	h.write("pkg/p.go", "package pkg\n\nfunc Alpha() int {\n\treturn 1\n}\n")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	// No monolithic DB is written at the repo root; the mirror tree is the
	// source of truth.
	_, statErr := os.Stat(filepath.Join(h.root, "pkb.db"))
	require.True(t, os.IsNotExist(statErr), "reindex must not write pkb.db at the repo root")

	tree := mirror.NewTree(paths.AbsPath(h.root))
	entries, err := tree.List()
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Contains(t, entries, paths.GitRootRelativePath("a.md"))
	require.Contains(t, entries, paths.GitRootRelativePath("pkg/p.go"))

	// Both sibling files exist for each artifact and decode to aligned chunks.
	for rel := range entries {
		require.FileExists(t, filepath.Join(h.root, mirror.IndexDir, string(rel)+mirror.MetaExt))
		require.FileExists(t, filepath.Join(h.root, mirror.IndexDir, string(rel)+mirror.VecExt))
		a, ok, err := tree.TryRead(rel)
		require.NoError(t, err)
		require.True(t, ok)
		require.NotEmpty(t, a.Chunks)
		for _, c := range a.Chunks {
			require.Len(t, c.Embedding, 3, "each chunk carries its vector")
		}
	}
}

func TestReindexReusesChunkKeepsArtifactBytes(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n")
	h.commit("init")

	rec := &recordingModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, rec)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	firstEmbeds := len(rec.inputs())

	// Capture the Alpha chunk's bytes: its meta+vec must be untouched after an
	// edit to a different chunk. We locate it by decoding the artifact.
	tree := mirror.NewTree(paths.AbsPath(h.root))
	before, ok, err := tree.TryRead("p.go")
	require.NoError(t, err)
	require.True(t, ok)
	alphaBefore := findChunk(t, o, before, "return 1")

	// Edit only Beta's body.
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 22\n}\n")
	h.commit("edit beta")

	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, firstEmbeds+1, len(rec.inputs()), "only the changed chunk re-embeds")

	after, ok, err := tree.TryRead("p.go")
	require.NoError(t, err)
	require.True(t, ok)
	alphaAfter := findChunk(t, o, after, "return 1")
	require.Equal(t, alphaBefore.Embedding, alphaAfter.Embedding, "reused chunk keeps its vector")
}

func findChunk(t *testing.T, o *Options, a mirror.Artifact, needle string) mirror.Chunk {
	t.Helper()
	content, err := o.Repo.CatBlob(a.BlobSha)
	require.NoError(t, err)
	for _, c := range a.Chunks {
		if strings.Contains(string(content[c.Start:c.End]), needle) {
			return c
		}
	}
	t.Fatalf("no chunk containing %q", needle)
	return mirror.Chunk{}
}

func TestReindexDeleteRemovesArtifact(t *testing.T) {
	h := newHarness(t)
	h.write("keep.md", "# Keep\n\nkeep content")
	h.write("del.md", "# Del\n\ndelete me")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	h.remove("del.md")
	h.commit("remove del")
	_, err = Reindex(o)
	require.NoError(t, err)

	tree := mirror.NewTree(paths.AbsPath(h.root))
	entries, err := tree.List()
	require.NoError(t, err)
	require.Contains(t, entries, paths.GitRootRelativePath("keep.md"))
	require.NotContains(t, entries, paths.GitRootRelativePath("del.md"))
	require.NoFileExists(t, filepath.Join(h.root, mirror.IndexDir, "del.md"+mirror.MetaExt))
	require.NoFileExists(t, filepath.Join(h.root, mirror.IndexDir, "del.md"+mirror.VecExt))
}

func TestCompactPreparedDropsZeroSignalChunks(t *testing.T) {
	// A chunk whose contextualized text is whitespace-only carries no
	// embeddable content; embedding it would send an empty string to the
	// provider. compactPrepared must drop the chunk row and its vector slot
	// together so the parallel slices passed to PutFile stay aligned.
	chunks := []chunk.ChunkInfo{
		{Text: "package main", HeadingContext: "a.go"},
		{Text: "   ", HeadingContext: ""}, // zero-signal: Contextualize -> "   "
		{Text: "func main() {}", HeadingContext: "a.go"},
	}
	reuse := []bool{false, false, false}
	reuseEmb := make([]embed.Embedding, 3)

	pf := compactPrepared("a.go", "//", chunks, reuse, reuseEmb)

	require.Len(t, pf.chunks, 2, "the whitespace-only chunk must be dropped")
	n := len(pf.chunks)
	require.Len(t, pf.contextualized, n)
	require.Len(t, pf.embeddings, n)
	require.Equal(t, []int{0, 1}, pf.embedIdx)
	require.Equal(t, n, pf.remaining)
	for _, c := range pf.contextualized {
		require.NotEmpty(t, strings.TrimSpace(c), "no empty text may survive compaction")
	}
}

// failOnEmptyModel records every embedding input and fails if it is ever asked
// to embed an empty or whitespace-only string, mirroring how Bedrock Cohere
// rejects empty inputs.
type failOnEmptyModel struct{ *embed.MockModel }

func (m *failOnEmptyModel) EmbedChunks(chunks []string) ([]embed.Embedding, error) {
	for i, c := range chunks {
		if strings.TrimSpace(c) == "" {
			return nil, fmt.Errorf("empty embed input at %d", i)
		}
	}
	return m.MockModel.EmbedChunks(chunks)
}

func TestReindexNeverEmbedsEmptyString(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content")
	h.write("b.go", "package b\n\nfunc B() {}\n")
	h.write("only-heading.md", "# Title\n")
	h.write("blanks.md", "# H\n\n\n\n")
	sha := h.commit("init")
	model := &failOnEmptyModel{MockModel: embed.NewMockModel("mock", 3)}
	o, _ := h.opts(t, model)
	st, err := Reindex(o)
	require.NoError(t, err, "reindex must never send an empty string to the embedder")
	require.Equal(t, sha, st.Commit)
}

// recordingContextModel wraps a mock model and records the documents passed to
// EmbedDocument so tests can assert windowing and that the whole-document path
// was taken.
type recordingContextModel struct {
	*embed.MockModel
	mu   sync.Mutex
	docs []string
}

func (r *recordingContextModel) EmbedDocument(document string) ([]embed.ContextualChunk, error) {
	r.mu.Lock()
	r.docs = append(r.docs, document)
	r.mu.Unlock()
	return r.MockModel.EmbedDocument(document)
}

func (r *recordingContextModel) documents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.docs...)
}

func TestContextualizeTextRoutesTextThroughEmbedDocument(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph\n\n## Sub\n\nnested paragraph")
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 8)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	require.Equal(t, 0, model.ChunkCalls(), "text file must not go through the isolated EmbedChunks path")
	require.Equal(t, 1, model.DocumentCalls(), "small text file is sent whole in one EmbedDocument call")

	res, err := Search(o, "paragraph", 10)
	require.NoError(t, err)
	require.NotEmpty(t, res)
	for _, r := range res {
		require.Equal(t, 0, r.StartLine, "auto-chunk text chunks are file-tagged with no line range")
		require.Equal(t, 0, r.EndLine)
	}
}

func TestContextualizeTextWindowsLargeFileAndDedups(t *testing.T) {
	h := newHarness(t)
	var b strings.Builder
	filler := strings.Repeat("x", 2000)
	for i := 0; b.Len() <= autoChunkMaxWindowByte+autoChunkOverlapByte; i++ {
		fmt.Fprintf(&b, "paragraph number %06d %s\n\n", i, filler)
	}
	h.write("big.md", b.String())
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 8)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	docs := model.documents()
	require.Greater(t, len(docs), 1, "an oversized file must be split into multiple windows")
	// Consecutive windows overlap by autoChunkOverlapByte.
	prev := docs[0]
	require.Equal(t, autoChunkMaxWindowByte, len(prev))
	require.Equal(t, prev[len(prev)-autoChunkOverlapByte:], docs[1][:autoChunkOverlapByte],
		"consecutive windows must share the configured overlap")

	res, err := Search(o, "paragraph", 4096)
	require.NoError(t, err)
	seen := map[string]struct{}{}
	for _, r := range res {
		_, dup := seen[r.Text]
		require.False(t, dup, "overlap-induced duplicate chunks must be deduped before writing")
		seen[r.Text] = struct{}{}
	}
}

func TestContextualizeTextLeavesCodeOnIsolatedPath(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n")
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 8)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	require.Equal(t, 0, model.DocumentCalls(), "code files must never use the auto-chunk path")
	require.Greater(t, model.ChunkCalls(), 0, "code files stay on the isolated EmbedChunks path")
}

func TestContextualizeTextReembedsOnEditSkipsUnchanged(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph")
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 8)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 1, model.DocumentCalls())

	// Unchanged reindex is a blob_sha skip.
	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 1, model.DocumentCalls(), "unchanged file must not be re-embedded")

	// Editing the file re-sends the whole document.
	h.write("doc.md", "# Top\n\nintro paragraph changed")
	h.commit("edit")
	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 2, model.DocumentCalls(), "edited file must be re-embedded whole")
}

func TestContextualizeTextSharesOneVecTable(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph about widgets")
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n")
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 8)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	files, err := st.IndexedFiles(model.ModelName())
	require.NoError(t, err)
	require.Contains(t, files, "doc.md")
	require.Contains(t, files, "p.go")
	require.Len(t, files, 2, "code and text share one vec table under one model id")

	codeHit, err := Search(o, "func Alpha", 10)
	require.NoError(t, err)
	require.NotEmpty(t, codeHit, "code hits come from the shared table")
	textHit, err := Search(o, "widgets paragraph", 10)
	require.NoError(t, err)
	require.NotEmpty(t, textHit, "text hits come from the shared table")
}

// coldStore opens a brand-new, empty SQLite cache for the same repo, modeling a
// fresh clone whose gitignored cache.db does not yet exist. Search must rebuild
// it from the committed mirror tree.
func coldStore(t *testing.T, o *Options) *Options {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cache.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return &Options{Repo: o.Repo, Store: st, Model: o.Model, Ignore: NewIgnore(nil)}
}

// TestSearchColdCacheMatchesWarm verifies the source-of-truth invariant: a
// missing cache never changes results. A search against an empty cache rebuilds
// it from the mirror tree and returns exactly what the warm cache returns.
func TestSearchColdCacheMatchesWarm(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nalpha content about apples")
	h.write("b.go", "package b\n\nfunc Beta() int { return 2 }\n")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o)
	require.NoError(t, err)

	warm, err := Search(o, "alpha content", 5)
	require.NoError(t, err)
	require.NotEmpty(t, warm)

	cold, err := Search(coldStore(t, o), "alpha content", 5)
	require.NoError(t, err)
	require.Equal(t, warm, cold, "cold cache must reproduce warm results exactly")
}

// TestSyncReadsIndexedBlobNotWorkingTree verifies sync reconstructs chunk text
// from the exact blob the artifact was indexed against, never the working tree:
// after the source file is edited on disk without reindexing, a cold-cache sync
// still yields the indexed (committed) content.
func TestSyncReadsIndexedBlobNotWorkingTree(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\nindexed distinctive alpha content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o)
	require.NoError(t, err)

	// Mutate the working tree only (no reindex): the mirror artifact still
	// references the committed blob, so sync must ignore this content.
	h.write("a.md", "# A\n\ntampered omega working-tree content")

	res, err := Search(coldStore(t, o), "distinctive alpha content", 10)
	require.NoError(t, err)
	var joined string
	for _, r := range res {
		joined += r.Text
	}
	require.Contains(t, joined, "indexed distinctive alpha content", "sync must reconstruct from the indexed blob")
	require.NotContains(t, joined, "tampered", "sync must not read the working tree")
}

// TestSearchEvictsRemovedArtifact verifies the read-path eviction invariant: an
// artifact gone from the mirror tree is dropped from the cache on the next
// search, even when the cache still holds its rows.
func TestSearchEvictsRemovedArtifact(t *testing.T) {
	h := newHarness(t)
	h.write("keep.md", "# Keep\n\nkeep content")
	h.write("del.md", "# Del\n\ndelete this distinctive content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o) // warm cache holds both files
	require.NoError(t, err)

	// Drop del.md's artifact directly from the tree (no reindex): the warm cache
	// still has its rows, so only the read-path sync can evict it.
	tree := mirror.NewTree(paths.AbsPath(h.root))
	require.NoError(t, tree.Delete("del.md"))

	res, err := Search(o, "delete this distinctive content", 10)
	require.NoError(t, err)
	for _, r := range res {
		require.NotEqual(t, "del.md", r.Path, "removed artifact must not appear in results")
	}
}

// TestSearchIncrementalReflectsEditedArtifact verifies incremental upsert on the
// read path: after an artifact's content changes on disk, a warm cache re-parses
// just that artifact and search reflects the new text.
func TestSearchIncrementalReflectsEditedArtifact(t *testing.T) {
	h := newHarness(t)
	h.write("a.md", "# A\n\noriginal alpha content")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o) // warm cache
	require.NoError(t, err)

	// Rewrite the source and refresh only the mirror tree (reindex writes the
	// tree and syncs, but the point is that a subsequent read-path sync is a
	// no-op cost-wise; here we assert the content is current).
	h.write("a.md", "# A\n\nrewritten distinctive omega content")
	h.commit("edit")
	_, err = Reindex(o)
	require.NoError(t, err)

	res, err := Search(coldStore(t, o), "omega content", 10)
	require.NoError(t, err)
	require.NotEmpty(t, res)
	var joined string
	for _, r := range res {
		joined += r.Text
	}
	require.Contains(t, joined, "omega", "search reflects the edited artifact")
}

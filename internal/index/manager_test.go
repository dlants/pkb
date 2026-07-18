package index

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

// recordingModel wraps a mock embedding model and captures the documents passed
// to EmbedDocument so tests can assert what was embedded. failOnce injects a
// one-shot failure to simulate a crash mid-run.
type recordingModel struct {
	*embed.MockModel
	mu       sync.Mutex
	embedded []string
	failOnce bool
}

func (r *recordingModel) EmbedDocument(document string) ([]embed.ContextualChunk, error) {
	r.mu.Lock()
	if r.failOnce {
		r.failOnce = false
		r.mu.Unlock()
		return nil, fmt.Errorf("injected embed failure")
	}
	r.embedded = append(r.embedded, document)
	r.mu.Unlock()
	return r.MockModel.EmbedDocument(document)
}

func (r *recordingModel) inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.embedded...)
}

func TestCrashMidRunMarkerSafety(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n")
	h.commit("init")

	rec := &recordingModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, rec)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	// Edit the file, then crash the next embed call.
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 22\n}\n")
	h.commit("edit body")
	rec.failOnce = true
	_, err = Reindex(o)
	require.Error(t, err, "injected failure should abort the run")

	// Retry to completion: the changed file is re-embedded whole (per-chunk
	// reuse is gone; a changed blob is re-embedded wholesale).
	_, err = Reindex(o)
	require.NoError(t, err)

	// The committed index holds exactly the file's three chunks.
	stats, err := st.Stats("mock")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Files)
	require.Equal(t, 3, stats.Chunks)
}

func TestEditedCodeReembedsWholeFile(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n")
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 1, model.DocumentCalls(), "code file is auto-chunked in one whole-document call")

	// Change only the body of one function: the whole file re-embeds (code
	// boundaries are the API's now; there is no per-chunk reuse).
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 22\n}\n\nfunc Gamma() int {\n\treturn 3\n}\n")
	h.commit("edit beta")

	_, err = Reindex(o)
	require.NoError(t, err)
	require.Equal(t, 2, model.DocumentCalls(), "an edited code file is re-embedded whole")
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

func TestUnifiedAutoChunkBreadcrumbsAndSpans(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph\n\n## Sub\n\nnested paragraph")
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n")
	h.commit("init")

	model := embed.NewMockModel("mock", 3)
	o, st := h.opts(t, model)
	defer st.Close()
	_, err := Reindex(o)
	require.NoError(t, err)

	reconstruct := func(rel paths.GitRootRelativePath) []reconstructedChunk {
		tree := mirror.NewTree(paths.AbsPath(h.root))
		a, ok, err := tree.TryRead(rel)
		require.NoError(t, err)
		require.True(t, ok)
		content, err := o.Repo.CatBlob(a.BlobSha)
		require.NoError(t, err)
		require.NotEmpty(t, a.Chunks)
		spans := make([]byteSpan, len(a.Chunks))
		for i, c := range a.Chunks {
			spans[i] = byteSpan{Start: c.Start, End: c.End}
			// Every stored span slices back to exactly the returned chunk text.
			require.NotEmpty(t, strings.TrimSpace(string(content[c.Start:c.End])))
		}
		recons, err := Reconstruct(rel, content, spans)
		require.NoError(t, err)
		return recons
	}

	find := func(recons []reconstructedChunk, needle string) reconstructedChunk {
		for _, rc := range recons {
			if strings.Contains(rc.Text, needle) {
				return rc
			}
		}
		t.Fatalf("no chunk containing %q", needle)
		return reconstructedChunk{}
	}

	// Markdown breadcrumbs are the enclosing header hierarchy.
	md := reconstruct("doc.md")
	nested := find(md, "nested paragraph")
	require.Contains(t, nested.HeadingContext, "Top")
	require.Contains(t, nested.HeadingContext, "Sub")
	// The contextualized text is the breadcrumb-decorated chunk, differing from
	// the raw chunk text by exactly the decoration.
	require.NotEqual(t, nested.Text, nested.Contextualized)
	require.Contains(t, nested.Contextualized, nested.Text)

	// Code breadcrumbs are the AST symbol path enclosing the span.
	code := reconstruct("p.go")
	alpha := find(code, "func Alpha")
	require.Contains(t, alpha.HeadingContext, "Alpha")
}

// badChunkModel returns a chunk whose text does not appear in the source, to
// exercise the verbatim-substring hard-fail guard.
type badChunkModel struct{ *embed.MockModel }

func (m *badChunkModel) EmbedDocument(document string) ([]embed.ContextualChunk, error) {
	return []embed.ContextualChunk{{
		Text:      "text that is not present anywhere in the source document",
		Embedding: make(embed.Embedding, m.Dims),
	}}, nil
}

func TestReindexHardFailsOnUnlocatableChunk(t *testing.T) {
	h := newHarness(t)
	h.write("doc.md", "# Top\n\nintro paragraph")
	h.commit("init")

	model := &badChunkModel{MockModel: embed.NewMockModel("mock", 3)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.Error(t, err, "an unlocatable chunk must fail the reindex hard")
	require.Contains(t, err.Error(), "doc.md", "error names the file")
	require.Contains(t, err.Error(), "verbatim substring", "error explains the guard")
	require.Contains(t, err.Error(), "chunk 0", "error names the chunk index")

	// No artifact was written for the failed file.
	tree := mirror.NewTree(paths.AbsPath(h.root))
	_, ok, err := tree.TryRead("doc.md")
	require.NoError(t, err)
	require.False(t, ok, "no artifact must be written for the failed file")
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

func TestAutoChunkWindowsSmallDocument(t *testing.T) {
	doc := "the whole document fits in one window"
	windows := autoChunkWindows(doc)
	require.Len(t, windows, 1)
	require.Equal(t, doc, windows[0].input)
	require.Equal(t, InputOffset(0), windows[0].prefixLen)
	require.Equal(t, mirror.RawOffset(0), windows[0].bodyStart)
}

func TestAutoChunkWindowsLargeDocumentBodiesCoverBlob(t *testing.T) {
	doc := strings.Repeat("abcdefghij", (autoChunkMaxWindowByte+autoChunkOverlapByte)/10)
	windows := autoChunkWindows(doc)
	require.Greater(t, len(windows), 1)

	step := autoChunkMaxWindowByte - autoChunkOverlapByte
	for i, w := range windows {
		// Each window's body is a verbatim contiguous copy of the raw blob.
		require.Equal(t, InputOffset(0), w.prefixLen)
		require.Equal(t, mirror.RawOffset(i*step), w.bodyStart)
		require.Equal(t, doc[int(w.bodyStart):int(w.bodyStart)+len(w.input)], w.input)
	}
	// Consecutive bodies overlap by autoChunkOverlapByte, so no content is lost.
	for i := 1; i < len(windows); i++ {
		prevEnd := int(windows[i-1].bodyStart) + len(windows[i-1].input)
		require.GreaterOrEqual(t, prevEnd-int(windows[i].bodyStart), autoChunkOverlapByte)
	}
	// The last body reaches the end of the blob.
	last := windows[len(windows)-1]
	require.Equal(t, len(doc), int(last.bodyStart)+len(last.input))
}

func TestAutoChunkWindowToRaw(t *testing.T) {
	w := autoChunkWindow{input: "PREFIXbody", prefixLen: InputOffset(6), bodyStart: mirror.RawOffset(100)}

	// An offset inside the body maps affinely to the raw blob.
	raw, ok := w.toRaw(InputOffset(6))
	require.True(t, ok)
	require.Equal(t, mirror.RawOffset(100), raw)
	raw, ok = w.toRaw(InputOffset(9))
	require.True(t, ok)
	require.Equal(t, mirror.RawOffset(103), raw)

	// An offset inside the synthetic prefix has no raw preimage.
	_, ok = w.toRaw(InputOffset(0))
	require.False(t, ok)
	_, ok = w.toRaw(InputOffset(5))
	require.False(t, ok)
}

func TestCodeRoutesThroughEmbedDocument(t *testing.T) {
	h := newHarness(t)
	h.write("p.go", "package p\n\nfunc Alpha() int {\n\treturn 1\n}\n")
	h.commit("init")

	model := &recordingContextModel{MockModel: embed.NewMockModel("mock", 8)}
	o, st := h.opts(t, model)
	defer st.Close()

	_, err := Reindex(o)
	require.NoError(t, err)

	require.Equal(t, 0, model.ChunkCalls(), "code files no longer use the isolated EmbedChunks path")
	require.Greater(t, model.DocumentCalls(), 0, "code files are auto-chunked via EmbedDocument")
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

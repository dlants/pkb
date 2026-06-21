package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/embed"
)

func mkChunk(text string) chunk.ChunkInfo {
	return chunk.ChunkInfo{
		Text:  text,
		Start: chunk.Position{Line: 1, Col: 0},
		End:   chunk.Position{Line: 1, Col: len(text)},
	}
}

func openTestStore(t *testing.T) (*Store, *embed.MockModel) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	m := embed.NewMockModel("mock@8", 8)
	if err := s.EnsureVecTable(m.ModelName(), m.Dimensions()); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	return s, m
}

func emb(t *testing.T, m *embed.MockModel, text string) embed.Embedding {
	t.Helper()
	e, err := m.EmbedChunk(text)
	if err != nil {
		t.Fatalf("EmbedChunk: %v", err)
	}
	return e
}

// writeGen writes a full generation for a path: start, insert each chunk,
// finalize. Returns the new generation.
func writeGen(t *testing.T, s *Store, m *embed.MockModel, path, blobSha, minorSpec string, chunks []chunk.ChunkInfo, augs []string) int64 {
	t.Helper()
	fileID, gen, err := s.StartFile(path, m.ModelName(), blobSha, minorSpec)
	if err != nil {
		t.Fatalf("StartFile: %v", err)
	}
	for i, c := range chunks {
		if err := s.InsertChunk(fileID, gen, m.ModelName(), c, c.Text, augs[i], minorSpec, emb(t, m, c.Text)); err != nil {
			t.Fatalf("InsertChunk: %v", err)
		}
	}
	if err := s.FinalizeFile(fileID, gen, m.ModelName()); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	return gen
}

func TestGenerationLifecycle(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"

	c1 := mkChunk("alpha chunk")
	c2 := mkChunk("beta chunk")
	writeGen(t, s, m, path, "sha1", "on|x|1", []chunk.ChunkInfo{c1, c2}, []string{"blurbA", "blurbB"})

	// Finalized file is searchable.
	res, err := s.Search(m.ModelName(), emb(t, m, "alpha chunk"), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 results, got %d", len(res))
	}
	if res[0].Text != "alpha chunk" {
		t.Fatalf("expected alpha chunk top hit, got %q", res[0].Text)
	}

	// Stats counts the committed generation.
	st, err := s.Stats(m.ModelName())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Files != 1 || st.Chunks != 2 {
		t.Fatalf("expected 1 file / 2 chunks, got %d / %d", st.Files, st.Chunks)
	}

	// Reuse map keys on ChunkKey and carries blurbs.
	reuse, err := s.ChunkEmbeddings(path, m.ModelName())
	if err != nil {
		t.Fatalf("ChunkEmbeddings: %v", err)
	}
	rc, ok := reuse[ChunkKey("", "alpha chunk")]
	if !ok {
		t.Fatalf("expected reuse entry for alpha chunk")
	}
	if rc.Augmentation != "blurbA" {
		t.Fatalf("expected blurbA, got %q", rc.Augmentation)
	}
	if len(rc.Embedding) != m.Dimensions() {
		t.Fatalf("expected %d dims, got %d", m.Dimensions(), len(rc.Embedding))
	}
}

func TestPartialGenerationInvisible(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"

	old := mkChunk("old chunk")
	writeGen(t, s, m, path, "sha1", "on|x|1", []chunk.ChunkInfo{old}, []string{"oldBlurb"})

	// Start a new generation and write part of it, but do not finalize.
	fileID, gen, err := s.StartFile(path, m.ModelName(), "sha2", "on|x|1")
	if err != nil {
		t.Fatalf("StartFile: %v", err)
	}
	newChunk := mkChunk("new chunk")
	if err := s.InsertChunk(fileID, gen, m.ModelName(), newChunk, newChunk.Text, "newBlurb", "on|x|1", emb(t, m, newChunk.Text)); err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}

	// Search still returns only the committed generation.
	res, err := s.Search(m.ModelName(), emb(t, m, "new chunk"), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.Text == "new chunk" {
			t.Fatalf("uncommitted generation should be invisible to Search")
		}
	}

	// Reuse map only sees the committed generation too.
	reuse, err := s.ChunkEmbeddings(path, m.ModelName())
	if err != nil {
		t.Fatalf("ChunkEmbeddings: %v", err)
	}
	if _, ok := reuse[ChunkKey("", "new chunk")]; ok {
		t.Fatalf("uncommitted chunk should not be reusable")
	}
	if _, ok := reuse[ChunkKey("", "old chunk")]; !ok {
		t.Fatalf("committed chunk should still be reusable")
	}

	// Finalize: new generation visible, old generation dropped.
	if err := s.FinalizeFile(fileID, gen, m.ModelName()); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}
	st, err := s.Stats(m.ModelName())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Chunks != 1 {
		t.Fatalf("expected 1 chunk after finalize, got %d", st.Chunks)
	}
	res, err = s.Search(m.ModelName(), emb(t, m, "new chunk"), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Text != "new chunk" {
		t.Fatalf("expected only new chunk after finalize, got %v", res)
	}
}

func TestStartFileClearsCrashedAttempt(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"

	good := mkChunk("good chunk")
	writeGen(t, s, m, path, "sha1", "off||", []chunk.ChunkInfo{good}, []string{""})

	// Simulate a crashed attempt: start gen, write a chunk, never finalize.
	fileID, gen, err := s.StartFile(path, m.ModelName(), "sha2", "off||")
	if err != nil {
		t.Fatalf("StartFile: %v", err)
	}
	half := mkChunk("half chunk")
	if err := s.InsertChunk(fileID, gen, m.ModelName(), half, half.Text, "", "off||", emb(t, m, half.Text)); err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}

	// Retry: StartFile must clear the half-written gen and the old committed
	// generation stays intact.
	fileID2, gen2, err := s.StartFile(path, m.ModelName(), "sha2", "off||")
	if err != nil {
		t.Fatalf("StartFile retry: %v", err)
	}
	if fileID2 != fileID {
		t.Fatalf("expected same file id, got %d vs %d", fileID2, fileID)
	}
	retry := mkChunk("retry chunk")
	if err := s.InsertChunk(fileID2, gen2, m.ModelName(), retry, retry.Text, "", "off||", emb(t, m, retry.Text)); err != nil {
		t.Fatalf("InsertChunk retry: %v", err)
	}
	if err := s.FinalizeFile(fileID2, gen2, m.ModelName()); err != nil {
		t.Fatalf("FinalizeFile: %v", err)
	}

	st, err := s.Stats(m.ModelName())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Chunks != 1 {
		t.Fatalf("expected exactly 1 chunk, got %d (stale gen not cleared?)", st.Chunks)
	}
	res, err := s.Search(m.ModelName(), emb(t, m, "retry chunk"), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Text != "retry chunk" {
		t.Fatalf("expected only retry chunk, got %v", res)
	}
}

func TestMigrationFromOldSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// Build a database with the pre-generations schema and an existing row.
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = raw.Exec(`
		CREATE TABLE files (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL,
			model_name TEXT NOT NULL,
			embedding_version INTEGER NOT NULL,
			blob_sha TEXT NOT NULL,
			UNIQUE(path, model_name, embedding_version)
		);
		CREATE TABLE chunks (
			id INTEGER PRIMARY KEY,
			file_id INTEGER NOT NULL,
			text TEXT NOT NULL,
			contextualized_text TEXT NOT NULL,
			start_line INTEGER NOT NULL,
			start_col INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			end_col INTEGER NOT NULL
		);
		INSERT INTO files (path, model_name, embedding_version, blob_sha)
			VALUES ('legacy.md', 'mock@8', 3, 'oldsha');
		INSERT INTO chunks (file_id, text, contextualized_text, start_line, start_col, end_line, end_col)
			VALUES (1, 'legacy chunk', 'legacy chunk', 1, 0, 1, 12);
	`)
	if err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	raw.Close()

	// Open via Store: migrations should add the new columns with safe defaults.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	files, err := s.IndexedFiles("mock@8")
	if err != nil {
		t.Fatalf("IndexedFiles: %v", err)
	}
	meta, ok := files["legacy.md"]
	if !ok {
		t.Fatalf("expected migrated legacy file")
	}
	if !meta.Complete {
		t.Fatalf("expected legacy file to default to complete=true")
	}
	if meta.Sha != "oldsha" {
		t.Fatalf("expected preserved sha, got %q", meta.Sha)
	}

	// Existing chunk (gen=0=indexed_gen) remains visible via Stats.
	st, err := s.Stats("mock@8")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Files != 1 || st.Chunks != 1 {
		t.Fatalf("expected 1 file / 1 chunk after migration, got %d / %d", st.Files, st.Chunks)
	}
}

func TestIndexedFilesExposesCompleteAndMinorSpec(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"
	writeGen(t, s, m, path, "sha1", "on|model@1|1", []chunk.ChunkInfo{mkChunk("x")}, []string{""})

	files, err := s.IndexedFiles(m.ModelName())
	if err != nil {
		t.Fatalf("IndexedFiles: %v", err)
	}
	meta, ok := files[path]
	if !ok {
		t.Fatalf("expected file %q", path)
	}
	if !meta.Complete {
		t.Fatalf("expected complete=true after finalize")
	}
	if meta.MinorSpec != "on|model@1|1" {
		t.Fatalf("expected minor spec recorded, got %q", meta.MinorSpec)
	}

	// An unfinalized start marks the file incomplete.
	if _, _, err := s.StartFile(path, m.ModelName(), "sha2", "off||"); err != nil {
		t.Fatalf("StartFile: %v", err)
	}
	files, err = s.IndexedFiles(m.ModelName())
	if err != nil {
		t.Fatalf("IndexedFiles: %v", err)
	}
	if files[path].Complete {
		t.Fatalf("expected complete=false during an in-progress generation")
	}
}

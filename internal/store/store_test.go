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

// putFile (re)indexes a path via PutFile, deriving the contextualized text,
// aug specs, and embeddings from the supplied chunks/blurbs.
func putFile(t *testing.T, s *Store, m *embed.MockModel, path, blobSha, minorSpec string, chunks []chunk.ChunkInfo, augs []string) {
	t.Helper()
	contextualized := make([]string, len(chunks))
	augSpecs := make([]string, len(chunks))
	embeddings := make([]embed.Embedding, len(chunks))
	for i, c := range chunks {
		contextualized[i] = c.Text
		augSpecs[i] = minorSpec
		embeddings[i] = emb(t, m, c.Text)
	}
	if err := s.PutFile(path, m.ModelName(), blobSha, minorSpec, chunks, contextualized, augs, augSpecs, embeddings); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
}

func TestPutFileLifecycle(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"

	c1 := mkChunk("alpha chunk")
	c2 := mkChunk("beta chunk")
	putFile(t, s, m, path, "sha1", "on|x|1", []chunk.ChunkInfo{c1, c2}, []string{"blurbA", "blurbB"})

	// Indexed file is searchable.
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

func TestPutFileReplacesOldChunks(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"

	putFile(t, s, m, path, "sha1", "off||", []chunk.ChunkInfo{mkChunk("old chunk")}, []string{""})
	// Reindex the same path with new content: old chunks must be replaced.
	putFile(t, s, m, path, "sha2", "off||", []chunk.ChunkInfo{mkChunk("new chunk")}, []string{""})

	st, err := s.Stats(m.ModelName())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Files != 1 || st.Chunks != 1 {
		t.Fatalf("expected 1 file / 1 chunk after replace, got %d / %d", st.Files, st.Chunks)
	}
	res, err := s.Search(m.ModelName(), emb(t, m, "new chunk"), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Text != "new chunk" {
		t.Fatalf("expected only new chunk, got %v", res)
	}
}

func TestMigrationFromOldSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// Build a database with the pre-augmentation schema and an existing row.
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
			VALUES ('legacy.md', 'mock@8', 4, 'oldsha');
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
	if meta.Sha != "oldsha" {
		t.Fatalf("expected preserved sha, got %q", meta.Sha)
	}

	// Existing chunk remains visible via Stats.
	st, err := s.Stats("mock@8")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Files != 1 || st.Chunks != 1 {
		t.Fatalf("expected 1 file / 1 chunk after migration, got %d / %d", st.Files, st.Chunks)
	}
}

func TestIndexedFilesExposesMinorSpec(t *testing.T) {
	s, m := openTestStore(t)
	path := "doc.md"
	putFile(t, s, m, path, "sha1", "on|model@1|1", []chunk.ChunkInfo{mkChunk("x")}, []string{""})

	files, err := s.IndexedFiles(m.ModelName())
	if err != nil {
		t.Fatalf("IndexedFiles: %v", err)
	}
	meta, ok := files[path]
	if !ok {
		t.Fatalf("expected file %q", path)
	}
	if meta.MinorSpec != "on|model@1|1" {
		t.Fatalf("expected minor spec recorded, got %q", meta.MinorSpec)
	}
}

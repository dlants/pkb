// Package store wraps the sqlite + sqlite-vec database: a files table keyed by
// (path, model, version), a chunks table, and one vec0 table per
// (modelName, version). There are no tracked-source/exclusion tables.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"strings"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/embed"
)

// MajorVersion is the embedding compatibility identity ("major version").
// Together with the embedding model_name (which already encodes model@dims),
// it forms the major identity that keys each vec table and the files row, and
// determines when a stored vector is still usable. Bump it whenever anything
// that changes the meaning of an embedding changes: the chunking algorithm,
// breadcrumb/heading-context handling, tree-sitter grammars, or the tags.scm /
// pkb chunking logic. Bumping it isolates old vectors into separate vec tables
// and forces a full recompute.
const MajorVersion = 5

var vecOnce bool

// Store owns the database connection.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at dbPath and ensures the base
// schema exists.
func Open(dbPath string) (*Store, error) {
	if !vecOnce {
		sqlite_vec.Auto()
		vecOnce = true
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Vacuum reclaims free pages left behind by reindex churn, shrinking the db
// file to its live size.
func (s *Store) Vacuum() error {
	_, err := s.db.Exec("VACUUM")
	return err
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL,
			model_name TEXT NOT NULL,
			embedding_version INTEGER NOT NULL,
			blob_sha TEXT NOT NULL,
			UNIQUE(path, model_name, embedding_version)
		);
		CREATE TABLE IF NOT EXISTS chunks (
			id INTEGER PRIMARY KEY,
			file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			text TEXT NOT NULL,
			contextualized_text TEXT NOT NULL,
			heading_context TEXT NOT NULL DEFAULT '',
			start_line INTEGER NOT NULL,
			start_col INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			end_col INTEGER NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	// addColumn applies an idempotent ALTER TABLE migration, tolerating the
	// "duplicate column name" error so re-opening an already-migrated db is a
	// no-op.
	addColumn := func(table, def string) error {
		_, aerr := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s`, table, def))
		if aerr != nil && !strings.Contains(aerr.Error(), "duplicate column name") {
			return aerr
		}
		return nil
	}
	// Migrate pre-existing databases that lack the newer columns.
	for _, m := range []struct{ table, def string }{
		{"chunks", "heading_context TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumn(m.table, m.def); err != nil {
			return err
		}
	}
	return nil
}

var nonIdent = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func vecTableName(modelName string, version int) string {
	return fmt.Sprintf("vec_%s_v%d", nonIdent.ReplaceAllString(modelName, "_"), version)
}

// EnsureVecTable creates the vec0 table for a model if it does not exist.
func (s *Store) EnsureVecTable(modelName string, dims int) error {
	name := vecTableName(modelName, MajorVersion)
	_, err := s.db.Exec(fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(chunk_id INTEGER PRIMARY KEY, embedding float[%d] distance_metric=cosine)`,
		name, dims))
	return err
}

// FileMeta records the reuse-relevant metadata stored for an indexed file: the
// blob sha.
type FileMeta struct {
	Sha string
}

// IndexedFiles returns a map of relative path -> FileMeta for files already
// indexed by the given model.
func (s *Store) IndexedFiles(modelName string) (map[string]FileMeta, error) {
	rows, err := s.db.Query(
		`SELECT path, blob_sha FROM files WHERE model_name = ? AND embedding_version = ?`,
		modelName, MajorVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileMeta{}
	for rows.Next() {
		var path, sha string
		if err := rows.Scan(&path, &sha); err != nil {
			return nil, err
		}
		out[path] = FileMeta{Sha: sha}
	}
	return out, rows.Err()
}

// DeleteFile removes a file's row, its chunks, and its vec entries.
func (s *Store) DeleteFile(path, modelName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteFileTx(tx, path, modelName); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteFileTx(tx *sql.Tx, path, modelName string) error {
	var fileID int64
	err := tx.QueryRow(
		`SELECT id FROM files WHERE path = ? AND model_name = ? AND embedding_version = ?`,
		path, modelName, MajorVersion).Scan(&fileID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	vec := vecTableName(modelName, MajorVersion)
	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM %s WHERE chunk_id IN (SELECT id FROM chunks WHERE file_id = ?)`, vec), fileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chunks WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM files WHERE id = ?`, fileID)
	return err
}

// PutFile (re)indexes a single file in one transaction: it deletes any existing
// rows for the path, inserts the file row recording the new blob sha, then
// inserts every chunk (with its vector). Because the whole write is a single
// transaction, a crash leaves the previously committed state intact, so the
// file is simply reindexed on the next run; expensive embedding work has
// already been done in memory by the caller before this is invoked.
func (s *Store) PutFile(path, modelName, blobSha string, chunks []chunk.ChunkInfo, contextualized []string, embeddings []embed.Embedding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := deleteFileTx(tx, path, modelName); err != nil {
		return err
	}

	res, err := tx.Exec(
		`INSERT INTO files (path, model_name, embedding_version, blob_sha) VALUES (?, ?, ?, ?)`,
		path, modelName, MajorVersion, blobSha)
	if err != nil {
		return err
	}
	fileID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	vec := vecTableName(modelName, MajorVersion)
	for i, c := range chunks {
		cres, err := tx.Exec(
			`INSERT INTO chunks (file_id, text, contextualized_text, heading_context, start_line, start_col, end_line, end_col)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			fileID, c.Text, contextualized[i], c.HeadingContext, c.Start.Line, c.Start.Col, c.End.Line, c.End.Col)
		if err != nil {
			return err
		}
		chunkID, err := cres.LastInsertId()
		if err != nil {
			return err
		}
		blob, err := sqlite_vec.SerializeFloat32(embeddings[i])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s (chunk_id, embedding) VALUES (?, ?)`, vec), chunkID, blob); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ChunkKey is the reuse key for a chunk: the deterministic inputs to its
// embedding (heading breadcrumb + raw text). A chunk re-embeds when its text or
// any parent heading/breadcrumb changes, but is reused otherwise.
func ChunkKey(headingContext, text string) string {
	return headingContext + "\x00" + text
}

// ChunkEmbeddings returns a map of ChunkKey -> embedding for a path/model, so an
// incremental reindex can reuse vectors for unchanged chunks instead of
// re-embedding them. Duplicate keys collapse harmlessly (identical
// deterministic input yields an identical embedding).
func (s *Store) ChunkEmbeddings(path, modelName string) (map[string]embed.Embedding, error) {
	vec := vecTableName(modelName, MajorVersion)
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT c.heading_context, c.text, v.embedding
		 FROM chunks c
		 JOIN files f ON f.id = c.file_id
		 JOIN %s v ON v.chunk_id = c.id
		 WHERE f.path = ? AND f.model_name = ? AND f.embedding_version = ?`, vec),
		path, modelName, MajorVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]embed.Embedding{}
	for rows.Next() {
		var headingContext, text string
		var blob []byte
		if err := rows.Scan(&headingContext, &text, &blob); err != nil {
			return nil, err
		}
		out[ChunkKey(headingContext, text)] = deserializeFloat32(blob)
	}
	return out, rows.Err()
}

func deserializeFloat32(b []byte) embed.Embedding {
	out := make(embed.Embedding, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// CleanupOrphans drops vec tables and removes files/chunks rows for any model
// that is not in activeModels (at the current MajorVersion). This is how a
// model-name change reclaims storage instead of silently mixing vectors.
func (s *Store) CleanupOrphans(activeModels []string) error {
	keep := map[string]struct{}{}
	for _, m := range activeModels {
		keep[vecTableName(m, MajorVersion)] = struct{}{}
	}

	rows, err := s.db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND sql LIKE '%USING vec0%'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, name := range tables {
		if _, ok := keep[name]; ok {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
			return err
		}
	}

	// Remove files/chunks rows whose model is no longer active.
	mrows, err := s.db.Query(`SELECT DISTINCT model_name FROM files`)
	if err != nil {
		return err
	}
	var models []string
	for mrows.Next() {
		var m string
		if err := mrows.Scan(&m); err != nil {
			mrows.Close()
			return err
		}
		models = append(models, m)
	}
	if err := mrows.Err(); err != nil {
		mrows.Close()
		return err
	}
	mrows.Close()

	active := map[string]struct{}{}
	for _, m := range activeModels {
		active[m] = struct{}{}
	}
	for _, m := range models {
		if _, ok := active[m]; ok {
			continue
		}
		if _, err := s.db.Exec(
			`DELETE FROM chunks WHERE file_id IN (SELECT id FROM files WHERE model_name = ?)`, m); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM files WHERE model_name = ?`, m); err != nil {
			return err
		}
	}
	return nil
}

// SearchResult is one hit from a vector search.
type SearchResult struct {
	Path           string
	Text           string
	HeadingContext string
	StartLine      int
	EndLine        int
	Score          float64
}

// Search queries a model's vec table for the topK nearest chunks to the query
// embedding.
func (s *Store) Search(modelName string, query embed.Embedding, topK int) ([]SearchResult, error) {
	vec := vecTableName(modelName, MajorVersion)
	blob, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT f.path, c.text, c.heading_context, c.start_line, c.end_line, v.distance
		 FROM %s v
		 JOIN chunks c ON c.id = v.chunk_id
		 JOIN files f ON f.id = c.file_id
		 WHERE v.embedding MATCH ? AND v.k = ?
		 ORDER BY v.distance`, vec), blob, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var distance float64
		if err := rows.Scan(&r.Path, &r.Text, &r.HeadingContext, &r.StartLine, &r.EndLine, &distance); err != nil {
			return nil, err
		}
		r.Score = 1 - distance
		results = append(results, r)
	}
	return results, rows.Err()
}

// Stats holds index counts for a model.
type Stats struct {
	Files  int
	Chunks int
}

// Stats returns file and chunk counts for a model.
func (s *Store) Stats(modelName string) (Stats, error) {
	var st Stats
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM files WHERE model_name = ? AND embedding_version = ?`,
		modelName, MajorVersion).Scan(&st.Files)
	if err != nil {
		return st, err
	}
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM chunks c JOIN files f ON c.file_id = f.id
		 WHERE f.model_name = ? AND f.embedding_version = ?`,
		modelName, MajorVersion).Scan(&st.Chunks)
	return st, err
}

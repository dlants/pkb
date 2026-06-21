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
// and forces a full recompute. The augmentation spec (see minorSpec in
// internal/index) is deliberately NOT part of this identity.
const MajorVersion = 3

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
			inference_model TEXT NOT NULL DEFAULT '',
			complete INTEGER NOT NULL DEFAULT 1,
			indexed_gen INTEGER NOT NULL DEFAULT 0,
			minor_spec TEXT NOT NULL DEFAULT '',
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
			end_col INTEGER NOT NULL,
			gen INTEGER NOT NULL DEFAULT 0,
			augmentation TEXT NOT NULL DEFAULT '',
			aug_spec TEXT NOT NULL DEFAULT ''
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
	// Migrate pre-existing databases that lack the newer columns. Defaults are
	// chosen so existing rows remain valid: gen=0=indexed_gen, complete=1.
	for _, m := range []struct{ table, def string }{
		{"chunks", "heading_context TEXT NOT NULL DEFAULT ''"},
		{"files", "inference_model TEXT NOT NULL DEFAULT ''"},
		{"files", "complete INTEGER NOT NULL DEFAULT 1"},
		{"files", "indexed_gen INTEGER NOT NULL DEFAULT 0"},
		{"files", "minor_spec TEXT NOT NULL DEFAULT ''"},
		{"chunks", "gen INTEGER NOT NULL DEFAULT 0"},
		{"chunks", "augmentation TEXT NOT NULL DEFAULT ''"},
		{"chunks", "aug_spec TEXT NOT NULL DEFAULT ''"},
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
// blob sha and the inference-model identity that produced its augmented
// embeddings (empty when augmentation was disabled).
type FileMeta struct {
	Sha            string
	InferenceModel string
	Complete       bool
	MinorSpec      string
}

// IndexedFiles returns a map of relative path -> FileMeta for files already
// indexed by the given model.
func (s *Store) IndexedFiles(modelName string) (map[string]FileMeta, error) {
	rows, err := s.db.Query(
		`SELECT path, blob_sha, inference_model, complete, minor_spec FROM files WHERE model_name = ? AND embedding_version = ?`,
		modelName, MajorVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileMeta{}
	for rows.Next() {
		var path, sha, inferenceModel, minorSpec string
		var complete int
		if err := rows.Scan(&path, &sha, &inferenceModel, &complete, &minorSpec); err != nil {
			return nil, err
		}
		out[path] = FileMeta{Sha: sha, InferenceModel: inferenceModel, Complete: complete != 0, MinorSpec: minorSpec}
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

// PutFile (re)indexes a single file: it deletes any existing rows for the path
// and inserts the new chunks + embeddings in one transaction.
func (s *Store) PutFile(path, modelName, blobSha, inferenceModel string, chunks []chunk.ChunkInfo, contextualized []string, embeddings []embed.Embedding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := deleteFileTx(tx, path, modelName); err != nil {
		return err
	}

	res, err := tx.Exec(
		`INSERT INTO files (path, model_name, embedding_version, blob_sha, inference_model) VALUES (?, ?, ?, ?, ?)`,
		path, modelName, MajorVersion, blobSha, inferenceModel)
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

// StartFile begins an incremental, generation-guarded (re)index of a file. It
// ensures the file row exists, records the target blob sha and minor spec,
// marks the row incomplete, clears any half-written rows left by a prior
// crashed attempt (any chunk whose gen differs from the committed
// indexed_gen), and returns the file id plus the new generation
// (indexed_gen + 1) under which fresh chunks should be written. The committed
// generation's chunks are left intact so they remain searchable and reusable
// until FinalizeFile swaps generations.
func (s *Store) StartFile(path, modelName, blobSha, minorSpec string) (fileID, newGen int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	var indexedGen int64
	err = tx.QueryRow(
		`SELECT id, indexed_gen FROM files WHERE path = ? AND model_name = ? AND embedding_version = ?`,
		path, modelName, MajorVersion).Scan(&fileID, &indexedGen)
	switch {
	case err == sql.ErrNoRows:
		res, ierr := tx.Exec(
			`INSERT INTO files (path, model_name, embedding_version, blob_sha, inference_model, complete, indexed_gen, minor_spec)
			 VALUES (?, ?, ?, ?, '', 0, 0, ?)`,
			path, modelName, MajorVersion, blobSha, minorSpec)
		if ierr != nil {
			return 0, 0, ierr
		}
		if fileID, ierr = res.LastInsertId(); ierr != nil {
			return 0, 0, ierr
		}
		indexedGen = 0
	case err != nil:
		return 0, 0, err
	default:
		if _, uerr := tx.Exec(
			`UPDATE files SET blob_sha = ?, minor_spec = ?, complete = 0 WHERE id = ?`,
			blobSha, minorSpec, fileID); uerr != nil {
			return 0, 0, uerr
		}
	}

	if err = deleteChunksOtherGenTx(tx, modelName, fileID, indexedGen); err != nil {
		return 0, 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return fileID, indexedGen + 1, nil
}

// InsertChunk persists a single chunk (and its vector) under the given
// generation in its own transaction, so progress survives a crash. The
// augmentation blurb and the minor spec that produced it are stored on the row
// for inspection.
func (s *Store) InsertChunk(fileID, gen int64, modelName string, c chunk.ChunkInfo, contextualized, augmentation, augSpec string, embedding embed.Embedding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	cres, err := tx.Exec(
		`INSERT INTO chunks (file_id, text, contextualized_text, heading_context, start_line, start_col, end_line, end_col, gen, augmentation, aug_spec)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, c.Text, contextualized, c.HeadingContext, c.Start.Line, c.Start.Col, c.End.Line, c.End.Col, gen, augmentation, augSpec)
	if err != nil {
		return err
	}
	chunkID, err := cres.LastInsertId()
	if err != nil {
		return err
	}
	blob, err := sqlite_vec.SerializeFloat32(embedding)
	if err != nil {
		return err
	}
	vec := vecTableName(modelName, MajorVersion)
	if _, err := tx.Exec(fmt.Sprintf(`INSERT INTO %s (chunk_id, embedding) VALUES (?, ?)`, vec), chunkID, blob); err != nil {
		return err
	}
	return tx.Commit()
}

// FinalizeFile commits a generation swap: it advances indexed_gen to newGen,
// marks the file complete, and drops every chunk (and vector) of any other
// generation. Only after this does the new generation become visible to Search
// and ChunkEmbeddings; the old generation's free pages are reclaimed by the
// end-of-run Vacuum.
func (s *Store) FinalizeFile(fileID, newGen int64, modelName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE files SET indexed_gen = ?, complete = 1 WHERE id = ?`, newGen, fileID); err != nil {
		return err
	}
	if err := deleteChunksOtherGenTx(tx, modelName, fileID, newGen); err != nil {
		return err
	}
	return tx.Commit()
}

// deleteChunksOtherGenTx removes every chunk (and its vec row) of a file whose
// gen differs from keepGen. StartFile uses it to discard a crashed attempt's
// half-written rows; FinalizeFile uses it to drop the superseded generation.
func deleteChunksOtherGenTx(tx *sql.Tx, modelName string, fileID, keepGen int64) error {
	vec := vecTableName(modelName, MajorVersion)
	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM %s WHERE chunk_id IN (SELECT id FROM chunks WHERE file_id = ? AND gen <> ?)`, vec),
		fileID, keepGen); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM chunks WHERE file_id = ? AND gen <> ?`, fileID, keepGen)
	return err
}

// ChunkKey is the reuse key for a chunk: the deterministic inputs to its
// embedding (heading breadcrumb + raw text). It deliberately excludes any
// non-deterministic augmentation, so a chunk re-embeds when its text or any
// parent heading/breadcrumb changes, but is reused otherwise.
func ChunkKey(headingContext, text string) string {
	return headingContext + "\x00" + text
}

// ReuseChunk carries the reusable state for an unchanged chunk: its stored
// embedding plus the augmentation blurb that was embedded with it (empty when
// the chunk was never augmented).
type ReuseChunk struct {
	Embedding    embed.Embedding
	Augmentation string
	AugSpec      string
}

// ChunkEmbeddings returns a map of ChunkKey -> ReuseChunk for a path/model, so
// an incremental reindex can reuse vectors (and their stored augmentation
// blurbs) for unchanged chunks instead of re-embedding/re-augmenting them. Only
// rows of the file's committed generation (c.gen = f.indexed_gen) are returned,
// so a partially written new generation is never reused. Duplicate keys
// collapse harmlessly (identical deterministic input yields an identical
// embedding).
func (s *Store) ChunkEmbeddings(path, modelName string) (map[string]ReuseChunk, error) {
	vec := vecTableName(modelName, MajorVersion)
	rows, err := s.db.Query(fmt.Sprintf(
		`SELECT c.heading_context, c.text, c.augmentation, c.aug_spec, v.embedding
		 FROM chunks c
		 JOIN files f ON f.id = c.file_id
		 JOIN %s v ON v.chunk_id = c.id
		 WHERE f.path = ? AND f.model_name = ? AND f.embedding_version = ? AND c.gen = f.indexed_gen`, vec),
		path, modelName, MajorVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ReuseChunk{}
	for rows.Next() {
		var headingContext, text, augmentation, augSpec string
		var blob []byte
		if err := rows.Scan(&headingContext, &text, &augmentation, &augSpec, &blob); err != nil {
			return nil, err
		}
		out[ChunkKey(headingContext, text)] = ReuseChunk{Embedding: deserializeFloat32(blob), Augmentation: augmentation, AugSpec: augSpec}
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
		 WHERE v.embedding MATCH ? AND v.k = ? AND c.gen = f.indexed_gen
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
		 WHERE f.model_name = ? AND f.embedding_version = ? AND c.gen = f.indexed_gen`,
		modelName, MajorVersion).Scan(&st.Chunks)
	return st, err
}

// Package index implements the reindex flow: it diffs the marker commit
// (.pkb/state.json) against a target ref (or does a full ls-tree on cold
// start/recovery), then indexes/updates/deletes files. There is no watcher;
// reindex runs to completion and exits.
package index

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/filetype"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/paths"
	"github.com/dlants/pkb/internal/store"
)

// State is the persisted marker recording how far indexing has progressed.
type State struct {
	Commit     string `json:"commit"`
	IndexedAt  string `json:"indexedAt"`
	FileCount  int    `json:"fileCount"`
	ChunkCount int    `json:"chunkCount"`
}

const statePath = ".pkb/state.json"

// Ignore matches paths against .pkbignore patterns using simple segment/prefix
// matching (full gitignore semantics are out of scope for v1).
type Ignore struct {
	patterns []string
}

// LoadIgnore reads .pkbignore from the repo root (missing file -> empty Ignore).
func LoadIgnore(repoRoot string) (*Ignore, error) {
	f, err := os.Open(filepath.Join(repoRoot, ".pkbignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return &Ignore{}, nil
		}
		return nil, err
	}
	defer f.Close()
	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, strings.TrimRight(line, "/"))
	}
	return &Ignore{patterns: patterns}, scanner.Err()
}

// Match reports whether relPath is excluded by any pattern.
func (i *Ignore) Match(rel paths.GitRootRelativePath) bool {
	relPath := string(rel)
	base := filepath.Base(relPath)
	for _, p := range i.patterns {
		if base == p {
			return true
		}
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			return true
		}
	}
	return false
}

// Options configures a reindex run.
type Options struct {
	Repo  *git.Repo
	Store *store.Store
	// Model embeds all files (code and text).
	Model  embed.EmbeddingModel
	Ref    string
	Ignore *Ignore
	// ExtOverrides forces a file extension to a file type ("code"/"text").
	ExtOverrides map[string]string
}

// activeModels returns the embedding models in use.
func (o *Options) activeModels() []embed.EmbeddingModel {
	return []embed.EmbeddingModel{o.Model}
}

// route returns the file type for a path, applying any extension overrides.
func (o *Options) route(path paths.GitRootRelativePath) filetype.FileType {
	ext := strings.ToLower(filepath.Ext(string(path)))
	if o.ExtOverrides != nil {
		if t, ok := o.ExtOverrides[ext]; ok {
			if t == "code" {
				return filetype.Code
			}
			return filetype.Text
		}
	}
	return filetype.RoutePath(string(path)).Type
}

// grammarFor returns the tree-sitter grammar name for a code path (empty if the
// extension has no recognized grammar, in which case ChunkCode falls back to
// line-based chunking).
func (o *Options) grammarFor(path paths.GitRootRelativePath) string {
	return filetype.RoutePath(string(path)).Grammar
}

// textExts is the allowlist of indexable text extensions.
var textExts = map[string]struct{}{
	".md":       {},
	".markdown": {},
	".txt":      {},
}

// candidate reports whether a path should be indexed: a recognized code file,
// or an allowlisted text file; never the .pkb state dir; not ignored.
func (o *Options) candidate(path paths.GitRootRelativePath) bool {
	if path == ".pkbignore" || strings.HasPrefix(string(path), ".pkb/") {
		return false
	}
	if o.Ignore != nil && o.Ignore.Match(path) {
		return false
	}
	if o.route(path) == filetype.Code {
		return true
	}
	_, ok := textExts[strings.ToLower(filepath.Ext(string(path)))]
	return ok
}

func readState(repoRoot string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, statePath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeState(repoRoot string, s State) error {
	dir := filepath.Join(repoRoot, ".pkb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(repoRoot, statePath), data, 0o644)
}

// Reindex brings the index in sync with the target ref and, only on success,
// advances the marker.
func Reindex(o *Options) (State, error) {
	ref := o.Ref
	if ref == "" {
		ref = "HEAD"
	}
	repoRoot := string(o.Repo.Root)

	targetSha, err := o.Repo.ResolveRef(ref)
	if err != nil {
		return State{}, err
	}

	models := o.activeModels()
	for _, m := range models {
		if err := o.Store.EnsureVecTable(m.ModelName(), m.Dimensions()); err != nil {
			return State{}, err
		}
	}
	activeNames := make([]string, len(models))
	for i, m := range models {
		activeNames[i] = m.ModelName()
	}
	if err := o.Store.CleanupOrphans(activeNames); err != nil {
		return State{}, err
	}

	// indexed maps each already-indexed path to the model that embedded it and
	// its stored blob sha.
	indexed := map[paths.GitRootRelativePath]indexedEntry{}
	for _, m := range models {
		files, err := o.Store.IndexedFiles(m.ModelName())
		if err != nil {
			return State{}, err
		}
		for path, sha := range files {
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: sha}
		}
	}

	treeFiles, err := o.Repo.LsTree(ref)
	if err != nil {
		return State{}, err
	}
	treeMap := make(map[paths.GitRootRelativePath]string, len(treeFiles))
	for _, f := range treeFiles {
		treeMap[f.Path] = f.BlobSha
	}

	prev, err := readState(repoRoot)
	if err != nil {
		return State{}, err
	}

	touched, err := o.touchedPaths(prev, targetSha, treeMap, indexed)
	if err != nil {
		return State{}, err
	}

	for path := range touched {
		blobSha, inTree := treeMap[path]
		prevEntry, wasIndexed := indexed[path]
		if inTree && o.candidate(path) {
			model := o.Model
			if wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha {
				continue // content unchanged, same model; skip embed
			}
			// If a different model previously embedded this path (e.g. routing
			// changed), purge the stale rows first.
			if wasIndexed && prevEntry.model != model.ModelName() {
				if err := o.Store.DeleteFile(string(path), prevEntry.model); err != nil {
					return State{}, err
				}
			}
			if err := o.indexFile(path, blobSha, model); err != nil {
				return State{}, err
			}
		} else {
			if wasIndexed {
				if err := o.Store.DeleteFile(string(path), prevEntry.model); err != nil {
					return State{}, err
				}
			}
		}
	}

	var stats store.Stats
	for _, m := range models {
		s, err := o.Store.Stats(m.ModelName())
		if err != nil {
			return State{}, err
		}
		stats.Files += s.Files
		stats.Chunks += s.Chunks
	}
	st := State{
		Commit:     targetSha,
		IndexedAt:  time.Now().UTC().Format(time.RFC3339),
		FileCount:  stats.Files,
		ChunkCount: stats.Chunks,
	}
	if err := writeState(repoRoot, st); err != nil {
		return State{}, err
	}
	return st, nil
}

// touchedPaths computes the set of paths that might need work, choosing the
// incremental, divergence, or full strategy.
func (o *Options) touchedPaths(prev *State, targetSha string, treeMap map[paths.GitRootRelativePath]string, indexed map[paths.GitRootRelativePath]indexedEntry) (map[paths.GitRootRelativePath]struct{}, error) {
	touched := map[paths.GitRootRelativePath]struct{}{}

	full := prev == nil || prev.Commit == "" || !o.Repo.ObjectExists(prev.Commit)
	if !full && prev.Commit == targetSha {
		return touched, nil // nothing changed
	}

	if full {
		for path := range treeMap {
			if o.candidate(path) {
				touched[path] = struct{}{}
			}
		}
		for path := range indexed {
			touched[path] = struct{}{}
		}
		return touched, nil
	}

	addDiff := func(from, to string) error {
		changes, err := o.Repo.DiffNameStatus(from, to)
		if err != nil {
			return err
		}
		for _, c := range changes {
			if c.Path != "" {
				touched[c.Path] = struct{}{}
			}
			if c.OldPath != "" {
				touched[c.OldPath] = struct{}{}
			}
		}
		return nil
	}

	if o.Repo.IsAncestor(prev.Commit, targetSha) {
		if err := addDiff(prev.Commit, targetSha); err != nil {
			return nil, err
		}
		return touched, nil
	}

	// Divergence: union of diffs from the merge-base to each side.
	base, err := o.Repo.MergeBase(prev.Commit, targetSha)
	if err != nil {
		return nil, err
	}
	if err := addDiff(base, prev.Commit); err != nil {
		return nil, err
	}
	if err := addDiff(base, targetSha); err != nil {
		return nil, err
	}
	return touched, nil
}

func (o *Options) indexFile(path paths.GitRootRelativePath, blobSha string, model embed.EmbeddingModel) error {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return err
	}
	// Code files are chunked along syntactic boundaries via tree-sitter (with a
	// line-based fallback); text/markdown files use the structural markdown
	// chunker.
	var chunks []chunk.ChunkInfo
	if o.route(path) == filetype.Code {
		grammar := o.grammarFor(path)
		var err error
		chunks, err = chunk.ChunkCode(content, grammar, string(path), chunk.TargetChunkSize)
		if err != nil {
			return err
		}
	} else {
		chunks = chunk.ChunkMarkdown(string(content), chunk.TargetChunkSize)
	}

	contextualized := make([]string, len(chunks))
	for i, c := range chunks {
		if c.HeadingContext != "" {
			contextualized[i] = fmt.Sprintf("<context>\n%s\n</context>\n\n%s", c.HeadingContext, c.Text)
		} else {
			contextualized[i] = c.Text
		}
	}

	var embeddings []embed.Embedding
	if len(contextualized) > 0 {
		embeddings, err = model.EmbedChunks(contextualized)
		if err != nil {
			return err
		}
	}

	return o.Store.PutFile(string(path), model.ModelName(), blobSha, chunks, contextualized, embeddings)
}

// indexedEntry records which model embedded a path and the stored blob sha.
type indexedEntry struct {
	model string
	sha   string
}

// Search embeds the query with every active model, queries each model's vec
// table, and merges results by descending score (truncated to topK).
func Search(o *Options, query string, topK int) ([]store.SearchResult, error) {
	var all []store.SearchResult
	for _, m := range o.activeModels() {
		qe, err := m.EmbedQuery(query)
		if err != nil {
			return nil, err
		}
		res, err := o.Store.Search(m.ModelName(), qe, topK)
		if err != nil {
			return nil, err
		}
		all = append(all, res...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if topK > 0 && len(all) > topK {
		all = all[:topK]
	}
	return all, nil
}

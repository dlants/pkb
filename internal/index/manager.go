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
	"strings"
	"time"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/filetype"
	"github.com/dlants/pkb/internal/git"
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
func (i *Ignore) Match(relPath string) bool {
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
	Repo   *git.Repo
	Store  *store.Store
	Model  embed.EmbeddingModel
	Ref    string
	Ignore *Ignore
}

// textExts is the Stage 1 allowlist of indexable text extensions. Stage 2
// introduces full file-type routing and code support.
var textExts = map[string]struct{}{
	".md":       {},
	".markdown": {},
	".txt":      {},
}

// candidate reports whether a path should be indexed (Stage 1: markdown/text
// files only, never the .pkb state dir, not ignored).
func (o *Options) candidate(path string) bool {
	if path == ".pkbignore" || strings.HasPrefix(path, ".pkb/") {
		return false
	}
	if o.Ignore != nil && o.Ignore.Match(path) {
		return false
	}
	if filetype.RoutePath(path).Type != filetype.Text {
		return false
	}
	_, ok := textExts[strings.ToLower(filepath.Ext(path))]
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
	repoRoot := o.Repo.Root

	targetSha, err := o.Repo.ResolveRef(ref)
	if err != nil {
		return State{}, err
	}

	if err := o.Store.EnsureVecTable(o.Model.ModelName(), o.Model.Dimensions()); err != nil {
		return State{}, err
	}

	indexed, err := o.Store.IndexedFiles(o.Model.ModelName())
	if err != nil {
		return State{}, err
	}

	treeFiles, err := o.Repo.LsTree(ref)
	if err != nil {
		return State{}, err
	}
	treeMap := make(map[string]string, len(treeFiles))
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
		if inTree && o.candidate(path) {
			if indexed[path] == blobSha {
				continue // content unchanged; skip embed
			}
			if err := o.indexFile(path, blobSha); err != nil {
				return State{}, err
			}
		} else {
			if _, ok := indexed[path]; ok {
				if err := o.Store.DeleteFile(path, o.Model.ModelName()); err != nil {
					return State{}, err
				}
			}
		}
	}

	stats, err := o.Store.Stats(o.Model.ModelName())
	if err != nil {
		return State{}, err
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
func (o *Options) touchedPaths(prev *State, targetSha string, treeMap, indexed map[string]string) (map[string]struct{}, error) {
	touched := map[string]struct{}{}

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

func (o *Options) indexFile(path, blobSha string) error {
	content, err := os.ReadFile(filepath.Join(o.Repo.Root, path))
	if err != nil {
		return err
	}
	chunks := chunk.ChunkMarkdown(string(content), chunk.TargetChunkSize)

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
		embeddings, err = o.Model.EmbedChunks(contextualized)
		if err != nil {
			return err
		}
	}

	return o.Store.PutFile(path, o.Model.ModelName(), blobSha, chunks, contextualized, embeddings)
}

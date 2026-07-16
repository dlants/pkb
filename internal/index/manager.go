// Package index implements the reindex flow: it lists the target tree (HEAD by
// default) and compares each file's git blob sha against the blob shas already
// recorded in the store, then indexes/updates/deletes only the files that
// differ. There is no watcher; reindex runs to completion and exits.
package index

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/cost"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/filetype"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/mirror"
	"github.com/dlants/pkb/internal/paths"
	"github.com/dlants/pkb/internal/store"
)

// State is the persisted marker recording how far indexing has progressed.
type State struct {
	Commit     string `toml:"commit"`
	FileCount  int    `toml:"fileCount"`
	ChunkCount int    `toml:"chunkCount"`
}

const statePath = "pkb-state.toml"

// Ignore matches paths against the configured exclude globs using simple
// segment/prefix matching (full gitignore semantics are out of scope for v1).
type Ignore struct {
	patterns []string
}

// NewIgnore builds an Ignore from the configured exclude patterns. Blank lines
// and leading/trailing whitespace or trailing slashes are normalized away.
func NewIgnore(patterns []string) *Ignore {
	var cleaned []string
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		cleaned = append(cleaned, strings.TrimRight(p, "/"))
	}
	return &Ignore{patterns: cleaned}
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
	Ignore *Ignore
	// ExtOverrides forces a file extension to a file type ("code"/"text").
	ExtOverrides map[string]string
	// MaxReindexCost caps the projected dollar cost of a single run (a
	// per-run cap, not a cumulative spend limit). Reindex estimates the cost
	// before any paid work and aborts when it exceeds MaxReindexCost. A
	// non-positive value disables the gate.
	MaxReindexCost float64
	// Staged, when true, indexes the staging area (git write-tree) rather than
	// HEAD, so a pre-commit hook can index not-yet-committed content. The staged
	// blob shas equal the blob shas the commit will contain, so a subsequent
	// post-commit reindex is a no-op.
	Staged bool
}

// contextualModel returns the embedding model as a ContextualEmbeddingModel.
// The second result reports whether the model supports the contextual
// whole-document path (all text files use it).
func (o *Options) contextualModel() (embed.ContextualEmbeddingModel, bool) {
	cm, ok := o.Model.(embed.ContextualEmbeddingModel)
	return cm, ok
}

// isContextual reports whether a path is embedded via the whole-document
// auto-chunk path: the contextual text path is active and the file is not code.
func (o *Options) isContextual(path paths.GitRootRelativePath) bool {
	_, ok := o.contextualModel()
	return ok && o.route(path) != filetype.Code
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
	if strings.HasPrefix(string(path), ".pkb/") {
		return false
	}
	if string(path) == statePath {
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
	if err := toml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func writeState(repoRoot string, s State) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(s); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(repoRoot, statePath), buf.Bytes(), 0o644)
}

// Reindex brings the index in sync with HEAD and, only on success, advances
// the marker.
func Reindex(o *Options) (State, error) {
	repoRoot := string(o.Repo.Root)

	treeRef := "HEAD"
	if o.Staged {
		treeSha, err := o.Repo.WriteTree()
		if err != nil {
			return State{}, err
		}
		treeRef = treeSha
	}

	// targetSha is recorded in the state marker for human-facing output only;
	// correctness comes from the per-file blob shas. For a staged run it is the
	// parent HEAD commit (or empty when indexing the very first commit).
	targetSha, err := o.Repo.ResolveRef("HEAD")
	if err != nil {
		if !o.Staged {
			return State{}, err
		}
		targetSha = ""
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
	// its stored blob sha, read from the mirror tree (the source of truth).
	indexed, err := o.treeIndexed(models)
	if err != nil {
		return State{}, err
	}

	treeFiles, err := o.Repo.LsTree(treeRef)
	if err != nil {
		return State{}, err
	}
	treeMap := make(map[paths.GitRootRelativePath]string, len(treeFiles))
	for _, f := range treeFiles {
		treeMap[f.Path] = f.BlobSha
	}

	touched := o.touchedPaths(treeMap, indexed)

	est, err := o.estimate(touched, treeMap, indexed, false)
	if err != nil {
		return State{}, err
	}
	fmt.Fprintf(os.Stderr, "estimated reindex cost: $%.2f (%d files, %d chunks, ~%d embed tokens)\n",
		est.dollars, est.files, est.chunks, est.embedTokens)
	if o.MaxReindexCost > 0 && est.dollars > o.MaxReindexCost {
		return State{}, fmt.Errorf("estimated reindex cost $%.2f exceeds max reindex cost $%.2f (%d files, %d chunks, ~%d embed tokens); reindex locally instead",
			est.dollars, o.MaxReindexCost, est.files, est.chunks, est.embedTokens)
	}

	var (
		filesIndexed  int
		chunksIndexed int
		charsEmbedded int
		embedTime     time.Duration
		indexStart    = time.Now()
		lastProgress  = time.Now()
	)
	const (
		charsPerToken    = 3 // rough approximation for tok/s reporting
		verboseFileLimit = 50
		progressInterval = time.Minute
	)
	// maxBatchChars bounds a single embedding request. Voyage caps a request at
	// 120000 tokens across the whole batch; at ~3 chars/token that is ~360000
	// chars, so we keep a margin. Chunks are aggregated across files into one
	// batch (the single level of batching); a file is written only once all of
	// its chunks have been embedded.
	const maxBatchChars = 250000
	var (
		pending    []*preparedFile
		batchTexts []string
		batchRefs  []embedRef
		batchChars int
	)
	report := func(pf *preparedFile) {
		filesIndexed++
		chunksIndexed += len(pf.chunks)
		charsEmbedded += pf.chars
		elapsed := time.Since(indexStart).Seconds()
		embedTokPerSec := 0.0
		if s := embedTime.Seconds(); s > 0 {
			embedTokPerSec = float64(charsEmbedded) / charsPerToken / s
		}
		if filesIndexed <= verboseFileLimit {
			fmt.Fprintf(os.Stderr, "indexed %s (%d chunks) | %d files, %d chunks, %.1f chunks/sec, embed ~%.0f tok/sec\n",
				pf.path, len(pf.chunks), filesIndexed, chunksIndexed, float64(chunksIndexed)/elapsed, embedTokPerSec)
			if filesIndexed == verboseFileLimit {
				fmt.Fprintf(os.Stderr, "... suppressing per-file output; reporting high-level progress every %s\n", progressInterval)
			}
		} else if time.Since(lastProgress) >= progressInterval {
			lastProgress = time.Now()
			fmt.Fprintf(os.Stderr, "progress: %d/%d files indexed, %d chunks, %.1f chunks/sec, embed ~%.0f tok/sec\n",
				filesIndexed, est.files, chunksIndexed, float64(chunksIndexed)/elapsed, embedTokPerSec)
		}
	}
	flush := func() error {
		if len(batchTexts) == 0 {
			return nil
		}
		embedStart := time.Now()
		fresh, err := o.Model.EmbedChunks(batchTexts)
		if err != nil {
			return err
		}
		embedTime += time.Since(embedStart)
		for j, ref := range batchRefs {
			ref.pf.embeddings[ref.idx] = fresh[j]
			ref.pf.remaining--
		}
		batchTexts = nil
		batchRefs = nil
		batchChars = 0
		// Write every file whose chunks are now all embedded, in original order.
		kept := pending[:0]
		for _, pf := range pending {
			if pf.remaining > 0 {
				kept = append(kept, pf)
				continue
			}
			if err := o.writeFile(pf, o.Model); err != nil {
				return err
			}
			report(pf)
		}
		pending = kept
		return nil
	}
	for path := range touched {
		blobSha, inTree := treeMap[path]
		prevEntry, wasIndexed := indexed[path]
		if inTree && o.candidate(path) {
			model := o.Model
			// Skip a file only when it was indexed by the same embedding model
			// against the same blob. Because each file is written in a single
			// transaction, a recorded file row always reflects a fully indexed
			// file.
			if wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha {
				continue
			}
			// A different model previously embedded this path (e.g. routing
			// changed): writing the new artifact below overwrites the old one, so
			// no explicit purge is needed here (syncCache reconciles the cache).
			if cm, ok := o.contextualModel(); ok && o.isContextual(path) {
				pf, err := o.prepareContextualFile(path, blobSha, cm)
				if err != nil {
					return State{}, err
				}
				if err := o.writeFile(pf, model); err != nil {
					return State{}, err
				}
				report(pf)
				continue
			}
			pf, err := o.prepareFile(path, blobSha, model)
			if err != nil {
				return State{}, err
			}
			if len(pf.embedIdx) == 0 {
				// Fully reused: nothing to embed, write immediately.
				if err := o.writeFile(pf, model); err != nil {
					return State{}, err
				}
				report(pf)
				continue
			}
			pending = append(pending, pf)
			for _, idx := range pf.embedIdx {
				batchTexts = append(batchTexts, pf.contextualized[idx])
				batchRefs = append(batchRefs, embedRef{pf: pf, idx: idx})
				batchChars += len(pf.contextualized[idx])
				if batchChars >= maxBatchChars {
					if err := flush(); err != nil {
						return State{}, err
					}
				}
			}
		} else {
			if wasIndexed {
				if err := o.mirrorTree().Delete(path); err != nil {
					return State{}, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return State{}, err
	}

	// The mirror tree is now the up-to-date source of truth; refresh the derived
	// SQLite cache from it so stats/state counts and queries stay correct.
	if err := o.syncCache(models); err != nil {
		return State{}, err
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
		FileCount:  stats.Files,
		ChunkCount: stats.Chunks,
	}
	if err := writeState(repoRoot, st); err != nil {
		return State{}, err
	}
	if err := o.Store.Vacuum(); err != nil {
		return State{}, err
	}
	return st, nil
}

// CostEstimate is a read-only projection of a reindex run's cost: how many
// files/chunks need fresh work and the token/dollar totals that work is
// expected to incur. It is the public form of the internal costEstimate.
type CostEstimate struct {
	Files       int
	Chunks      int
	EmbedTokens int
	Dollars     float64
}

// Estimate projects the dollar cost of a reindex without performing any paid
// embedding/inference work or mutating index data. When full is true it
// projects a from-scratch reindex of the entire repository, ignoring per-chunk
// reuse so every chunk is charged; otherwise it projects the next incremental
// reindex against HEAD, mirroring Reindex's skip and reuse decisions exactly.
func Estimate(o *Options, full bool) (CostEstimate, error) {
	ref := "HEAD"

	models := o.activeModels()
	for _, m := range models {
		if err := o.Store.EnsureVecTable(m.ModelName(), m.Dimensions()); err != nil {
			return CostEstimate{}, err
		}
	}

	indexed, err := o.treeIndexed(models)
	if err != nil {
		return CostEstimate{}, err
	}

	treeFiles, err := o.Repo.LsTree(ref)
	if err != nil {
		return CostEstimate{}, err
	}
	treeMap := make(map[paths.GitRootRelativePath]string, len(treeFiles))
	for _, f := range treeFiles {
		treeMap[f.Path] = f.BlobSha
	}

	var touched map[paths.GitRootRelativePath]struct{}
	if full {
		touched = map[paths.GitRootRelativePath]struct{}{}
		for path := range treeMap {
			if o.candidate(path) {
				touched[path] = struct{}{}
			}
		}
	} else {
		touched = o.touchedPaths(treeMap, indexed)
	}

	est, err := o.estimate(touched, treeMap, indexed, full)
	if err != nil {
		return CostEstimate{}, err
	}
	return CostEstimate{
		Files:       est.files,
		Chunks:      est.chunks,
		EmbedTokens: est.embedTokens,
		Dollars:     est.dollars,
	}, nil
}

// touchedPaths computes the set of paths that might need work by comparing the
// target tree's blob shas against the blob shas already recorded in the store.
// A path is touched when it is a new/modified candidate (absent from the index
// or with a differing blob sha) or a deletion (indexed but no longer in the
// tree). This is derived purely from the
// tree and the stored blob shas, so it is correct regardless of commit history
// shape and needs no commit inputs. A same-content embedding-model swap falls
// out naturally: the new model has no stored blob shas, so every tree candidate
// differs and is re-embedded.
func (o *Options) touchedPaths(treeMap map[paths.GitRootRelativePath]string, indexed map[paths.GitRootRelativePath]indexedEntry) map[paths.GitRootRelativePath]struct{} {
	touched := map[paths.GitRootRelativePath]struct{}{}

	for path, blobSha := range treeMap {
		if !o.candidate(path) {
			continue
		}
		entry, wasIndexed := indexed[path]
		if !wasIndexed || entry.sha != blobSha {
			touched[path] = struct{}{}
		}
	}

	// Deletions: a path recorded in the store but absent from the target tree.
	for path := range indexed {
		if _, inTree := treeMap[path]; !inTree {
			touched[path] = struct{}{}
		}
	}

	return touched
}

// costEstimate summarizes a projected reindex run: how many files/chunks need
// fresh work and the token/dollar totals that work is expected to cost. It is
// computed with no API calls and no DB mutation.
type costEstimate struct {
	files        int
	chunks       int
	embedTokens  int
	embedDollars float64
	dollars      float64
}

// estimate projects the dollar cost of indexing the touched set, counting only
// work that will actually be paid for. It mirrors Reindex's skip decision and
// per-chunk reuse so reuse hits are never charged: a file fully indexed by the
// same model against the same blob is skipped, and within a reindexed file only
// reuse-miss chunks contribute embedding tokens. No network or DB writes occur.
func (o *Options) estimate(touched map[paths.GitRootRelativePath]struct{}, treeMap map[paths.GitRootRelativePath]string, indexed map[paths.GitRootRelativePath]indexedEntry, fromScratch bool) (costEstimate, error) {
	var est costEstimate
	model := o.Model
	for path := range touched {
		blobSha, inTree := treeMap[path]
		if !inTree || !o.candidate(path) {
			continue
		}
		prevEntry, wasIndexed := indexed[path]
		if !fromScratch && wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha {
			continue
		}
		// Text files are sent whole to the contextual endpoint. Project their
		// embedding tokens from the whole-file length.
		if o.isContextual(path) {
			content, err := os.ReadFile(o.Repo.Root.Join(path).String())
			if err != nil {
				return costEstimate{}, err
			}
			est.files++
			est.chunks++
			est.embedTokens += len(content) / cost.CharsPerToken
			continue
		}
		content, err := os.ReadFile(o.Repo.Root.Join(path).String())
		if err != nil {
			return costEstimate{}, err
		}
		chunks, err := o.chunkFile(path, content)
		if err != nil {
			return costEstimate{}, err
		}
		// A model change purges the old rows, so reuse is keyed on the new
		// model's name -- an empty map there, correctly charging every chunk. A
		// from-scratch projection ignores reuse entirely and charges every chunk.
		var existing map[string]embed.Embedding
		if !fromScratch {
			existing, err = o.reuseMap(path, model.ModelName())
			if err != nil {
				return costEstimate{}, err
			}
		}
		est.files++
		for _, c := range chunks {
			if _, ok := existing[store.ChunkKey(c.HeadingContext, c.Text)]; ok {
				continue
			}
			est.chunks++
			est.embedTokens += len(Contextualize(filetype.LineComment(string(path)), c.HeadingContext, c.Text)) / cost.CharsPerToken
		}
	}
	est.embedDollars = float64(est.embedTokens) * cost.EmbeddingPricePerToken(model.ModelName())
	est.dollars = est.embedDollars
	return est, nil
}

// preparedFile holds everything needed to persist one file except the freshly
// computed embeddings: chunking and reuse resolution are all done
// up front, and embedIdx lists the chunk indices still awaiting embedding.
// remaining counts how many of those are not yet filled in (decremented as
// batches flush); the file is written only when it reaches zero.
type preparedFile struct {
	path           paths.GitRootRelativePath
	blobSha        string
	chunks         []chunk.ChunkInfo
	contextualized []string
	embeddings     []embed.Embedding
	embedIdx       []int
	remaining      int
	chars          int
}

// embedRef points a slot in a cross-file embedding batch back to the chunk it
// belongs to so freshly returned vectors can be scattered into place.
type embedRef struct {
	pf  *preparedFile
	idx int
}

// chunkFile chunks a file's content along the appropriate boundaries: code
// files via tree-sitter (with a line-based fallback) or the config chunker;
// text/markdown files via the structural markdown chunker. It is shared by the
// real index path and the cost estimator so both see identical chunking.
func (o *Options) chunkFile(path paths.GitRootRelativePath, content []byte) ([]chunk.ChunkInfo, error) {
	if o.route(path) == filetype.Code {
		grammar := o.grammarFor(path)
		if chunk.IsConfigGrammar(grammar) {
			return chunk.ChunkConfig(content, grammar, string(path), chunk.TargetChunkSize)
		}
		return chunk.ChunkCode(content, grammar, string(path), chunk.TargetChunkSize)
	}
	return chunk.ChunkMarkdown(string(content), string(path), chunk.TargetChunkSize), nil
}

// mirrorTree returns the on-disk mirror tree rooted at the repo root. The tree
// is the source of truth for the index; the store is a derived cache.
func (o *Options) mirrorTree() *mirror.Tree {
	return mirror.NewTree(o.Repo.Root)
}

// treeIndexed enumerates the mirror tree as the set of already-indexed paths,
// mapping each to the model that embedded it and its recorded blob sha. It
// replaces Store.IndexedFiles as the reindex/estimate/healthcheck source of
// truth.
func (o *Options) treeIndexed(models []embed.EmbeddingModel) (map[paths.GitRootRelativePath]indexedEntry, error) {
	active := map[string]struct{}{}
	for _, m := range models {
		active[m.ModelName()] = struct{}{}
	}
	entries, err := o.mirrorTree().List()
	if err != nil {
		return nil, err
	}
	out := make(map[paths.GitRootRelativePath]indexedEntry, len(entries))
	for path, e := range entries {
		// Only artifacts embedded by an active model count as "indexed"; an
		// artifact left by a since-swapped model reads as absent, so the path is
		// treated as new and re-embedded under the active model.
		if _, ok := active[e.ModelName]; !ok {
			continue
		}
		out[path] = indexedEntry{model: e.ModelName, sha: e.BlobSha}
	}
	return out, nil
}

// reuseMap builds the per-chunk reuse map (ChunkKey -> embedding) for a path
// from its existing mirror artifact, replacing Store.ChunkEmbeddings. An
// artifact embedded by a different model, or a missing/torn artifact, yields an
// empty map so every chunk is re-embedded.
func (o *Options) reuseMap(path paths.GitRootRelativePath, modelName string) (map[string]embed.Embedding, error) {
	a, ok, err := o.mirrorTree().TryRead(path)
	if err != nil {
		return nil, err
	}
	if !ok || a.ModelName != modelName {
		return map[string]embed.Embedding{}, nil
	}
	out := make(map[string]embed.Embedding, len(a.Chunks))
	for _, c := range a.Chunks {
		out[store.ChunkKey(c.Info.HeadingContext, c.Info.Text)] = c.Embedding
	}
	return out, nil
}

// syncCache reconciles the derived SQLite store against the mirror tree: it
// (re)writes any artifact whose blob sha or model differs from the cache and
// evicts cache rows for artifacts no longer in the tree. It is the single load
// path the reindex writer and (later) the query readers share.
func (o *Options) syncCache(models []embed.EmbeddingModel) error {
	entries, err := o.mirrorTree().List()
	if err != nil {
		return err
	}
	cached := map[paths.GitRootRelativePath]indexedEntry{}
	for _, m := range models {
		files, err := o.Store.IndexedFiles(m.ModelName())
		if err != nil {
			return err
		}
		for path, meta := range files {
			cached[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha}
		}
	}

	for path, e := range entries {
		cur, ok := cached[path]
		if ok && cur.model == e.ModelName && cur.sha == e.BlobSha {
			continue
		}
		if ok && cur.model != e.ModelName {
			if err := o.Store.DeleteFile(string(path), cur.model); err != nil {
				return err
			}
		}
		a, present, err := o.mirrorTree().TryRead(path)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		chunks := make([]chunk.ChunkInfo, len(a.Chunks))
		contextualized := make([]string, len(a.Chunks))
		embeddings := make([]embed.Embedding, len(a.Chunks))
		for i, c := range a.Chunks {
			chunks[i] = c.Info
			contextualized[i] = c.ContextualizedText
			embeddings[i] = c.Embedding
		}
		if err := o.Store.PutFile(string(path), a.ModelName, a.BlobSha, chunks, contextualized, embeddings); err != nil {
			return err
		}
	}

	for path, cur := range cached {
		if _, ok := entries[path]; !ok {
			if err := o.Store.DeleteFile(string(path), cur.model); err != nil {
				return err
			}
		}
	}
	return nil
}

// prepareFile does all per-file work except the embedding call and the database
// write: it chunks the file, resolves reuse, and builds the contextualized text
// for every chunk. The returned preparedFile carries reused vectors already in
// place and embedIdx listing the chunks that still need embedding (in a
// cross-file batch).
func (o *Options) prepareFile(path paths.GitRootRelativePath, blobSha string, model embed.EmbeddingModel) (*preparedFile, error) {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return nil, err
	}
	chunks, err := o.chunkFile(path, content)
	if err != nil {
		return nil, err
	}

	comment := filetype.LineComment(string(path))

	// Load the committed-generation reuse map keyed on ChunkKey (heading
	// breadcrumb + raw text) from the file's existing mirror artifact, so an
	// unchanged chunk keeps its embedding even when the file changed elsewhere.
	existing, err := o.reuseMap(path, model.ModelName())
	if err != nil {
		return nil, err
	}

	// Resolve, per chunk, whether it is a reuse hit and carry over its stored
	// vector; misses are re-embedded.
	reuse := make([]bool, len(chunks))
	reuseEmb := make([]embed.Embedding, len(chunks))
	for i, c := range chunks {
		if emb, ok := existing[store.ChunkKey(c.HeadingContext, c.Text)]; ok {
			reuse[i] = true
			reuseEmb[i] = emb
		}
	}

	// Build the embedded text for every chunk from its heading breadcrumb.
	// Reused vectors are carried over in place; reuse misses are recorded in
	// embedIdx to be embedded later in a cross-file batch.
	pf := compactPrepared(path, comment, chunks, reuse, reuseEmb)
	pf.blobSha = blobSha
	return pf, nil
}

// Auto-chunk windowing: a text file's estimated token count (chars/charsPerAutoChunkToken)
// is compared against autoChunkTokenLimit. Files at or under the limit are sent
// whole in one EmbedDocument call; larger files are split into overlapping
// windows so no chunk boundary is lost across a split.
const (
	charsPerAutoChunkToken = 5
	autoChunkTokenLimit    = 120000
	autoChunkOverlapTokens = 10000
	autoChunkMaxWindowByte = autoChunkTokenLimit * charsPerAutoChunkToken
	autoChunkOverlapByte   = autoChunkOverlapTokens * charsPerAutoChunkToken
)

// prepareContextualFile builds a preparedFile for a text file via the model's
// whole-document auto-chunking endpoint. PKB does not chunk the file; instead
// the model returns its own chunks, each with a contextualized vector. Chunks
// are file-tagged (no line ranges). Large files are sent in overlapping windows
// and the resulting chunks are deduped by text identity. Per-chunk reuse does
// not apply here; unchanged files are skipped wholesale by the caller's
// blob_sha check.
func (o *Options) prepareContextualFile(path paths.GitRootRelativePath, blobSha string, cm embed.ContextualEmbeddingModel) (*preparedFile, error) {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return nil, err
	}
	windows := autoChunkWindows(string(content))
	pf := &preparedFile{path: path, blobSha: blobSha}
	seen := map[string]struct{}{}
	for _, w := range windows {
		chunks, err := cm.EmbedDocument(w)
		if err != nil {
			return nil, err
		}
		for _, c := range chunks {
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			if _, dup := seen[text]; dup {
				continue
			}
			seen[text] = struct{}{}
			pf.chunks = append(pf.chunks, chunk.ChunkInfo{Text: text})
			pf.contextualized = append(pf.contextualized, text)
			pf.embeddings = append(pf.embeddings, c.Embedding)
			pf.chars += len(text)
		}
	}
	return pf, nil
}

// autoChunkWindows splits a document into byte windows for whole-document
// embedding. A document at or under the window size is returned as a single
// window; larger documents are split into windows of autoChunkMaxWindowByte
// that overlap by autoChunkOverlapByte so no content falls between two windows.
func autoChunkWindows(document string) []string {
	if len(document) <= autoChunkMaxWindowByte {
		return []string{document}
	}
	var windows []string
	step := autoChunkMaxWindowByte - autoChunkOverlapByte
	for start := 0; start < len(document); start += step {
		end := start + autoChunkMaxWindowByte
		if end > len(document) {
			end = len(document)
		}
		windows = append(windows, document[start:end])
		if end == len(document) {
			break
		}
	}
	return windows
}

// compactPrepared builds the embedded text for every chunk and assembles the
// parallel slices that make up a preparedFile, dropping any "zero-signal" chunk
// whose contextualized text is empty or whitespace-only. Such a chunk carries no
// retrievable content and, if embedded, would send an empty string to the
// embedder -- which some providers (e.g. Bedrock Cohere embed-v4) reject with a
// ValidationException. Dropping it here removes both the chunk row and its
// vector together, so the embedding count stays aligned with what PutFile
// writes. Reused vectors are carried over in place; reuse misses are recorded in
// embedIdx (as indices into the compacted slices) to be embedded later.
func compactPrepared(path paths.GitRootRelativePath, comment string, chunks []chunk.ChunkInfo, reuse []bool, reuseEmb []embed.Embedding) *preparedFile {
	pf := &preparedFile{path: path}
	for i, c := range chunks {
		ctx := Contextualize(comment, c.HeadingContext, c.Text)
		if strings.TrimSpace(ctx) == "" {
			fmt.Fprintf(os.Stderr, "warning: skipping empty chunk %d in %s (no embeddable content)\n", i, path)
			continue
		}
		j := len(pf.chunks)
		pf.chunks = append(pf.chunks, c)
		pf.contextualized = append(pf.contextualized, ctx)
		if reuse[i] {
			pf.embeddings = append(pf.embeddings, reuseEmb[i])
			continue
		}
		pf.embeddings = append(pf.embeddings, nil)
		pf.chars += len(ctx)
		pf.embedIdx = append(pf.embedIdx, j)
	}
	pf.remaining = len(pf.embedIdx)
	return pf
}

// writeFile persists a fully embedded file in a single transaction. The
// expensive embedding work is already done, so the write is quick. A crash
// before it commits leaves the previously indexed state intact, and the file is
// simply reindexed on the next run (reusing unchanged chunks via
// ChunkEmbeddings).
func (o *Options) writeFile(pf *preparedFile, model embed.EmbeddingModel) error {
	a := mirror.Artifact{
		BlobSha:   pf.blobSha,
		ModelName: model.ModelName(),
		Chunks:    make([]mirror.Chunk, len(pf.chunks)),
	}
	for i := range pf.chunks {
		a.Chunks[i] = mirror.Chunk{
			Info:               pf.chunks[i],
			ContextualizedText: pf.contextualized[i],
			Embedding:          pf.embeddings[i],
		}
	}
	return o.mirrorTree().Write(pf.path, a)
}

// Contextualize builds the text actually embedded for a chunk: the raw chunk
// text prefixed by its heading breadcrumb. When the file has a known
// line-comment prefix the metadata is rendered as zero-indented comments in the
// file's own language, which keeps the embedded text in distribution for code
// embedding models; the chunk text itself is left untouched so its original
// indentation is preserved. Files without a comment prefix (markdown/plaintext)
// fall back to <context>...</context> blocks. Empty components are omitted.
func Contextualize(comment, headingContext, text string) string {
	s := text
	if headingContext != "" {
		s = metaBlock(comment, headingContext) + "\n\n" + s
	}
	return s
}

// metaBlock renders a metadata string as a zero-indented comment block using the
// given line-comment prefix, one prefix per line. With an empty prefix it falls
// back to a <context>...</context> block.
func metaBlock(comment, s string) string {
	if comment == "" {
		return fmt.Sprintf("<context>\n%s\n</context>", s)
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = comment + " " + l
	}
	return strings.Join(lines, "\n")
}

// indexedEntry records which model embedded a path and the stored blob sha.
type indexedEntry struct {
	model string
	sha   string
}

// HealthIssue is a single discrepancy found by Healthcheck. Path is the
// repo-relative file the issue concerns, or empty for index-wide issues.
type HealthIssue struct {
	Path string
	Msg  string
}

// HealthReport is the result of Healthcheck: the commits compared, the headline
// counts, and every discrepancy found between the git tree, the SQLite index,
// and the state marker. It is healthy exactly when Issues is empty.
type HealthReport struct {
	HeadCommit    string
	StateCommit   string
	StateMissing  bool
	ExpectedFiles int
	IndexedFiles  int
	IndexedChunks int
	Issues        []HealthIssue
}

func (r *HealthReport) addf(path, format string, args ...any) {
	r.Issues = append(r.Issues, HealthIssue{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// Healthcheck verifies the SQLite index and the state marker against the git
// tree at HEAD without mutating anything. It checks that every expected file
// (an indexable, non-ignored path in the tree) is indexed with the matching
// blob sha, that no stale files linger in the index, and that the state
// marker's file/chunk counts agree with the index. Blob-sha coverage -- not the
// recorded commit -- is the source of correctness, so a mismatched marker commit
// (e.g. after an amend, rebase, or a staged/pre-commit index) is not flagged.
func Healthcheck(o *Options) (HealthReport, error) {
	repoRoot := string(o.Repo.Root)
	var rep HealthReport

	targetSha, err := o.Repo.ResolveRef("HEAD")
	if err != nil {
		return rep, err
	}
	rep.HeadCommit = targetSha

	prev, err := readState(repoRoot)
	if err != nil {
		return rep, err
	}
	if prev == nil {
		rep.StateMissing = true
		rep.addf("", "no state marker (%s); run `pkb reindex`", statePath)
		return rep, nil
	}
	rep.StateCommit = prev.Commit

	treeFiles, err := o.Repo.LsTree("HEAD")
	if err != nil {
		return rep, err
	}
	expected := make(map[paths.GitRootRelativePath]string, len(treeFiles))
	for _, f := range treeFiles {
		if o.candidate(f.Path) {
			expected[f.Path] = f.BlobSha
		}
	}
	rep.ExpectedFiles = len(expected)

	treeEntries, err := o.mirrorTree().List()
	if err != nil {
		return rep, err
	}
	active := map[string]struct{}{}
	for _, m := range o.activeModels() {
		active[m.ModelName()] = struct{}{}
	}
	indexed := make(map[paths.GitRootRelativePath]indexedEntry, len(treeEntries))
	for path, e := range treeEntries {
		if _, ok := active[e.ModelName]; !ok {
			continue
		}
		indexed[path] = indexedEntry{model: e.ModelName, sha: e.BlobSha}
		rep.IndexedFiles++
		rep.IndexedChunks += e.Chunks
	}

	for path, sha := range expected {
		e, ok := indexed[path]
		if !ok {
			rep.addf(string(path), "expected file is missing from the index")
			continue
		}
		if e.sha != sha {
			rep.addf(string(path), "stale blob: index has %s, tree has %s", e.sha, sha)
		}
	}
	for path := range indexed {
		if _, ok := expected[path]; !ok {
			rep.addf(string(path), "indexed but not an expected file (deleted or no longer indexable)")
		}
	}

	if rep.IndexedFiles != prev.FileCount {
		rep.addf("", "state fileCount %d does not match indexed file count %d", prev.FileCount, rep.IndexedFiles)
	}
	if rep.IndexedChunks != prev.ChunkCount {
		rep.addf("", "state chunkCount %d does not match indexed chunk count %d", prev.ChunkCount, rep.IndexedChunks)
	}

	sort.SliceStable(rep.Issues, func(i, j int) bool {
		if rep.Issues[i].Path != rep.Issues[j].Path {
			return rep.Issues[i].Path < rep.Issues[j].Path
		}
		return rep.Issues[i].Msg < rep.Issues[j].Msg
	})
	return rep, nil
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

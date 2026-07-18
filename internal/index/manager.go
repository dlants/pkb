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
// whole-document auto-chunk path (every file — code and text — uses it).
func (o *Options) contextualModel() (embed.ContextualEmbeddingModel, bool) {
	cm, ok := o.Model.(embed.ContextualEmbeddingModel)
	return cm, ok
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
		indexStart    = time.Now()
		lastProgress  = time.Now()
	)
	const (
		verboseFileLimit = 50
		progressInterval = time.Minute
	)
	report := func(pf *preparedFile) {
		filesIndexed++
		chunksIndexed += len(pf.chunks)
		elapsed := time.Since(indexStart).Seconds()
		if filesIndexed <= verboseFileLimit {
			fmt.Fprintf(os.Stderr, "indexed %s (%d chunks) | %d files, %d chunks, %.1f chunks/sec\n",
				pf.path, len(pf.chunks), filesIndexed, chunksIndexed, float64(chunksIndexed)/elapsed)
			if filesIndexed == verboseFileLimit {
				fmt.Fprintf(os.Stderr, "... suppressing per-file output; reporting high-level progress every %s\n", progressInterval)
			}
		} else if time.Since(lastProgress) >= progressInterval {
			lastProgress = time.Now()
			fmt.Fprintf(os.Stderr, "progress: %d/%d files indexed, %d chunks, %.1f chunks/sec\n",
				filesIndexed, est.files, chunksIndexed, float64(chunksIndexed)/elapsed)
		}
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
			if wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha && prevEntry.version == store.MajorVersion {
				continue
			}
			// Every file — code and text alike — is embedded via the model's
			// whole-document auto-chunk endpoint; a different model previously
			// embedding this path is handled by overwriting the artifact below
			// (syncCache reconciles the cache).
			cm, ok := o.contextualModel()
			if !ok {
				return State{}, fmt.Errorf("model %q does not support document auto-chunking required for indexing", model.ModelName())
			}
			pf, err := o.prepareFile(path, blobSha, cm)
			if err != nil {
				return State{}, err
			}
			if err := o.writeFile(pf, model); err != nil {
				return State{}, err
			}
			report(pf)
		} else {
			if wasIndexed {
				if err := o.mirrorTree().Delete(path); err != nil {
					return State{}, err
				}
			}
		}
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
		if !wasIndexed || entry.sha != blobSha || entry.version != store.MajorVersion {
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
		if !fromScratch && wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha && prevEntry.version == store.MajorVersion {
			continue
		}
		// Every file — code and text — is sent whole to the contextual
		// auto-chunk endpoint, so project its embedding tokens from the
		// whole-file length. Per-chunk reuse no longer applies; a file is either
		// skipped wholesale (same blob, above) or re-embedded whole. Size the
		// projection from the recorded blob (not the working tree) so the
		// estimate matches the content that will actually be embedded.
		content, err := o.Repo.CatBlob(blobSha)
		if err != nil {
			return costEstimate{}, err
		}
		est.files++
		est.chunks++
		est.embedTokens += len(content) / cost.CharsPerToken
	}
	est.embedDollars = float64(est.embedTokens) * cost.EmbeddingPricePerToken(model.ModelName())
	est.dollars = est.embedDollars
	return est, nil
}

// preparedFile holds everything needed to persist one file: the auto-chunk
// output already embedded, plus each chunk's verbatim byte span in the source.
type preparedFile struct {
	path       paths.GitRootRelativePath
	blobSha    string
	chunks     []chunk.ChunkInfo
	embeddings []embed.Embedding
	chars      int
	// spans[i] is the [start,end) byte offset of chunk i within blobSha's
	// content, recovered by locating the chunk text verbatim in the source.
	spans [][2]int
}

// resolveSpans locates each chunk's text verbatim within content, returning the
// [start,end) byte span of every chunk. It scans forward from a moving cursor so
// non-overlapping chunks resolve to precise, ordered offsets. A chunk whose text
// is not found at/after the cursor is a hard failure: offsets are load-bearing
// (the chunk text is reconstructed from them at sync), so a miss must surface
// immediately rather than persist a wrong span.
func resolveSpans(path paths.GitRootRelativePath, content []byte, chunks []chunk.ChunkInfo) ([][2]int, error) {
	spans := make([][2]int, len(chunks))
	cursor := 0
	for i, c := range chunks {
		start := -1
		// Prefer a match at/after the cursor so repeated chunk texts (e.g.
		// identical code lines) resolve to distinct, ordered spans.
		if rel := bytes.Index(content[cursor:], []byte(c.Text)); rel >= 0 {
			start = cursor + rel
			cursor = start + len(c.Text)
		} else if abs := bytes.Index(content, []byte(c.Text)); abs >= 0 {
			// Auto-chunk windowing can emit a chunk from an overlapping window
			// out of file order; fall back to a whole-file search without moving
			// the cursor backward.
			start = abs
		}
		if start < 0 {
			preview := c.Text
			if len(preview) > 80 {
				preview = preview[:80]
			}
			return nil, fmt.Errorf("index: cannot locate chunk %d of %s as a verbatim substring: %q", i, path, preview)
		}
		spans[i] = [2]int{start, start + len(c.Text)}
	}
	return spans, nil
}

// reconstructArtifact rebuilds the per-chunk cache rows (text, contextualized
// text, heading breadcrumb, and line/col positions) for an artifact from its
// stored byte offsets. It loads the artifact's exact blob from git (never the
// working tree) so offsets resolve against the content they were generated from,
// slices each chunk's text, and re-derives its contextualized form. Positions
// are computed for code (offset -> line/col) and left file-tagged (zero) for the
// auto-chunked text path, matching how each path was originally stored.
func (o *Options) reconstructArtifact(path paths.GitRootRelativePath, a mirror.Artifact) ([]chunk.ChunkInfo, []string, error) {
	content, err := o.Repo.CatBlob(a.BlobSha)
	if err != nil {
		return nil, nil, err
	}
	spans := make([]byteSpan, len(a.Chunks))
	for i, c := range a.Chunks {
		spans[i] = byteSpan{Start: c.Start, End: c.End}
	}
	recons, err := Reconstruct(path, content, spans)
	if err != nil {
		return nil, nil, err
	}
	chunks := make([]chunk.ChunkInfo, len(recons))
	contextualized := make([]string, len(recons))
	for i, rc := range recons {
		chunks[i] = chunk.ChunkInfo{
			Text:           rc.Text,
			HeadingContext: rc.HeadingContext,
			Start:          rc.Start,
			End:            rc.End,
		}
		contextualized[i] = rc.Contextualized
	}
	return chunks, contextualized, nil
}

// mirrorTree returns the on-disk mirror tree rooted at the repo root. The tree
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
		out[path] = indexedEntry{model: e.ModelName, sha: e.BlobSha, version: e.Version}
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
		chunks, contextualized, err := o.reconstructArtifact(path, a)
		if err != nil {
			return err
		}
		embeddings := make([]embed.Embedding, len(a.Chunks))
		for i, c := range a.Chunks {
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

// Auto-chunk windowing: a text file's estimated token count (chars/charsPerAutoChunkToken)
// is compared against autoChunkTokenLimit. Files at or under the limit are sent
// whole in one EmbedDocument call; larger files are split into overlapping
// windows so no chunk boundary is lost across a split.
const (
	// charsPerAutoChunkToken deliberately underestimates real chars-per-token
	// (English prose is ~4-5, dense code can be ~3.5) so the byte-sized window
	// stays comfortably under the endpoint's token cap. Underestimating makes
	// windows smaller and safer; the only cost is more requests for huge files.
	charsPerAutoChunkToken = 3
	// autoChunkTokenLimit is the per-window token budget. It sits below Voyage's
	// hard 120000-tokens-per-batch limit so a window never trips the cap even
	// when a window tokenizes denser than charsPerAutoChunkToken assumes.
	autoChunkTokenLimit    = 100000
	autoChunkOverlapTokens = 10000
	autoChunkMaxWindowByte = autoChunkTokenLimit * charsPerAutoChunkToken
	autoChunkOverlapByte   = autoChunkOverlapTokens * charsPerAutoChunkToken

	// autoChunkHeadTokens is the size of the head-of-file section prepended to
	// every window after the first, so the file's preamble (imports, module
	// docstring, frontmatter, top-level headings) contextualizes every body.
	autoChunkHeadTokens = 4000
	autoChunkHeadByte   = autoChunkHeadTokens * charsPerAutoChunkToken
	// autoChunkBreadcrumbReserveByte is the fixed allowance for the structural
	// breadcrumb part of the prefix. Head + breadcrumb reserve is subtracted from
	// the window cap to size later-window bodies, so body extents are
	// deterministic and independent of how long a given breadcrumb turns out to
	// be. A breadcrumb that overflows the reserve is a hard failure.
	autoChunkBreadcrumbReserveByte = 2000
	autoChunkPrefixReserveByte     = autoChunkHeadByte + autoChunkBreadcrumbReserveByte
	// autoChunkBodyBudget is the verbatim-body size for windows after the first;
	// the first window's body is the full autoChunkMaxWindowByte (it carries the
	// head already, so no prefix). Reserving a fixed prefix budget keeps the whole
	// transformed input under the per-request cap.
	autoChunkBodyBudget = autoChunkMaxWindowByte - autoChunkPrefixReserveByte
)

// prepareFile builds a preparedFile for any file — code or text — via the
// model's whole-document auto-chunking endpoint. PKB does not chunk the file;
// the model returns its own chunks, each with a contextualized vector. Each
// returned chunk is located within its window's transformed input and its span
// is mapped back to the raw blob via the window's affine toRaw bridge. Chunks
// touching the synthetic context prefix (head section or breadcrumb) have no raw
// preimage and are dropped; body chunks are asserted to slice byte-identically
// out of the blob. Chunks carry only offsets; text, line/col and breadcrumbs are
// reconstructed at sync. Large files are sent in overlapping windows and the
// resulting chunks are deduped by covered raw range (an overlap-region chunk
// maps to a range a neighbouring window already covered, so it collapses
// deterministically). Per-chunk reuse does not apply; unchanged files are
// skipped wholesale by the caller's blob_sha check.
func (o *Options) prepareFile(path paths.GitRootRelativePath, blobSha string, cm embed.ContextualEmbeddingModel) (*preparedFile, error) {
	// Embed and resolve spans against the exact blob recorded for this file,
	// never the working tree. The stored offsets are sliced back out of this
	// same blob at reconstruction time, so embedding a dirty working-tree copy
	// would produce offsets that are out of range for the committed blob.
	content, err := o.Repo.CatBlob(blobSha)
	if err != nil {
		return nil, err
	}
	windows, err := autoChunkWindows(path, string(content))
	if err != nil {
		return nil, err
	}
	pf := &preparedFile{path: path, blobSha: blobSha}
	var covered coveredRanges
	for _, w := range windows {
		chunks, err := cm.EmbedDocument(w.input)
		if err != nil {
			return nil, err
		}
		input := []byte(w.input)
		cursor := 0
		for i, c := range chunks {
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			// Locate the chunk within this window's transformed input with a
			// forward cursor, mirroring resolveSpans: prefer a match at/after the
			// cursor so repeated texts resolve to distinct, ordered spans, else
			// fall back to a whole-input search without moving the cursor.
			var t0 int
			if rel := bytes.Index(input[cursor:], []byte(text)); rel >= 0 {
				t0 = cursor + rel
				cursor = t0 + len(text)
			} else if abs := bytes.Index(input, []byte(text)); abs >= 0 {
				t0 = abs
			} else {
				preview := text
				if len(preview) > 80 {
					preview = preview[:80]
				}
				return nil, fmt.Errorf("index: cannot locate chunk %d of %s as a verbatim substring of the window input: %q", i, path, preview)
			}
			t1 := t0 + len(text)
			// Classify via the affine bridge: a chunk touching the synthetic
			// prefix (head section or breadcrumb) or straddling the prefix/body
			// boundary has no raw preimage and is dropped. Straddles are rare
			// (the separator makes them unlikely) and the same content is emitted
			// cleanly as a body chunk in the window whose body starts earlier.
			rawStart, ok0 := w.toRaw(InputOffset(t0))
			rawEnd, ok1 := w.toRaw(InputOffset(t1))
			if !ok0 || !ok1 {
				continue
			}
			// The affine map must be exact: the raw span slices byte-identically
			// to the chunk text, or a mapping bug slipped through.
			if string(content[rawStart:rawEnd]) != text {
				return nil, fmt.Errorf("index: chunk raw span [%d,%d) of %s does not slice to chunk text", rawStart, rawEnd, path)
			}
			// Dedup by covered raw range: an overlap-region chunk maps to a range
			// a neighbouring window already covered, so it is dropped regardless
			// of how the endpoint split the boundary.
			if covered.contains(rawStart, rawEnd) {
				continue
			}
			covered.add(rawStart, rawEnd)
			pf.chunks = append(pf.chunks, chunk.ChunkInfo{Text: text})
			pf.embeddings = append(pf.embeddings, c.Embedding)
			pf.chars += len(text)
			pf.spans = append(pf.spans, [2]int{int(rawStart), int(rawEnd)})
		}
	}
	return pf, nil
}

// coveredRanges is the set of raw [start,end) byte ranges already accepted for a
// file, kept as sorted, merged, non-overlapping intervals. It backs auto-chunk
// dedup: an overlap-region chunk whose range is already fully covered by an
// accepted chunk is dropped.
type coveredRanges struct {
	ivals [][2]mirror.RawOffset
}

// contains reports whether [start,end) lies entirely within the union of the
// accepted intervals.
func (c *coveredRanges) contains(start, end mirror.RawOffset) bool {
	for _, iv := range c.ivals {
		if iv[0] <= start && end <= iv[1] {
			return true
		}
	}
	return false
}

// add records [start,end) as covered, merging it into any intervals it overlaps
// or abuts so the set stays sorted, merged, and non-overlapping.
func (c *coveredRanges) add(start, end mirror.RawOffset) {
	merged := make([][2]mirror.RawOffset, 0, len(c.ivals)+1)
	for _, iv := range c.ivals {
		if iv[1] < start || end < iv[0] {
			merged = append(merged, iv)
			continue
		}
		if iv[0] < start {
			start = iv[0]
		}
		if iv[1] > end {
			end = iv[1]
		}
	}
	merged = append(merged, [2]mirror.RawOffset{start, end})
	sort.Slice(merged, func(i, j int) bool { return merged[i][0] < merged[j][0] })
	c.ivals = merged
}

// autoChunkWindows splits a document into whole-document embedding windows. A
// document at or under the window size is one prefix-less window. A larger
// document is split into windows whose bodies are verbatim, contiguous copies of
// the raw blob overlapping by autoChunkOverlapByte (so no chunk boundary is
// lost). The first window's body spans the head, so it carries no prefix and
// gets the full autoChunkMaxWindowByte budget. Every later window prepends a
// synthetic context prefix (the file's head section plus a structural
// breadcrumb locating the body in the file) and uses the reduced
// autoChunkBodyBudget; the prefix has no raw preimage and is dropped after
// embedding. The whole transformed input stays under the per-request cap because
// bodies are sized against the fixed autoChunkPrefixReserveByte and each real
// prefix is asserted to fit within that reserve.
func autoChunkWindows(path paths.GitRootRelativePath, document string) ([]autoChunkWindow, error) {
	if len(document) <= autoChunkMaxWindowByte {
		return []autoChunkWindow{{input: document}}, nil
	}

	route := filetype.RoutePath(string(path))
	comment := filetype.LineComment(string(path))
	isCode := route.Type == filetype.Code
	var coder *chunk.CodeBreadcrumber
	if isCode {
		var err error
		coder, err = chunk.NewCodeBreadcrumber([]byte(document), route.Grammar, string(path))
		if err != nil {
			return nil, fmt.Errorf("index: window %s: %w", path, err)
		}
	}

	head := headSection(document)

	// First window: full budget, no prefix (its body already contains the head).
	windows := []autoChunkWindow{{input: document[:autoChunkMaxWindowByte]}}

	step := autoChunkBodyBudget - autoChunkOverlapByte
	for bodyStart := autoChunkMaxWindowByte - autoChunkOverlapByte; bodyStart < len(document); bodyStart += step {
		bodyEnd := bodyStart + autoChunkBodyBudget
		if bodyEnd > len(document) {
			bodyEnd = len(document)
		}
		var breadcrumb string
		if isCode {
			breadcrumb = coder.Breadcrumb(bodyStart, bodyStart)
		} else {
			breadcrumb = chunk.MarkdownBreadcrumb(document, string(path), bodyStart)
		}
		prefix := head
		if breadcrumb != "" {
			prefix += "\n" + metaBlock(comment, breadcrumb)
		}
		prefix += "\n\n"
		if InputOffset(len(prefix)) > autoChunkPrefixReserveByte {
			return nil, fmt.Errorf("index: window %s: context prefix of %d bytes exceeds reserve %d", path, len(prefix), autoChunkPrefixReserveByte)
		}
		windows = append(windows, autoChunkWindow{
			input:     prefix + document[bodyStart:bodyEnd],
			prefixLen: InputOffset(len(prefix)),
			bodyStart: mirror.RawOffset(bodyStart),
		})
		if bodyEnd == len(document) {
			break
		}
	}
	return windows, nil
}

// headSection returns the file's leading bytes (at most autoChunkHeadByte),
// trimmed back to the last line boundary so it ends on a whole line. It is the
// preamble prepended to later windows to contextualize their bodies.
func headSection(document string) string {
	head := document
	if len(head) > autoChunkHeadByte {
		head = head[:autoChunkHeadByte]
	}
	if idx := strings.LastIndexByte(head, '\n'); idx >= 0 {
		head = head[:idx+1]
	}
	return head
}

// InputOffset is a byte offset into a window's transformed input string (the
// synthetic context prefix followed by the verbatim body). It is meaningful
// only within a single window and is never persisted. It is a defined type so
// the compiler refuses to mix it with a raw-blob mirror.RawOffset without an
// explicit conversion; the only sanctioned bridge between the two systems is
// autoChunkWindow.toRaw.
type InputOffset int

// autoChunkWindow is one whole-document embedding request. Its body,
// input[prefixLen:], is a verbatim contiguous copy of blob[bodyStart:...], so a
// transformed-input offset maps back to a raw-blob offset by a single affine
// shift. The synthetic context prefix, input[:prefixLen], has no raw preimage.
type autoChunkWindow struct {
	input     string           // transformed text sent to EmbedDocument
	prefixLen InputOffset      // bytes of synthetic context; body is input[prefixLen:]
	bodyStart mirror.RawOffset // raw blob offset where the body begins
}

// toRaw converts an InputOffset within this window to its RawOffset in the
// source blob, reporting false when the offset lies inside the synthetic prefix
// (which has no raw preimage). It is the single sanctioned bridge between the
// InputOffset and RawOffset coordinate systems.
func (w autoChunkWindow) toRaw(t InputOffset) (mirror.RawOffset, bool) {
	if t < w.prefixLen {
		return 0, false
	}
	return w.bodyStart + mirror.RawOffset(t-w.prefixLen), true
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
		Version:   store.MajorVersion,
		Chunks:    make([]mirror.Chunk, len(pf.chunks)),
	}
	for i := range pf.chunks {
		a.Chunks[i] = mirror.Chunk{
			Start:     mirror.RawOffset(pf.spans[i][0]),
			End:       mirror.RawOffset(pf.spans[i][1]),
			Embedding: pf.embeddings[i],
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

// indexedEntry records which model embedded a path, the stored blob sha, and the
// embedding/storage version the artifact was written under. The version lets the
// reindex skip decision treat a format bump as staleness even when the blob is
// unchanged.
type indexedEntry struct {
	model   string
	sha     string
	version int
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
		indexed[path] = indexedEntry{model: e.ModelName, sha: e.BlobSha, version: e.Version}
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
		if e.version != store.MajorVersion {
			rep.addf(string(path), "stale format: artifact version %d, current %d", e.version, store.MajorVersion)
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

// SyncCache reconciles the derived SQLite cache with the mirror tree so read
// commands observe up-to-date results. It ensures each active model's vec table
// exists, then upserts artifacts whose fingerprint changed and evicts those no
// longer in the tree. It is safe to call on a cold cache (a full build) and on a
// warm one (an incremental diff); a missing/stale cache only affects latency,
// never results, because the mirror tree is the source of truth.
func SyncCache(o *Options) error {
	models := o.activeModels()
	for _, m := range models {
		if err := o.Store.EnsureVecTable(m.ModelName(), m.Dimensions()); err != nil {
			return err
		}
	}
	return o.syncCache(models)
}

// Search embeds the query with every active model, queries each model's vec
// table, and merges results by descending score (truncated to topK). It first
// syncs the derived cache from the mirror tree so results reflect the committed
// index even when the cache is stale or absent.
func Search(o *Options, query string, topK int) ([]store.SearchResult, error) {
	if err := SyncCache(o); err != nil {
		return nil, err
	}
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

// Package index implements the reindex flow: it diffs the marker commit
// (pkb-state.toml) against HEAD (or does a full ls-tree on cold
// start/recovery), then indexes/updates/deletes files. There is no watcher;
// reindex runs to completion and exits.
package index

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/cost"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/filetype"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/infer"
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
	Model embed.EmbeddingModel
	// Inference, when non-nil, augments text/markdown chunks before embedding.
	// Its identity is folded into text-file reuse so switching models
	// invalidates stale augmented vectors. Nil disables augmentation.
	Inference infer.InferenceModel
	Ignore    *Ignore
	// ExtOverrides forces a file extension to a file type ("code"/"text").
	ExtOverrides map[string]string
	// MaxParallelism bounds concurrent inference (augmentation) calls during
	// indexing. Values below 1 are treated as 1.
	MaxParallelism int
	// MaxReindexCost caps the projected dollar cost of a single run (a
	// per-run cap, not a cumulative spend limit). Reindex estimates the cost
	// before any paid work and aborts when it exceeds MaxReindexCost. A
	// non-positive value disables the gate.
	MaxReindexCost float64
	// ContextualizeText, when true and the embedding model implements
	// embed.ContextualEmbeddingModel, routes text files through the model's
	// whole-document auto-chunking endpoint (skipping PKB chunking and
	// inference augmentation) instead of the isolated per-chunk path. Code
	// files are unaffected.
	ContextualizeText bool
}

// contextualModel returns the embedding model as a ContextualEmbeddingModel
// when the auto-chunk text path is enabled and the model supports it. The
// second result reports whether the contextual text path is active.
func (o *Options) contextualModel() (embed.ContextualEmbeddingModel, bool) {
	if !o.ContextualizeText {
		return nil, false
	}
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

// promptVersion is a hand-maintained version of the augmentation prompt
// template (see augmentPrompt). Bump it whenever the prompt text changes so the
// recorded minor spec reflects the augmentation that produced a chunk's blurb.
// It is part of the minor spec only: changing it never invalidates an existing
// embedding (augmentation merely contributes extra text to the embedded input).
const promptVersion = "1"

// minorSpec serializes the augmentation configuration -- whether augmentation
// was enabled, the inference-model identity, and the prompt version -- into a
// compact, deterministic string for recording and inspection. It is
// deliberately excluded from embedding compatibility/reuse decisions: a stored
// vector remains valid even when the minor spec changes, because augmentation
// only adds text to the embedded input. When augmentation is disabled the spec
// is the empty-augmentation form ("off||").
func (o *Options) minorSpec() string {
	if o.Inference == nil {
		return "off||"
	}
	return fmt.Sprintf("on|%s|%s", o.Inference.ModelName(), promptVersion)
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
	ref := "HEAD"
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
		for path, meta := range files {
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha, minorSpec: meta.MinorSpec}
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

	est, err := o.estimate(touched, treeMap, indexed, false)
	if err != nil {
		return State{}, err
	}
	fmt.Fprintf(os.Stderr, "estimated reindex cost: $%.2f (%d files, %d chunks, ~%d embed tokens, ~%d inference in/%d out tokens)\n",
		est.dollars, est.files, est.chunks, est.embedTokens, est.inferInputTokens, est.inferOutputTokens)
	if o.MaxReindexCost > 0 && est.dollars > o.MaxReindexCost {
		return State{}, fmt.Errorf("estimated reindex cost $%.2f exceeds max reindex cost $%.2f (%d files, %d chunks, ~%d embed tokens, ~%d inference in/%d out tokens); reindex locally instead",
			est.dollars, o.MaxReindexCost, est.files, est.chunks, est.embedTokens, est.inferInputTokens, est.inferOutputTokens)
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
			// file. The minor (augmentation) spec is deliberately excluded: an
			// augmentation-spec change (inference model or prompt) never
			// invalidates a vector, so it triggers no re-embedding or
			// re-augmentation.
			// A stored autoChunkMinorSpec marks a file embedded via the
			// whole-document auto-chunk path. Flipping the contextualizeText
			// option changes which path a text file takes, so a file whose
			// stored mode disagrees with the current mode must be re-embedded
			// even though its blob and model name are unchanged. Code files are
			// never on the auto-chunk path, so this never fires for them.
			wasAutoChunk := prevEntry.minorSpec == autoChunkMinorSpec
			if wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha && wasAutoChunk == o.isContextual(path) {
				continue
			}
			// If a different model previously embedded this path (e.g. routing
			// changed), purge the stale rows first.
			if wasIndexed && prevEntry.model != model.ModelName() {
				if err := o.Store.DeleteFile(string(path), prevEntry.model); err != nil {
					return State{}, err
				}
			}
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
				if err := o.Store.DeleteFile(string(path), prevEntry.model); err != nil {
					return State{}, err
				}
			}
		}
	}
	if err := flush(); err != nil {
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
	Files             int
	Chunks            int
	EmbedTokens       int
	InferInputTokens  int
	InferOutputTokens int
	EmbedDollars      float64
	InferDollars      float64
	Dollars           float64
}

// Estimate projects the dollar cost of a reindex without performing any paid
// embedding/inference work or mutating index data. When full is true it
// projects a from-scratch reindex of the entire repository, ignoring per-chunk
// reuse so every chunk is charged; otherwise it projects the next incremental
// reindex against HEAD, mirroring Reindex's skip and reuse decisions exactly.
func Estimate(o *Options, full bool) (CostEstimate, error) {
	ref := "HEAD"
	repoRoot := string(o.Repo.Root)

	targetSha, err := o.Repo.ResolveRef(ref)
	if err != nil {
		return CostEstimate{}, err
	}

	models := o.activeModels()
	for _, m := range models {
		if err := o.Store.EnsureVecTable(m.ModelName(), m.Dimensions()); err != nil {
			return CostEstimate{}, err
		}
	}

	indexed := map[paths.GitRootRelativePath]indexedEntry{}
	for _, m := range models {
		files, err := o.Store.IndexedFiles(m.ModelName())
		if err != nil {
			return CostEstimate{}, err
		}
		for path, meta := range files {
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha, minorSpec: meta.MinorSpec}
		}
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
		prev, err := readState(repoRoot)
		if err != nil {
			return CostEstimate{}, err
		}
		touched, err = o.touchedPaths(prev, targetSha, treeMap, indexed)
		if err != nil {
			return CostEstimate{}, err
		}
	}

	est, err := o.estimate(touched, treeMap, indexed, full)
	if err != nil {
		return CostEstimate{}, err
	}
	return CostEstimate{
		Files:             est.files,
		Chunks:            est.chunks,
		EmbedTokens:       est.embedTokens,
		InferInputTokens:  est.inferInputTokens,
		InferOutputTokens: est.inferOutputTokens,
		EmbedDollars:      est.embedDollars,
		InferDollars:      est.inferDollars,
		Dollars:           est.dollars,
	}, nil
}

// touchedPaths computes the set of paths that might need work, choosing the
// incremental, divergence, or full strategy.
func (o *Options) touchedPaths(prev *State, targetSha string, treeMap map[paths.GitRootRelativePath]string, indexed map[paths.GitRootRelativePath]indexedEntry) (map[paths.GitRootRelativePath]struct{}, error) {
	touched := map[paths.GitRootRelativePath]struct{}{}

	// addModeFlips adds any already-indexed file whose recorded auto-chunk mode
	// disagrees with the current contextualizeText decision. Flipping the option
	// leaves the commit and blob shas unchanged, so a same-commit reindex would
	// otherwise re-embed nothing; this forces the affected text files (and only
	// those) back through the pipeline. Code files are never on the auto-chunk
	// path, so their stored mode always matches and they are never added.
	addModeFlips := func() {
		for path, entry := range indexed {
			if _, inTree := treeMap[path]; !inTree || !o.candidate(path) {
				continue
			}
			wasAutoChunk := entry.minorSpec == autoChunkMinorSpec
			if wasAutoChunk != o.isContextual(path) {
				touched[path] = struct{}{}
			}
		}
	}

	full := prev == nil || prev.Commit == "" || !o.Repo.ObjectExists(prev.Commit)
	if !full && prev.Commit == targetSha {
		addModeFlips()
		return touched, nil // nothing changed except possible mode flips
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
		addModeFlips()
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
	addModeFlips()
	return touched, nil
}

// costEstimate summarizes a projected reindex run: how many files/chunks need
// fresh work and the token/dollar totals that work is expected to cost. It is
// computed with no API calls and no DB mutation.
type costEstimate struct {
	files             int
	chunks            int
	embedTokens       int
	inferInputTokens  int
	inferOutputTokens int
	embedDollars      float64
	inferDollars      float64
	dollars           float64
}

// estimate projects the dollar cost of indexing the touched set, counting only
// work that will actually be paid for. It mirrors Reindex's skip decision and
// per-chunk reuse so reuse hits are never charged: a file fully indexed by the
// same model against the same blob is skipped, and within a reindexed file only
// reuse-miss chunks contribute embedding (and, for text files with augmentation
// enabled, inference) tokens. Augmentation blurb length is unknown at estimate
// time, so embedding tokens are projected from the un-augmented contextual text
// -- a slight under-count of the augmentation contribution that the dominant
// inference term already accounts for. No network or DB writes occur.
func (o *Options) estimate(touched map[paths.GitRootRelativePath]struct{}, treeMap map[paths.GitRootRelativePath]string, indexed map[paths.GitRootRelativePath]indexedEntry, fromScratch bool) (costEstimate, error) {
	var est costEstimate
	model := o.Model
	for path := range touched {
		blobSha, inTree := treeMap[path]
		if !inTree || !o.candidate(path) {
			continue
		}
		prevEntry, wasIndexed := indexed[path]
		wasAutoChunk := prevEntry.minorSpec == autoChunkMinorSpec
		if !fromScratch && wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha && wasAutoChunk == o.isContextual(path) {
			continue
		}
		// Auto-chunk text files are sent whole to the contextual endpoint and
		// skip inference augmentation. Project their embedding tokens from the
		// whole-file length and charge no inference.
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
		var existing map[string]store.ReuseChunk
		if !fromScratch {
			existing, err = o.Store.ChunkEmbeddings(string(path), model.ModelName())
			if err != nil {
				return costEstimate{}, err
			}
		}
		isText := o.route(path) != filetype.Code
		est.files++
		for _, c := range chunks {
			if _, ok := existing[store.ChunkKey(c.HeadingContext, c.Text)]; ok {
				continue
			}
			est.chunks++
			est.embedTokens += len(Contextualize(filetype.LineComment(string(path)), c.HeadingContext, "", c.Text)) / cost.CharsPerToken
			if o.Inference != nil && isText {
				est.inferInputTokens += (len(content) + len(c.HeadingContext) + len(c.Text)) / cost.CharsPerToken
				est.inferOutputTokens += cost.AugmentMaxTokens
			}
		}
	}
	est.embedDollars = float64(est.embedTokens) * cost.EmbeddingPricePerToken(model.ModelName())
	if o.Inference != nil {
		ip := cost.InferencePricePerToken(o.Inference.ModelName())
		est.inferDollars = float64(est.inferInputTokens)*ip.InputPerToken + float64(est.inferOutputTokens)*ip.OutputPerToken
	}
	est.dollars = est.embedDollars + est.inferDollars
	return est, nil
}

// preparedFile holds everything needed to persist one file except the freshly
// computed embeddings: chunking, reuse resolution and augmentation are all done
// up front, and embedIdx lists the chunk indices still awaiting embedding.
// remaining counts how many of those are not yet filled in (decremented as
// batches flush); the file is written only when it reaches zero.
type preparedFile struct {
	path           paths.GitRootRelativePath
	blobSha        string
	minorSpec      string
	chunks         []chunk.ChunkInfo
	contextualized []string
	augmentations  []string
	augSpecs       []string
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

// prepareFile does all per-file work except the embedding call and the database
// write: it chunks the file, resolves reuse, augments reuse-miss text chunks,
// and builds the contextualized text for every chunk. The returned preparedFile
// carries reused vectors already in place and embedIdx listing the chunks that
// still need embedding (in a cross-file batch).
func (o *Options) prepareFile(path paths.GitRootRelativePath, blobSha string, model embed.EmbeddingModel) (*preparedFile, error) {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return nil, err
	}
	chunks, err := o.chunkFile(path, content)
	if err != nil {
		return nil, err
	}

	isText := o.route(path) != filetype.Code
	comment := filetype.LineComment(string(path))
	minorSpec := o.minorSpec()

	// Load the committed-generation reuse map keyed on ChunkKey (heading
	// breadcrumb + raw text). Reuse is valid for code and text alike: the minor
	// (augmentation) spec never invalidates a vector, so an unchanged chunk keeps
	// both its embedding and its stored augmentation blurb even when the file
	// changed elsewhere or the augmentation spec changed.
	existing, err := o.Store.ChunkEmbeddings(string(path), model.ModelName())
	if err != nil {
		return nil, err
	}

	// Resolve, per chunk: whether it is a reuse hit, the augmentation blurb, and
	// the aug_spec that produced that blurb. Reuse hits carry the stored blurb
	// and spec verbatim; misses default to no blurb under the current spec.
	reuse := make([]bool, len(chunks))
	augmentations := make([]string, len(chunks))
	augSpecs := make([]string, len(chunks))
	reuseEmb := make([]embed.Embedding, len(chunks))
	for i, c := range chunks {
		augSpecs[i] = minorSpec
		if rc, ok := existing[store.ChunkKey(c.HeadingContext, c.Text)]; ok {
			reuse[i] = true
			augmentations[i] = rc.Augmentation
			augSpecs[i] = rc.AugSpec
			reuseEmb[i] = rc.Embedding
		}
	}

	// Augment only reuse-miss chunks of text/markdown files (the contextual-
	// retrieval pattern): each gets a short blurb situating it within the whole
	// document. Inference failures degrade to the deterministic text with a
	// warning and never abort the run; reuse hits are never re-augmented.
	if o.Inference != nil && isText {
		parallelism := o.MaxParallelism
		if parallelism < 1 {
			parallelism = 1
		}
		sem := make(chan struct{}, parallelism)
		var wg sync.WaitGroup
		for i, c := range chunks {
			if reuse[i] {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, c chunk.ChunkInfo) {
				defer wg.Done()
				defer func() { <-sem }()
				blurb, err := o.Inference.Complete(augmentPrompt(string(content), c.HeadingContext, c.Text))
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: inference failed for %s chunk %d, using deterministic context: %v\n", path, i, err)
					return
				}
				blurb = stripThinking(blurb)
				if blurb == "" || strings.TrimSpace(blurb) == augmentNoneSentinel {
					return
				}
				augmentations[i] = blurb
			}(i, c)
		}
		wg.Wait()
	}

	// Build the embedded text for every chunk from its heading breadcrumb and
	// augmentation blurb. Reused vectors are carried over in place; reuse misses
	// are recorded in embedIdx to be embedded later in a cross-file batch.
	pf := compactPrepared(path, comment, chunks, augmentations, augSpecs, reuse, reuseEmb)
	pf.blobSha = blobSha
	pf.minorSpec = minorSpec
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
// whole-document auto-chunking endpoint. PKB does not chunk the file and no
// inference augmentation runs; instead the model returns its own chunks, each
// with a contextualized vector. Chunks are file-tagged (no line ranges) and
// carry no augmentation. Large files are sent in overlapping windows and the
// resulting chunks are deduped by text identity. Per-chunk reuse does not apply
// here; unchanged files are skipped wholesale by the caller's blob_sha check.
func (o *Options) prepareContextualFile(path paths.GitRootRelativePath, blobSha string, cm embed.ContextualEmbeddingModel) (*preparedFile, error) {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return nil, err
	}
	windows := autoChunkWindows(string(content))
	pf := &preparedFile{path: path, blobSha: blobSha, minorSpec: autoChunkMinorSpec}
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
			pf.augmentations = append(pf.augmentations, "")
			pf.augSpecs = append(pf.augSpecs, autoChunkMinorSpec)
			pf.embeddings = append(pf.embeddings, c.Embedding)
			pf.chars += len(text)
		}
	}
	return pf, nil
}

// autoChunkMinorSpec marks a file that was embedded via the whole-document
// auto-chunking path (rather than the isolated per-chunk augmentation path).
const autoChunkMinorSpec = "autochunk"

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
func compactPrepared(path paths.GitRootRelativePath, comment string, chunks []chunk.ChunkInfo, augmentations, augSpecs []string, reuse []bool, reuseEmb []embed.Embedding) *preparedFile {
	pf := &preparedFile{path: path}
	for i, c := range chunks {
		ctx := Contextualize(comment, c.HeadingContext, augmentations[i], c.Text)
		if strings.TrimSpace(ctx) == "" {
			fmt.Fprintf(os.Stderr, "warning: skipping empty chunk %d in %s (no embeddable content)\n", i, path)
			continue
		}
		j := len(pf.chunks)
		pf.chunks = append(pf.chunks, c)
		pf.contextualized = append(pf.contextualized, ctx)
		pf.augmentations = append(pf.augmentations, augmentations[i])
		pf.augSpecs = append(pf.augSpecs, augSpecs[i])
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
// expensive embedding and augmentation work is already done, so the write is
// quick. A crash before it commits leaves the previously indexed state intact,
// and the file is simply reindexed on the next run (reusing unchanged chunks via
// ChunkEmbeddings).
func (o *Options) writeFile(pf *preparedFile, model embed.EmbeddingModel) error {
	return o.Store.PutFile(string(pf.path), model.ModelName(), pf.blobSha, pf.minorSpec, pf.chunks, pf.contextualized, pf.augmentations, pf.augSpecs, pf.embeddings)
}

// Contextualize builds the text actually embedded for a chunk: the raw chunk
// text prefixed by its heading breadcrumb and (outermost) its augmentation
// blurb. When the file has a known line-comment prefix the metadata is rendered
// as zero-indented comments in the file's own language, which keeps the embedded
// text in distribution for code embedding models; the chunk text itself is left
// untouched so its original indentation is preserved. Files without a comment
// prefix (markdown/plaintext) fall back to <context>...</context> blocks. Empty
// components are omitted.
func Contextualize(comment, headingContext, augmentation, text string) string {
	s := text
	if headingContext != "" {
		s = metaBlock(comment, headingContext) + "\n\n" + s
	}
	if augmentation != "" {
		s = metaBlock(comment, augmentation) + "\n\n" + s
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

// augmentNoneSentinel is the exact token the model is told to emit when a chunk
// needs no added context. The pipeline maps it to an empty blurb so we never
// embed filler. It is unlikely to appear as legitimate context output.
const augmentNoneSentinel = "NONE"

// augmentPrompt builds the contextual-retrieval prompt asking the inference
// model for a single sentence that situates a chunk within its whole document,
// adding only information not already recoverable from the chunk text itself.
// The heading breadcrumb is passed explicitly so the model can anchor the chunk
// without re-deriving its location, and the instructions are placed after the
// chunk so they sit at the end of the context window where small models follow
// them more reliably (at a small prompt-caching cost).
func augmentPrompt(document, headingContext, chunkText string) string {
	chunkBlock := chunkText
	if headingContext != "" {
		chunkBlock = fmt.Sprintf("%s\n\n%s", headingContext, chunkText)
	}
	return fmt.Sprintf(`<document>
%s
</document>

Here is a chunk taken from the document above:
<chunk>
%s
</chunk>

When searching, the user is going to see the chunk without the rest of the document. Our job is to provide context to help the user understand the chunk when presented in isolation.

If the chunk mentions "it", "he", "the benchmark", "this approach", but doesn't capture what the chunk is referncing, explain the reference.

Examples:
-	"it" refers to chunking.
- "this approach" refers to context augmentation.

Resolve abbreviations and acronyms, when they are defined in the document.

Examples:
- pkb is "personal knowledge base", the name of this tool

Bias towards brevity, if you can't find obvious context to add, output exactly %s and nothing else.`, document, chunkBlock, augmentNoneSentinel)
}

// thinkBlockRe matches a complete <think>...</think> reasoning block.
var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinking removes reasoning output that thinking models (e.g. Qwen3) may
// emit before their answer, so it never gets embedded as part of the context
// blurb. It drops complete <think>...</think> blocks; if a dangling </think>
// remains (some chat templates emit the opening tag implicitly), everything up
// to and including it is dropped.
func stripThinking(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// indexedEntry records which model embedded a path, the stored blob sha, and
// the recorded minor spec (used to detect an auto-chunk mode flip).
type indexedEntry struct {
	model     string
	sha       string
	minorSpec string
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
// marker's commit and file/chunk counts agree with the index.
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
	if prev.Commit != targetSha {
		rep.addf("", "state commit %s does not match HEAD %s", prev.Commit, targetSha)
	}

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

	indexed := map[paths.GitRootRelativePath]indexedEntry{}
	for _, m := range o.activeModels() {
		files, err := o.Store.IndexedFiles(m.ModelName())
		if err != nil {
			return rep, err
		}
		for path, meta := range files {
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha, minorSpec: meta.MinorSpec}
		}
		s, err := o.Store.Stats(m.ModelName())
		if err != nil {
			return rep, err
		}
		rep.IndexedFiles += s.Files
		rep.IndexedChunks += s.Chunks
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

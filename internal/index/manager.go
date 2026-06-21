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
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha, complete: meta.Complete}
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
	)
	const charsPerToken = 3 // rough approximation for tok/s reporting
	for path := range touched {
		blobSha, inTree := treeMap[path]
		prevEntry, wasIndexed := indexed[path]
		if inTree && o.candidate(path) {
			model := o.Model
			// Skip a file only when it was fully indexed (complete=1) by the same
			// embedding model against the same blob. The minor (augmentation) spec
			// is deliberately excluded: an augmentation-spec change (inference
			// model or prompt) never invalidates a vector, so it triggers no
			// re-embedding or re-augmentation. A complete=0 file is always
			// reprocessed, and reprocessing is cheap (per-chunk reuse hits).
			if wasIndexed && prevEntry.complete && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha {
				continue
			}
			// If a different model previously embedded this path (e.g. routing
			// changed), purge the stale rows first.
			if wasIndexed && prevEntry.model != model.ModelName() {
				if err := o.Store.DeleteFile(string(path), prevEntry.model); err != nil {
					return State{}, err
				}
			}
			res, err := o.indexFile(path, blobSha, model)
			if err != nil {
				return State{}, err
			}
			filesIndexed++
			chunksIndexed += res.chunks
			charsEmbedded += res.chars
			embedTime += res.embedTime
			elapsed := time.Since(indexStart).Seconds()
			embedTokPerSec := 0.0
			if s := embedTime.Seconds(); s > 0 {
				embedTokPerSec = float64(charsEmbedded) / charsPerToken / s
			}
			fmt.Fprintf(os.Stderr, "indexed %s (%d chunks) | %d files, %d chunks, %.1f chunks/sec, embed ~%.0f tok/sec\n",
				path, res.chunks, filesIndexed, chunksIndexed, float64(chunksIndexed)/elapsed, embedTokPerSec)
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
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha, complete: meta.Complete}
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
		if !fromScratch && wasIndexed && prevEntry.complete && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha {
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
			est.embedTokens += len(contextualize(c.HeadingContext, "", c.Text)) / cost.CharsPerToken
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

// indexResult reports per-file work for progress logging: the number of chunks
// embedded, the total characters embedded (a proxy for tokens), and the wall
// time spent in the embedding call itself (excluding inference augmentation).
type indexResult struct {
	chunks    int
	chars     int
	embedTime time.Duration
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

func (o *Options) indexFile(path paths.GitRootRelativePath, blobSha string, model embed.EmbeddingModel) (indexResult, error) {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return indexResult{}, err
	}
	chunks, err := o.chunkFile(path, content)
	if err != nil {
		return indexResult{}, err
	}

	isText := o.route(path) != filetype.Code
	minorSpec := o.minorSpec()

	// Load the committed-generation reuse map keyed on ChunkKey (heading
	// breadcrumb + raw text). Reuse is valid for code and text alike: the minor
	// (augmentation) spec never invalidates a vector, so an unchanged chunk keeps
	// both its embedding and its stored augmentation blurb even when the file
	// changed elsewhere or the augmentation spec changed.
	existing, err := o.Store.ChunkEmbeddings(string(path), model.ModelName())
	if err != nil {
		return indexResult{}, err
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
	// augmentation blurb. Embed only reuse misses, in a single batched call,
	// carrying reused vectors over unchanged.
	contextualized := make([]string, len(chunks))
	embeddings := make([]embed.Embedding, len(chunks))
	var toEmbed []string
	var toEmbedIdx []int
	var chars int
	for i, c := range chunks {
		contextualized[i] = contextualize(c.HeadingContext, augmentations[i], c.Text)
		if reuse[i] {
			embeddings[i] = reuseEmb[i]
			continue
		}
		chars += len(contextualized[i])
		toEmbed = append(toEmbed, contextualized[i])
		toEmbedIdx = append(toEmbedIdx, i)
	}

	embedStart := time.Now()
	if len(toEmbed) > 0 {
		fresh, err := model.EmbedChunks(toEmbed)
		if err != nil {
			return indexResult{}, err
		}
		for j, e := range fresh {
			embeddings[toEmbedIdx[j]] = e
		}
	}
	embedTime := time.Since(embedStart)

	// Persist incrementally under a fresh generation: each chunk is written in
	// its own transaction (StartFile/InsertChunk) so a crash keeps every chunk
	// already embedded and augmented, and FinalizeFile makes the new generation
	// visible atomically while dropping the superseded one.
	fileID, newGen, err := o.Store.StartFile(string(path), model.ModelName(), blobSha, minorSpec)
	if err != nil {
		return indexResult{}, err
	}
	for i, c := range chunks {
		if err := o.Store.InsertChunk(fileID, newGen, model.ModelName(), c, contextualized[i], augmentations[i], augSpecs[i], embeddings[i]); err != nil {
			return indexResult{}, err
		}
	}
	if err := o.Store.FinalizeFile(fileID, newGen, model.ModelName()); err != nil {
		return indexResult{}, err
	}
	return indexResult{chunks: len(chunks), chars: chars, embedTime: embedTime}, nil
}

// contextualize builds the text actually embedded for a chunk: the raw text,
// wrapped first in its heading-breadcrumb context and then (outermost) in its
// augmentation blurb, each as a <context>...</context> block. Empty components
// are omitted so an un-augmented chunk embeds just its heading context + text.
func contextualize(headingContext, augmentation, text string) string {
	s := text
	if headingContext != "" {
		s = fmt.Sprintf("<context>\n%s\n</context>\n\n%s", headingContext, s)
	}
	if augmentation != "" {
		s = fmt.Sprintf("<context>\n%s\n</context>\n\n%s", augmentation, s)
	}
	return s
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
// whether the file was fully indexed (complete) by that model.
type indexedEntry struct {
	model    string
	sha      string
	complete bool
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

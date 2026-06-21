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
}

// activeModels returns the embedding models in use.
func (o *Options) activeModels() []embed.EmbeddingModel {
	return []embed.EmbeddingModel{o.Model}
}

// inferenceName returns the identity of the configured inference model, or ""
// when augmentation is disabled. It is folded into text-file reuse so a model
// switch invalidates stale augmented embeddings.
func (o *Options) inferenceName() string {
	if o.Inference == nil {
		return ""
	}
	return o.Inference.ModelName()
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
			indexed[paths.GitRootRelativePath(path)] = indexedEntry{model: m.ModelName(), sha: meta.Sha, inference: meta.InferenceModel}
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
			if wasIndexed && prevEntry.model == model.ModelName() && prevEntry.sha == blobSha {
				// Code files are deterministic, so unchanged content + same
				// embedding model is always reusable. Text files are
				// LLM-augmented, so also require the inference-model identity to
				// match; otherwise re-augment and re-embed the whole file.
				if o.route(path) == filetype.Code || prevEntry.inference == o.inferenceName() {
					continue
				}
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

// indexResult reports per-file work for progress logging: the number of chunks
// embedded, the total characters embedded (a proxy for tokens), and the wall
// time spent in the embedding call itself (excluding inference augmentation).
type indexResult struct {
	chunks    int
	chars     int
	embedTime time.Duration
}

func (o *Options) indexFile(path paths.GitRootRelativePath, blobSha string, model embed.EmbeddingModel) (indexResult, error) {
	content, err := os.ReadFile(o.Repo.Root.Join(path).String())
	if err != nil {
		return indexResult{}, err
	}
	// Code files are chunked along syntactic boundaries via tree-sitter (with a
	// line-based fallback); text/markdown files use the structural markdown
	// chunker.
	var chunks []chunk.ChunkInfo
	if o.route(path) == filetype.Code {
		grammar := o.grammarFor(path)
		var err error
		if chunk.IsConfigGrammar(grammar) {
			chunks, err = chunk.ChunkConfig(content, grammar, string(path), chunk.TargetChunkSize)
		} else {
			chunks, err = chunk.ChunkCode(content, grammar, string(path), chunk.TargetChunkSize)
		}
		if err != nil {
			return indexResult{}, err
		}
	} else {
		chunks = chunk.ChunkMarkdown(string(content), string(path), chunk.TargetChunkSize)
	}

	contextualized := make([]string, len(chunks))
	for i, c := range chunks {
		if c.HeadingContext != "" {
			contextualized[i] = fmt.Sprintf("<context>\n%s\n</context>\n\n%s", c.HeadingContext, c.Text)
		} else {
			contextualized[i] = c.Text
		}
	}

	// Text/markdown files are LLM-augmented (the contextual-retrieval pattern):
	// each chunk gets a short blurb situating it within the whole document,
	// prepended before embedding. Code files keep the deterministic prefix.
	// Inference failures degrade to the deterministic text with a warning and
	// never abort the run.
	if o.Inference != nil && o.route(path) != filetype.Code {
		parallelism := o.MaxParallelism
		if parallelism < 1 {
			parallelism = 1
		}
		sem := make(chan struct{}, parallelism)
		var wg sync.WaitGroup
		for i, c := range chunks {
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
				contextualized[i] = fmt.Sprintf("<context>\n%s\n</context>\n\n%s", blurb, contextualized[i])
			}(i, c)
		}
		wg.Wait()
	}

	// Code files are deterministic: reuse per-chunk vectors keyed on the
	// heading breadcrumb + raw text so a small edit re-embeds only the affected
	// chunks. Text files are LLM-augmented from the whole file, so a chunk's
	// vector can change for an edit anywhere in the file; reuse there is
	// all-or-nothing per file (handled by the blob-sha + inference-identity
	// short-circuit in Reindex), so on arrival here we re-embed every chunk.
	var chars int
	for _, c := range contextualized {
		chars += len(c)
	}

	var embeddings []embed.Embedding
	embedStart := time.Now()
	if o.route(path) == filetype.Code {
		embeddings, err = o.reuseEmbeddings(path, model, chunks, contextualized)
	} else {
		embeddings, err = model.EmbedChunks(contextualized)
	}
	embedTime := time.Since(embedStart)
	if err != nil {
		return indexResult{}, err
	}

	if err := o.Store.PutFile(string(path), model.ModelName(), blobSha, o.inferenceName(), chunks, contextualized, embeddings); err != nil {
		return indexResult{}, err
	}
	return indexResult{chunks: len(chunks), chars: chars, embedTime: embedTime}, nil
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

// reuseEmbeddings returns an embedding for every chunk, carrying over vectors
// for chunks whose deterministic key (heading breadcrumb + raw text) is
// unchanged and embedding only the rest in a single batched call. This keeps a
// small edit to a large file from re-embedding every chunk.
func (o *Options) reuseEmbeddings(path paths.GitRootRelativePath, model embed.EmbeddingModel, chunks []chunk.ChunkInfo, contextualized []string) ([]embed.Embedding, error) {
	if len(contextualized) == 0 {
		return nil, nil
	}
	existing, err := o.Store.ChunkEmbeddings(string(path), model.ModelName())
	if err != nil {
		return nil, err
	}

	embeddings := make([]embed.Embedding, len(contextualized))
	var toEmbed []string
	var toEmbedIdx []int
	for i, c := range chunks {
		if e, ok := existing[store.ChunkKey(c.HeadingContext, c.Text)]; ok {
			embeddings[i] = e
			continue
		}
		toEmbed = append(toEmbed, contextualized[i])
		toEmbedIdx = append(toEmbedIdx, i)
	}

	if len(toEmbed) > 0 {
		fresh, err := model.EmbedChunks(toEmbed)
		if err != nil {
			return nil, err
		}
		for j, e := range fresh {
			embeddings[toEmbedIdx[j]] = e
		}
	}
	return embeddings, nil
}

// indexedEntry records which model embedded a path, the stored blob sha, and
// the inference-model identity used for its (text-file) augmentation.
type indexedEntry struct {
	model     string
	sha       string
	inference string
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

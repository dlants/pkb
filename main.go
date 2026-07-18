// Command pkb is the CLI for the git-repo-rooted code+docs search index.
// It runs from a repo root and exposes the reindex, estimate, search, stats,
// and healthcheck commands.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dlants/pkb/internal/config"
	"github.com/dlants/pkb/internal/cost"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/index"
	"github.com/dlants/pkb/internal/paths"
	"github.com/dlants/pkb/internal/store"
)

func main() {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "reindex":
		err = runReindex(rest)
	case "estimate":
		err = runEstimate(rest)
	case "search":
		err = runSearch(rest)
	case "stats":
		err = runStats(rest)
	case "healthcheck":
		err = runHealthcheck(rest)
	case "version", "--version", "-v":
		err = runVersion(rest)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `pkb - git-repo-rooted code+docs search index

usage:
  pkb reindex          reindex the repo against HEAD (--staged for the git index)
  pkb estimate         estimate the cost of the next and a full reindex
  pkb search <query>   search the index
  pkb stats            print index statistics
  pkb healthcheck      verify the index + state marker against the git tree
  pkb version          print the pkb version

pkb runs from anywhere inside a git repository; it discovers the repo root,
reads pkb.toml / .pkb/config.toml and .pkbignore. The committed index is a
mirror tree under .pkb/index; a gitignored SQLite cache at .pkb/cache.db is
derived from it on demand to make queries fast. Reindex is meant to run from a
commit hook or CI step when code lands on the default branch (see README.md).
`)
}

// cacheRelPath is the fixed repo-relative location of the gitignored SQLite
// cache. The committed source of truth is the mirror tree under .pkb/index; the
// cache is derived from it and rebuilt on demand, so it is never committed.
const cacheRelPath = ".pkb/cache.db"

// setup discovers the repo root from cwd, loads config + .pkbignore, builds the
// two embedding models, opens the database, and assembles index.Options.
func setup() (*index.Options, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	repo, err := git.Open(cwd)
	if err != nil {
		return nil, nil, fmt.Errorf("not inside a git repository: %w", err)
	}

	cfg, err := config.Load(string(repo.Root))
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	ignore := index.NewIgnore(cfg.Exclude)

	model, err := embed.Build(cfg.Embedding.Provider, cfg.Embedding.Model, cfg.Embedding.Dimensions, cfg.Embedding.BaseURL, cfg.Embedding.APIKeyEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("building embedding model: %w", err)
	}
	if _, ok := model.(embed.ContextualEmbeddingModel); !ok {
		return nil, nil, fmt.Errorf("embedding model %q does not support contextualized document embeddings; a contextual embedder (e.g. voyage-context-*) is required", model.ModelName())
	}

	cachePath := filepath.Join(string(repo.Root), cacheRelPath)
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating cache directory: %w", err)
	}
	st, err := store.Open(cachePath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening cache database: %w", err)
	}

	opts := &index.Options{
		Repo:           repo,
		Store:          st,
		Model:          model,
		Ignore:         ignore,
		ExtOverrides:   cfg.ExtOverrides,
		MaxReindexCost: cfg.MaxReindexCost,
	}
	cleanup := func() { st.Close() }
	return opts, cleanup, nil
}

func runReindex(args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	maxCost := fs.Float64("max-reindex-cost", -1, "override the configured per-run max reindex cost in dollars (<=0 disables the gate)")
	staged := fs.Bool("staged", false, "index the staging area (git write-tree) instead of HEAD, for use in a pre-commit hook")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, cleanup, err := setup()
	if err != nil {
		return err
	}
	defer cleanup()
	if isFlagSet(fs, "max-reindex-cost") {
		opts.MaxReindexCost = *maxCost
	}
	opts.Staged = *staged

	st, err := index.Reindex(opts)
	if err != nil {
		return err
	}
	fmt.Printf("indexed commit %s: %d files, %d chunks\n", st.Commit, st.FileCount, st.ChunkCount)
	return nil
}

// isFlagSet reports whether the named flag was explicitly provided on the
// command line (as opposed to left at its default).
func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// round3 rounds x to 3 significant figures.
func round3(x float64) float64 {
	if x == 0 {
		return 0
	}
	mag := math.Pow(10, 2-math.Floor(math.Log10(math.Abs(x))))
	return math.Round(x*mag) / mag
}

// sig3 formats a number to 3 significant figures.
func sig3(x float64) string { return fmt.Sprintf("%g", round3(x)) }

// formatTok renders a token count to 3 significant figures, switching to kTok
// once the count reaches 1000.
func formatTok(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%dtok", n)
	}
	return fmt.Sprintf("%gkTok", round3(float64(n)/1000))
}

func printEstimate(label string, est index.CostEstimate) {
	fmt.Printf("%s - %d files / %d chunks: $%s. (%s embedding)\n",
		label, est.Files, est.Chunks, sig3(est.Dollars),
		formatTok(est.EmbedTokens))
}

func runEstimate(args []string) error {
	fs := flag.NewFlagSet("estimate", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, cleanup, err := setup()
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Println("Estimated costs:")
	fmt.Printf("- embedding: $%s/1M tok\n", sig3(cost.EmbeddingPricePerToken(opts.Model.ModelName())*1e6))

	next, err := index.Estimate(opts, false)
	if err != nil {
		return err
	}
	printEstimate("next reindex", next)

	full, err := index.Estimate(opts, true)
	if err != nil {
		return err
	}
	printEstimate("full reindex", full)
	return nil
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	topK := fs.Int("k", 5, "number of results to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("search: missing query")
	}
	opts, cleanup, err := setup()
	if err != nil {
		return err
	}
	defer cleanup()

	results, err := index.Search(opts, query, *topK)
	if err != nil {
		return err
	}
	fmt.Print(formatResults(opts.Repo.Root, results))
	return nil
}

func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, cleanup, err := setup()
	if err != nil {
		return err
	}
	defer cleanup()

	st, err := readState(string(opts.Repo.Root))
	if err != nil {
		return err
	}
	if st == nil {
		fmt.Println("no index yet; run `pkb reindex`")
		return nil
	}
	fmt.Printf("commit:    %s\n", st.Commit)
	fmt.Printf("files:     %d\n", st.FileCount)
	fmt.Printf("chunks:    %d\n", st.ChunkCount)
	return nil
}

// runVersion prints the module version. When pkb is built with `go install`,
// the version is embedded in the build info; for local/dev builds it falls back
// to "(devel)".
func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Printf("pkb %s\n", pkbVersion())
	return nil
}

// pkbVersion returns the module version: a clean vX.Y.Z for builds installed
// off a tag, or "(devel)" for local builds.
func pkbVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, cleanup, err := setup()
	if err != nil {
		return err
	}
	defer cleanup()

	rep, err := index.Healthcheck(opts)
	if err != nil {
		return err
	}

	fmt.Printf("HEAD:          %s\n", rep.HeadCommit)
	if rep.StateMissing {
		fmt.Println("state:         missing")
	} else {
		fmt.Printf("state commit:  %s\n", rep.StateCommit)
	}
	fmt.Printf("expected files: %d\n", rep.ExpectedFiles)
	fmt.Printf("indexed files:  %d\n", rep.IndexedFiles)
	fmt.Printf("indexed chunks: %d\n", rep.IndexedChunks)

	if len(rep.Issues) == 0 {
		fmt.Println("\nhealthy: index and state marker match the git tree")
		return nil
	}

	fmt.Printf("\n%d issue(s):\n", len(rep.Issues))
	for _, iss := range rep.Issues {
		if iss.Path == "" {
			fmt.Printf("  - %s\n", iss.Msg)
		} else {
			fmt.Printf("  - %s: %s\n", iss.Path, iss.Msg)
		}
	}
	return fmt.Errorf("healthcheck found %d issue(s)", len(rep.Issues))
}

// chunkHeading renders the shared header used by search results. The breadcrumb
// is prefixed with the file path for both code and
// markdown; we attach the chunk's starting line to that leading path in
// editor-friendly file:Ln form, e.g. "## path/to/file.go:L28 > type State" or
// "## docs/readme.md:L2 > # Title".
func chunkHeading(breadcrumb string, startLine int) string {
	file, rest, found := strings.Cut(breadcrumb, " > ")
	loc := fmt.Sprintf("## %s:L%d", file, startLine)
	if found {
		loc += " > " + rest
	}
	return loc
}

// formatResults renders search results as score-ordered markdown sections.
func formatResults(_ paths.AbsPath, results []store.SearchResult) string {
	if len(results) == 0 {
		return "No results found.\n"
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&b, "%s\n\n%s\n", chunkHeading(r.HeadingContext, r.StartLine), r.Text)
	}
	return b.String()
}

// state mirrors index.State for reading the marker file in `stats`.
type state struct {
	Commit     string `toml:"commit"`
	FileCount  int    `toml:"fileCount"`
	ChunkCount int    `toml:"chunkCount"`
}

func readState(repoRoot string) (*state, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "pkb-state.toml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s state
	if err := toml.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

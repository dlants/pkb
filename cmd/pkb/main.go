// Command pkb is the CLI for the git-repo-rooted code+docs search index.
// It runs from a repo root and exposes three commands: reindex, search, stats.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dlants/pkb/internal/config"
	"github.com/dlants/pkb/internal/embed"
	"github.com/dlants/pkb/internal/git"
	"github.com/dlants/pkb/internal/index"
	"github.com/dlants/pkb/internal/infer"
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
	case "search":
		err = runSearch(rest)
	case "stats":
		err = runStats(rest)
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
  pkb reindex          reindex the repo against HEAD
  pkb search <query>   search the index
  pkb stats            print index statistics

pkb runs from anywhere inside a git repository; it discovers the repo root,
reads pkb.toml / .pkb/config.toml and .pkbignore, and stores the index at
pkb.db at the repo root. Reindex is meant to run from a commit hook or CI step
when code
lands on the default branch (see README.md).
`)
}

// dbRelPath is the fixed repo-relative location of the index database.
const dbRelPath = "pkb.db"

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

	model, err := embed.Build(cfg.Embedding.Provider, cfg.Embedding.Model, cfg.Embedding.Dimensions, cfg.Embedding.Region, cfg.Embedding.Profile, cfg.Embedding.BaseURL, cfg.Embedding.APIKeyEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("building embedding model: %w", err)
	}

	inferenceModel, err := infer.Build(cfg.Inference.Provider, cfg.Inference.Model, cfg.Inference.Region, cfg.Inference.Profile, cfg.Inference.BaseURL, cfg.Inference.APIKeyEnv)
	if err != nil {
		return nil, nil, fmt.Errorf("building inference model: %w", err)
	}

	st, err := store.Open(filepath.Join(string(repo.Root), dbRelPath))
	if err != nil {
		return nil, nil, fmt.Errorf("opening database: %w", err)
	}

	opts := &index.Options{
		Repo:           repo,
		Store:          st,
		Model:          model,
		Inference:      inferenceModel,
		Ignore:         ignore,
		ExtOverrides:   cfg.ExtOverrides,
		MaxParallelism: cfg.MaxParallelism,
	}
	cleanup := func() { st.Close() }
	return opts, cleanup, nil
}

func runReindex(args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, cleanup, err := setup()
	if err != nil {
		return err
	}
	defer cleanup()

	st, err := index.Reindex(opts)
	if err != nil {
		return err
	}
	fmt.Printf("indexed commit %s: %d files, %d chunks\n", st.Commit, st.FileCount, st.ChunkCount)
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

// formatResults renders search results as score-ordered markdown sections.
func formatResults(root paths.AbsPath, results []store.SearchResult) string {
	if len(results) == 0 {
		return "No results found.\n"
	}
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n---\n\n")
		}
		abs := root.Join(paths.GitRootRelativePath(r.Path))
		fmt.Fprintf(&b, "## Result %d (score: %.3f)\nFile: %s\n\n%s\n", i+1, r.Score, abs, r.Text)
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

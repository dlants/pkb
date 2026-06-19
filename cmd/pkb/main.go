// Command pkb is the CLI for the git-repo-rooted code+docs search index.
// It runs from a repo root and exposes three commands: reindex, search, stats.
package main

import (
	"flag"
	"fmt"
	"os"
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
  pkb reindex          reindex the repo against the target ref
  pkb search <query>   search the index
  pkb stats            print index statistics
`)
}

func runReindex(args []string) error {
	return fmt.Errorf("reindex: not implemented")
}

func runSearch(args []string) error {
	return fmt.Errorf("search: not implemented")
}

func runStats(args []string) error {
	return fmt.Errorf("stats: not implemented")
}

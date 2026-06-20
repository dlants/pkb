// Package smoke proves the cgo toolchain links both sqlite-vec and a
// tree-sitter grammar into the binary.
package smoke

import (
	"database/sql"
	"testing"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

func TestSqliteVecLinks(t *testing.T) {
	sqlite_vec.Auto()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRow("select vec_version()").Scan(&version); err != nil {
		t.Fatalf("vec_version: %v", err)
	}
	if version == "" {
		t.Fatal("empty vec_version")
	}
}

func TestTreeSitterGrammarLinks(t *testing.T) {
	lang := tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
	if lang == nil {
		t.Fatal("failed to load typescript grammar")
	}

	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		t.Fatalf("set language: %v", err)
	}

	tree := parser.Parse([]byte("const x = 1;"), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatal("parse produced an error node")
	}
}

package chunk

import (
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tsrust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tsts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// grammars maps a grammar name (as produced by filetype routing) to a factory
// for its tree-sitter Language. Grammars are statically linked into the binary
// via cgo, so construction is cheap and never fails.
var grammars = map[string]func() *tree_sitter.Language{
	"go":         func() *tree_sitter.Language { return tree_sitter.NewLanguage(tsgo.Language()) },
	"javascript": func() *tree_sitter.Language { return tree_sitter.NewLanguage(tsjs.Language()) },
	"python":     func() *tree_sitter.Language { return tree_sitter.NewLanguage(tspy.Language()) },
	"rust":       func() *tree_sitter.Language { return tree_sitter.NewLanguage(tsrust.Language()) },
	"typescript": func() *tree_sitter.Language { return tree_sitter.NewLanguage(tsts.LanguageTypescript()) },
	"tsx":        func() *tree_sitter.Language { return tree_sitter.NewLanguage(tsts.LanguageTSX()) },
}

// HasGrammar reports whether a tree-sitter grammar is available for the name.
func HasGrammar(name string) bool {
	_, ok := grammars[name]
	return ok
}

// languageFor returns the tree-sitter Language for a grammar name, or nil if
// the grammar is not registered.
func languageFor(name string) *tree_sitter.Language {
	if f, ok := grammars[name]; ok {
		return f()
	}
	return nil
}

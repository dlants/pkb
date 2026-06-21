// Package filetype routes a file extension to a FileType (code or text) and,
// for code, the tree-sitter grammar name used to chunk it. The type also
// selects which embedding model embeds the file.
package filetype

import (
	"path/filepath"
	"strings"
)

// FileType distinguishes prose (text) from source code.
type FileType int

const (
	// Text is markdown/plaintext/prose, chunked structurally and embedded by
	// the text model.
	Text FileType = iota
	// Code is recognized source code, chunked by tree-sitter and embedded by
	// the code model.
	Code
)

func (t FileType) String() string {
	switch t {
	case Code:
		return "code"
	default:
		return "text"
	}
}

// Route describes how a file should be handled.
type Route struct {
	Type FileType
	// Grammar is the tree-sitter grammar name for code files; empty for text.
	Grammar string
}

// codeExts maps recognized source extensions to grammar names.
var codeExts = map[string]string{
	".ts":   "typescript",
	".tsx":  "tsx",
	".js":   "javascript",
	".jsx":  "javascript",
	".go":   "go",
	".py":   "python",
	".rs":   "rust",
	".hcl":  "hcl",
	".tf":   "hcl",
	".json": "json",
	".toml": "toml",
	".yaml": "yaml",
	".yml":  "yaml",
}

// RouteExt returns the Route for a file extension (including the leading dot).
func RouteExt(ext string) Route {
	if grammar, ok := codeExts[strings.ToLower(ext)]; ok {
		return Route{Type: Code, Grammar: grammar}
	}
	return Route{Type: Text}
}

// RoutePath returns the Route for a file path based on its extension.
func RoutePath(path string) Route {
	return RouteExt(filepath.Ext(path))
}

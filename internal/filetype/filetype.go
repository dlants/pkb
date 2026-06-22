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

// lineComments maps recognized source extensions to their single-line comment
// prefix. It is used to render chunk metadata (breadcrumb / augmentation) as a
// comment in the file's own language, which keeps the embedded text in
// distribution for code embedding models. Extensions without a natural line
// comment (notably JSON) borrow "//" since the text is only ever embedded, never
// parsed.
var lineComments = map[string]string{
	".ts":   "//",
	".tsx":  "//",
	".js":   "//",
	".jsx":  "//",
	".go":   "//",
	".rs":   "//",
	".json": "//",
	".py":   "#",
	".hcl":  "#",
	".tf":   "#",
	".toml": "#",
	".yaml": "#",
	".yml":  "#",
}

// LineComment returns the single-line comment prefix for a file path's
// extension, or "" if none is known (e.g. markdown/plaintext).
func LineComment(path string) string {
	return lineComments[strings.ToLower(filepath.Ext(path))]
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

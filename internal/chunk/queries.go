package chunk

import _ "embed"

// Vendored tree-sitter tags.scm queries, one per grammar. These are copied from
// each grammar's module (queries/tags.scm) and embedded because the binary is
// statically linked cgo and does not ship the module cache. Vendored queries
// must match the grammar version pinned in go.mod; bumping a grammar means
// re-vendoring its query. The typescript/tsx queries concatenate the
// javascript tags (upstream relies on query inheritance) so they capture the
// full ECMAScript construct set in addition to TypeScript-specific nodes.
//
// Source versions: go v0.23.4, javascript v0.23.1, python v0.23.6,
// rust v0.23.2, typescript v0.23.2.

//go:embed queries/go.scm
var queryGo string

//go:embed queries/javascript.scm
var queryJavascript string

//go:embed queries/python.scm
var queryPython string

//go:embed queries/rust.scm
var queryRust string

//go:embed queries/typescript.scm
var queryTypescript string

//go:embed queries/tsx.scm
var queryTSX string

// queries maps a grammar name to its embedded tags.scm source.
var queries = map[string]string{
	"go":         queryGo,
	"javascript": queryJavascript,
	"python":     queryPython,
	"rust":       queryRust,
	"typescript": queryTypescript,
	"tsx":        queryTSX,
}

// queryFor returns the vendored tags.scm source for a grammar name, or "" if
// none is vendored.
func queryFor(name string) string {
	return queries[name]
}

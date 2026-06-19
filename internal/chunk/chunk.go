// Package chunk defines the common chunk shape produced by every chunker
// (markdown and tree-sitter) so the downstream embedding/storage path is
// uniform regardless of source language.
package chunk

// Position is a zero-based line/column location within a file.
type Position struct {
	Line int
	Col  int
}

// ChunkInfo is the uniform output of every chunker. HeadingContext carries
// structural breadcrumbs (markdown heading hierarchy or code symbol path).
type ChunkInfo struct {
	Text           string
	HeadingContext string
	Start          Position
	End            Position
}

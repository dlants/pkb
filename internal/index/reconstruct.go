package index

import (
	"fmt"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/filetype"
	"github.com/dlants/pkb/internal/mirror"
	"github.com/dlants/pkb/internal/paths"
)

// byteSpan is a chunk's half-open [Start, End) byte range within a source blob.
// It is the only per-chunk state kept on disk; everything else is reconstructed
// from it at cache-sync time.
type byteSpan struct {
	Start mirror.RawOffset
	End   mirror.RawOffset
}

// reconstructedChunk is the fully rehydrated form of a stored offset span: the
// chunk text sliced from the blob, its heading breadcrumb (AST symbol path for
// code, header hierarchy for text), line/col positions, and the contextualized
// text actually associated with the chunk for display/situating.
type reconstructedChunk struct {
	Text           string
	HeadingContext string
	Start          chunk.Position
	End            chunk.Position
	Contextualized string
}

// Reconstruct rehydrates chunk rows from byte-offset spans against the exact
// blob content they were generated from. It slices each span's text, derives its
// breadcrumb from the file's structure (tree-sitter symbol path for code,
// markdown headers for text), computes line/col positions from the offsets, and
// builds the contextualized text via Contextualize. It is the single load path
// shared by the cache-sync reconstruction so code and text are rehydrated
// uniformly. A span out of range for the blob is a hard failure: a broken offset
// must surface immediately rather than degrade silently.
func Reconstruct(path paths.GitRootRelativePath, content []byte, spans []byteSpan) ([]reconstructedChunk, error) {
	route := filetype.RoutePath(string(path))
	comment := filetype.LineComment(string(path))
	isCode := route.Type == filetype.Code

	var coder *chunk.CodeBreadcrumber
	if isCode {
		var err error
		coder, err = chunk.NewCodeBreadcrumber(content, route.Grammar, string(path))
		if err != nil {
			return nil, fmt.Errorf("index: reconstruct %s: %w", path, err)
		}
	}

	out := make([]reconstructedChunk, len(spans))
	for i, s := range spans {
		if s.Start < 0 || int(s.End) > len(content) || s.Start > s.End {
			return nil, fmt.Errorf("index: chunk %d of %s has out-of-range span [%d,%d) for %d-byte blob", i, path, s.Start, s.End, len(content))
		}
		text := string(content[s.Start:s.End])
		var heading string
		var start, end chunk.Position
		if isCode {
			heading = coder.Breadcrumb(int(s.Start), int(s.End))
			start = chunk.PosFromByte(content, int(s.Start))
			end = chunk.PosFromByte(content, int(s.End))
		} else {
			heading = chunk.MarkdownBreadcrumb(string(content), string(path), int(s.Start))
		}
		out[i] = reconstructedChunk{
			Text:           text,
			HeadingContext: heading,
			Start:          start,
			End:            end,
			Contextualized: Contextualize(comment, heading, text),
		}
	}
	return out, nil
}

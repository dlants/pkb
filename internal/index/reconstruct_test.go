package index

import (
	"strings"
	"testing"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/filetype"
	"github.com/dlants/pkb/internal/paths"
)

func TestReconstructMarkdownMatchesChunker(t *testing.T) {
	text := "# Title\n\nintro para\n\n## Sub\n\nbody para\n"
	path := paths.GitRootRelativePath("doc.md")
	content := []byte(text)

	chunks := chunk.ChunkMarkdown(text, string(path), chunk.TargetChunkSize)
	rawSpans, err := resolveSpans(path, content, chunks)
	if err != nil {
		t.Fatal(err)
	}
	spans := make([]byteSpan, len(rawSpans))
	for i, s := range rawSpans {
		spans[i] = byteSpan{Start: s[0], End: s[1]}
	}

	recons, err := Reconstruct(path, content, spans)
	if err != nil {
		t.Fatal(err)
	}
	comment := filetype.LineComment(string(path))
	for i, c := range chunks {
		if recons[i].Text != c.Text {
			t.Fatalf("chunk %d text mismatch: %q vs %q", i, recons[i].Text, c.Text)
		}
		if recons[i].HeadingContext != c.HeadingContext {
			t.Fatalf("chunk %d breadcrumb: %q vs %q", i, recons[i].HeadingContext, c.HeadingContext)
		}
		want := Contextualize(comment, c.HeadingContext, c.Text)
		if recons[i].Contextualized != want {
			t.Fatalf("chunk %d contextualized: %q vs %q", i, recons[i].Contextualized, want)
		}
	}
}

func TestReconstructCodeEnclosingDefinition(t *testing.T) {
	src := "package p\n\nfunc Outer() {\n\tx := 1\n\t_ = x\n}\n"
	path := paths.GitRootRelativePath("p.go")
	content := []byte(src)

	start := strings.Index(src, "x := 1")
	span := byteSpan{Start: start, End: start + len("x := 1")}

	recons, err := Reconstruct(path, content, []byteSpan{span})
	if err != nil {
		t.Fatal(err)
	}
	got := recons[0]
	if got.Text != "x := 1" {
		t.Fatalf("text: %q", got.Text)
	}
	if got.HeadingContext != "p.go > function Outer" {
		t.Fatalf("breadcrumb: %q", got.HeadingContext)
	}
	if got.Start != chunk.PosFromByte(content, span.Start) {
		t.Fatalf("start pos: %+v", got.Start)
	}
	want := Contextualize(filetype.LineComment("p.go"), "p.go > function Outer", "x := 1")
	if got.Contextualized != want {
		t.Fatalf("contextualized: %q vs %q", got.Contextualized, want)
	}
}

func TestReconstructOutOfRangeSpanFails(t *testing.T) {
	path := paths.GitRootRelativePath("p.go")
	content := []byte("package p\n")
	_, err := Reconstruct(path, content, []byteSpan{{Start: 0, End: len(content) + 5}})
	if err == nil {
		t.Fatal("expected out-of-range error")
	}
}

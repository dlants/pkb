package chunk

import (
	"strings"
	"testing"
)

func TestChunkMarkdownEmpty(t *testing.T) {
	if got := ChunkMarkdown("   \n\n", "doc.md", TargetChunkSize); got != nil {
		t.Fatalf("expected nil chunks, got %v", got)
	}
}

func TestChunkMarkdownHeadingContext(t *testing.T) {
	md := "# Top\n\nintro paragraph\n\n## Sub\n\nnested paragraph"
	chunks := ChunkMarkdown(md, "docs/guide.md", TargetChunkSize)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].HeadingContext != "docs/guide.md > # Top" {
		t.Fatalf("chunk 0 heading: %q", chunks[0].HeadingContext)
	}
	if chunks[1].HeadingContext != "docs/guide.md > # Top > ## Sub" {
		t.Fatalf("chunk 1 heading: %q", chunks[1].HeadingContext)
	}
	if !strings.Contains(chunks[1].Text, "nested paragraph") {
		t.Fatalf("chunk 1 text: %q", chunks[1].Text)
	}
}

func TestChunkMarkdownSplitsLargeBlock(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Big\n\n")
	for i := 0; i < 200; i++ {
		b.WriteString("This is a sentence that adds bulk to the document. ")
		if i%5 == 0 {
			b.WriteString("\n\n")
		}
	}
	chunks := ChunkMarkdown(b.String(), "big.md", 500)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
}

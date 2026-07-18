package chunk

import (
	"strings"
	"testing"
)

func TestCodeBreadcrumberEnclosingSpan(t *testing.T) {
	src := "const top = 1;\n" +
		"export function foo() {\n" +
		"  const x = 1;\n" +
		"  return x;\n" +
		"}\n"
	b, err := NewCodeBreadcrumber([]byte(src), "typescript", "a/b.ts")
	if err != nil {
		t.Fatal(err)
	}

	inside := strings.Index(src, "const x = 1;")
	if got := b.Breadcrumb(inside, inside+len("const x = 1;")); got != "a/b.ts > function foo" {
		t.Fatalf("inside foo: got %q", got)
	}

	top := strings.Index(src, "const top = 1;")
	if got := b.Breadcrumb(top, top+len("const top = 1;")); got != "a/b.ts" {
		t.Fatalf("top-level: got %q", got)
	}
}

func TestCodeBreadcrumberNested(t *testing.T) {
	src := "export class Foo {\n" +
		"  alpha() {\n" +
		"    return 1;\n" +
		"  }\n" +
		"}\n"
	b, err := NewCodeBreadcrumber([]byte(src), "typescript", "c.ts")
	if err != nil {
		t.Fatal(err)
	}
	inside := strings.Index(src, "return 1;")
	if got := b.Breadcrumb(inside, inside+len("return 1;")); got != "c.ts > class Foo > method alpha" {
		t.Fatalf("nested: got %q", got)
	}
}

func TestCodeBreadcrumberUnknownGrammar(t *testing.T) {
	b, err := NewCodeBreadcrumber([]byte("hello world"), "cobol", "x.cbl")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Breadcrumb(0, 5); got != "x.cbl" {
		t.Fatalf("fallback: got %q", got)
	}
}

func TestMarkdownBreadcrumbMatchesChunkMarkdown(t *testing.T) {
	text := "# Title\n\nintro para\n\n## Sub\n\nbody para\n"
	chunks := ChunkMarkdown(text, "doc.md", TargetChunkSize)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	for _, c := range chunks {
		off := strings.Index(text, c.Text)
		if off < 0 {
			t.Fatalf("chunk text not found: %q", c.Text)
		}
		if got := MarkdownBreadcrumb(text, "doc.md", off); got != c.HeadingContext {
			t.Fatalf("breadcrumb mismatch for %q: got %q want %q", c.Text, got, c.HeadingContext)
		}
	}
}

func TestMarkdownBreadcrumbBeforeAnyHeading(t *testing.T) {
	text := "intro\n\n# H\n\nbody\n"
	if got := MarkdownBreadcrumb(text, "doc.md", 0); got != "doc.md" {
		t.Fatalf("pre-heading: got %q", got)
	}
}

func TestPosFromByteMatchesPositionAtOffset(t *testing.T) {
	src := "ab\ncde\nf"
	off := strings.Index(src, "cde")
	got := PosFromByte([]byte(src), off)
	want := positionAtOffset(src, Position{Line: 1, Col: 1}, off)
	if got != want {
		t.Fatalf("pos mismatch: got %+v want %+v", got, want)
	}
	if got.Line != 2 || got.Col != 1 {
		t.Fatalf("unexpected pos %+v", got)
	}
}

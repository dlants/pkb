package chunk

import (
	"strings"
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

func TestChunkCodeTwoFunctions(t *testing.T) {
	src := "function foo() {\n  return 1;\n}\n\nfunction bar() {\n  return 2;\n}\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "a/b.ts", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %+v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0].Text, "foo") || chunks[0].HeadingContext != "a/b.ts > function foo" {
		t.Fatalf("chunk0: %q ctx=%q", chunks[0].Text, chunks[0].HeadingContext)
	}
	if chunks[1].HeadingContext != "a/b.ts > function bar" {
		t.Fatalf("chunk1 ctx=%q", chunks[1].HeadingContext)
	}
	if chunks[0].Start.Line != 1 || chunks[1].Start.Line != 5 {
		t.Fatalf("positions: %+v %+v", chunks[0].Start, chunks[1].Start)
	}
}

func TestChunkCodeTopLevelFiller(t *testing.T) {
	src := "import { x } from \"y\";\nimport { z } from \"w\";\n\nfunction foo() {\n  return 1;\n}\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "f.ts", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (filler + func), got %d: %+v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0].Text, "import") || chunks[0].HeadingContext != "f.ts" {
		t.Fatalf("filler chunk: %q ctx=%q", chunks[0].Text, chunks[0].HeadingContext)
	}
}

func TestChunkCodeOversizedClassSplitsMethods(t *testing.T) {
	body := strings.Repeat("    this.x += 1;\n", 80)
	src := "class Foo {\n  alpha() {\n" + body + "  }\n\n  beta() {\n" + body + "  }\n}\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "c.ts", 500)
	if err != nil {
		t.Fatal(err)
	}
	var alpha, beta bool
	for _, c := range chunks {
		if c.HeadingContext == "c.ts > class Foo > method alpha" {
			alpha = true
		}
		if c.HeadingContext == "c.ts > class Foo > method beta" {
			beta = true
		}
	}
	if !alpha || !beta {
		t.Fatalf("expected per-method chunks, got: %+v", breadcrumbs(chunks))
	}
}

func TestChunkCodeOversizedFunctionLineSplit(t *testing.T) {
	body := strings.Repeat("  doThing();\n", 100)
	src := "function big() {\n" + body + "}\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "d.ts", 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected line-split into multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.HeadingContext != "d.ts > function big" {
			t.Fatalf("unexpected ctx %q", c.HeadingContext)
		}
		if len(c.Text) > 300 {
			t.Fatalf("chunk over budget: %d", len(c.Text))
		}
	}
}

func TestChunkCodeParseErrorFallback(t *testing.T) {
	src := "this is not valid code ((( \n@@@ <<< \njust some text lines\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "e.ts", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected fallback chunks, got none")
	}
	if chunks[0].HeadingContext != "e.ts" {
		t.Fatalf("expected file-path breadcrumb, got %q", chunks[0].HeadingContext)
	}
}

func TestChunkCodeUnknownGrammarFallback(t *testing.T) {
	src := "some content\nmore content\n"
	chunks, err := ChunkCode([]byte(src), "cobol", "x.cbl", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0].HeadingContext != "x.cbl" {
		t.Fatalf("expected single fallback chunk, got %+v", chunks)
	}
}

func TestChunkCodeDocCommentAttachedToDecl(t *testing.T) {
	src := "package p\n\n// Foo does a thing.\nfunc Foo() int {\n\treturn 1\n}\n"
	chunks, err := ChunkCode([]byte(src), "go", "p.go", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var fooChunk *ChunkInfo
	for i := range chunks {
		if chunks[i].HeadingContext == "p.go > function Foo" {
			fooChunk = &chunks[i]
		}
		if strings.Contains(chunks[i].HeadingContext, "Foo") == false &&
			strings.Contains(chunks[i].Text, "// Foo does a thing.") {
			t.Fatalf("doc comment leaked into filler chunk: %q", chunks[i].HeadingContext)
		}
	}
	if fooChunk == nil {
		t.Fatalf("no Foo chunk: %+v", breadcrumbs(chunks))
	}
	if !strings.Contains(fooChunk.Text, "// Foo does a thing.") {
		t.Fatalf("expected doc comment in Foo chunk, got %q", fooChunk.Text)
	}
	if fooChunk.Start.Line != 3 {
		t.Fatalf("expected Foo chunk to start at doc comment (line 3), got %d", fooChunk.Start.Line)
	}
}

func TestChunkContainerHeuristicFallback(t *testing.T) {
	src := "function foo() {\n  return 1;\n}\n\nfunction bar() {\n  return 2;\n}\n"
	lang := languageFor("typescript")
	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		t.Fatal(err)
	}
	tree := parser.Parse([]byte(src), nil)
	defer tree.Close()

	var out []ChunkInfo
	chunkContainer(tree.RootNode(), []byte(src), "a/b.ts", TargetChunkSize, nil, &out)
	got := breadcrumbs(out)
	want := []string{"a/b.ts > function foo", "a/b.ts > function bar"}
	if len(got) != len(want) {
		t.Fatalf("heuristic fallback breadcrumbs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("heuristic fallback breadcrumbs = %v, want %v", got, want)
		}
	}
}

func breadcrumbs(chunks []ChunkInfo) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.HeadingContext
	}
	return out
}

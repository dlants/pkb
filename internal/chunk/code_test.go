package chunk

import (
	"strings"
	"testing"
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

func TestChunkCodeOversizedClassEmitsHeaderChunk(t *testing.T) {
	body := strings.Repeat("    this.x += 1;\n", 80)
	src := "// Foo does things.\nclass Foo {\n  alpha() {\n" + body + "  }\n\n  beta() {\n" + body + "  }\n}\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "c.ts", 500)
	if err != nil {
		t.Fatal(err)
	}
	var header *ChunkInfo
	for i := range chunks {
		if chunks[i].HeadingContext == "c.ts > class Foo" {
			header = &chunks[i]
		}
	}
	if header == nil {
		t.Fatalf("expected a standalone class header chunk, got: %+v", breadcrumbs(chunks))
	}
	if !strings.Contains(header.Text, "// Foo does things.") || !strings.Contains(header.Text, "class Foo") {
		t.Fatalf("header chunk missing doc/decl: %q", header.Text)
	}
	if strings.Contains(header.Text, "this.x") {
		t.Fatalf("header chunk should not include method bodies: %q", header.Text)
	}
	if header.Start.Line != 1 || header.Start.Col != 1 {
		t.Fatalf("header start = %+v, want line 1 col 1", header.Start)
	}
	if header.End.Line != 2 {
		t.Fatalf("header end = %+v, want line 2 (end of `class Foo {`)", header.End)
	}
}

func TestChunkCodeOversizedClassHeaderChunkNoDoc(t *testing.T) {
	body := strings.Repeat("    this.x += 1;\n", 80)
	src := "class Foo {\n  alpha() {\n" + body + "  }\n\n  beta() {\n" + body + "  }\n}\n"
	chunks, err := ChunkCode([]byte(src), "typescript", "c.ts", 500)
	if err != nil {
		t.Fatal(err)
	}
	var header *ChunkInfo
	for i := range chunks {
		if chunks[i].HeadingContext == "c.ts > class Foo" {
			header = &chunks[i]
		}
	}
	if header == nil {
		t.Fatalf("expected a standalone class header chunk, got: %+v", breadcrumbs(chunks))
	}
	if !strings.Contains(header.Text, "class Foo") || strings.Contains(header.Text, "//") {
		t.Fatalf("no-doc header chunk = %q", header.Text)
	}
	if strings.Contains(header.Text, "this.x") {
		t.Fatalf("header chunk should not include method bodies: %q", header.Text)
	}
	if header.Start.Line != 1 || header.Start.Col != 1 {
		t.Fatalf("header start = %+v, want line 1 col 1", header.Start)
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

func TestChunkCodeDocCommentAttachedToTypeDecl(t *testing.T) {
	// A Go `type X struct` doc comment has no @doc capture (the comment is a
	// sibling of type_declaration, not the captured type_spec). It must still
	// land in the definition's chunk via sweep-driven attachment.
	src := "package p\n\nconst x = 1\n\n// Foo holds things.\ntype Foo struct {\n\tA int\n}\n"
	chunks, err := ChunkCode([]byte(src), "go", "p.go", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var fooChunk *ChunkInfo
	for i := range chunks {
		if chunks[i].HeadingContext == "p.go > type Foo" {
			fooChunk = &chunks[i]
		}
		if chunks[i].HeadingContext == "p.go" && strings.Contains(chunks[i].Text, "// Foo holds things.") {
			t.Fatalf("doc comment leaked into filler chunk: %q", chunks[i].Text)
		}
	}
	if fooChunk == nil {
		t.Fatalf("no Foo chunk: %v", breadcrumbs(chunks))
	}
	if !strings.HasPrefix(strings.TrimSpace(fooChunk.Text), "// Foo holds things.") {
		t.Fatalf("expected doc comment leading Foo chunk, got %q", fooChunk.Text)
	}
}

func TestChunkCodeBlankLineBreaksDocAttachment(t *testing.T) {
	// A blank line between a banner comment and the definition keeps them apart.
	src := "package p\n\n// banner\n\ntype Foo struct {\n\tA int\n}\n"
	chunks, err := ChunkCode([]byte(src), "go", "p.go", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chunks {
		if c.HeadingContext == "p.go > type Foo" && strings.Contains(c.Text, "// banner") {
			t.Fatalf("banner pulled into Foo chunk: %q", c.Text)
		}
	}
}

func TestChunkCodeCommentStaysWithPlainNodeAcrossFlush(t *testing.T) {
	// A comment leading a plain (non-def) statement stays with it across a
	// budget flush boundary.
	var b strings.Builder
	b.WriteString("package p\n\nfunc f() {\n")
	for i := 0; i < 20; i++ {
		b.WriteString("\tdoThing()\n")
	}
	b.WriteString("\t// marker comment here\n\tlastThing()\n}\n")
	chunks, err := ChunkCode([]byte(b.String()), "go", "p.go", 120)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range chunks {
		if strings.Contains(c.Text, "// marker comment here") {
			if !strings.Contains(c.Text, "lastThing()") {
				t.Fatalf("comment split from its statement: %q", c.Text)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("marker comment dropped: %v", breadcrumbs(chunks))
	}
}

func TestChunkCodeTrailingCommentEmitted(t *testing.T) {
	// A trailing comment with no following sibling is still emitted.
	src := "package p\n\nconst x = 1\n\n// trailing note\n"
	chunks, err := ChunkCode([]byte(src), "go", "p.go", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range chunks {
		if strings.Contains(c.Text, "// trailing note") {
			found = true
		}
	}
	if !found {
		t.Fatalf("trailing comment dropped: %v", breadcrumbs(chunks))
	}
}

func TestChunkCodeHCLBlocks(t *testing.T) {
	src := "region = \"us-east-1\"\n\n" +
		"resource \"aws_instance\" \"web\" {\n  ami = \"abc\"\n}\n\n" +
		"variable \"size\" {\n  default = 1\n}\n"
	chunks, err := ChunkCode([]byte(src), "hcl", "main.tf", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	got := breadcrumbs(chunks)
	var sawResource, sawVar, sawFiller bool
	for i, c := range chunks {
		switch got[i] {
		case "main.tf > resource \"aws_instance\" \"web\"":
			sawResource = true
		case "main.tf > variable \"size\"":
			sawVar = true
		case "main.tf":
			if strings.Contains(c.Text, "region") {
				sawFiller = true
			}
		}
	}
	if !sawResource || !sawVar || !sawFiller {
		t.Fatalf("HCL chunking breadcrumbs = %v", got)
	}
}

func TestChunkCodeHCLOversizedBlockLineSplit(t *testing.T) {
	body := strings.Repeat("  attr = \"value\"\n", 80)
	src := "resource \"aws_instance\" \"web\" {\n" + body + "}\n"
	chunks, err := ChunkCode([]byte(src), "hcl", "main.tf", 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected oversized block to line-split, got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if c.HeadingContext != "main.tf > resource \"aws_instance\" \"web\"" {
			t.Fatalf("unexpected breadcrumb %q", c.HeadingContext)
		}
	}
}

func TestChunkCodeGoTypeNotMergedWithImports(t *testing.T) {
	src := "package p\n\nimport (\n\t\"fmt\"\n)\n\ntype State struct {\n\tName string\n}\n"
	chunks, err := ChunkCode([]byte(src), "go", "m.go", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var stateChunk, importChunk *ChunkInfo
	for i := range chunks {
		switch chunks[i].HeadingContext {
		case "m.go > type State":
			stateChunk = &chunks[i]
		case "m.go":
			importChunk = &chunks[i]
		}
	}
	if stateChunk == nil {
		t.Fatalf("no `type State` chunk: %v", breadcrumbs(chunks))
	}
	if !strings.HasPrefix(strings.TrimSpace(stateChunk.Text), "type State struct") {
		t.Fatalf("type chunk missing `type ` keyword: %q", stateChunk.Text)
	}
	if strings.Contains(stateChunk.Text, "import") {
		t.Fatalf("type chunk merged with imports: %q", stateChunk.Text)
	}
	if importChunk == nil || !strings.Contains(importChunk.Text, "import") {
		t.Fatalf("imports not in their own filler chunk: %v", breadcrumbs(chunks))
	}
}

func TestChunkCodeGoGroupedTypes(t *testing.T) {
	src := "package p\n\ntype (\n\tA struct{}\n\tB struct{}\n)\n"
	chunks, err := ChunkCode([]byte(src), "go", "g.go", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var a, b bool
	for _, c := range chunks {
		if c.HeadingContext == "g.go > type A" {
			a = true
		}
		if c.HeadingContext == "g.go > type B" {
			b = true
		}
	}
	if !a || !b {
		t.Fatalf("expected separate chunks for grouped types A and B, got: %v", breadcrumbs(chunks))
	}
}

func TestChunkCodeCASTPacksSiblings(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\nfunc f() {\n")
	for i := 0; i < 60; i++ {
		b.WriteString("\tdoThing()\n")
	}
	b.WriteString("}\n")
	chunks, err := ChunkCode([]byte(b.String()), "go", "p.go", 200)
	if err != nil {
		t.Fatal(err)
	}
	var fnChunks int
	for _, c := range chunks {
		if len(c.Text) > 200 {
			t.Fatalf("chunk over budget (%d): %q", len(c.Text), c.HeadingContext)
		}
		if c.HeadingContext != "p.go > function f" {
			continue
		}
		fnChunks++
		// Each window should end on a node boundary (a full statement), not
		// mid-line.
		if strings.HasSuffix(c.Text, "doThing(") {
			t.Fatalf("chunk split mid-node: %q", c.Text)
		}
	}
	if fnChunks < 2 {
		t.Fatalf("expected oversized function to pack into multiple windows, got %d", fnChunks)
	}
}

// TestChunkCodePythonDecoratorIsFiller documents the first-cut decorator
// behavior: the python tags.scm captures `function_definition`, whose span
// excludes the `decorated_definition` wrapper, so leading decorators land in
// the preceding filler chunk rather than inside the function chunk. This is an
// accepted limitation; pulling decorators into the function span is noted as
// per-language tuning follow-up in the plan.
func TestChunkCodePythonDecoratorIsFiller(t *testing.T) {
	src := "@decorator\ndef f():\n    return 1\n"
	chunks, err := ChunkCode([]byte(src), "python", "a.py", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	var fnChunk, fillerChunk *ChunkInfo
	for i := range chunks {
		switch chunks[i].HeadingContext {
		case "a.py > function f":
			fnChunk = &chunks[i]
		case "a.py":
			fillerChunk = &chunks[i]
		}
	}
	if fnChunk == nil {
		t.Fatalf("no function chunk: %v", breadcrumbs(chunks))
	}
	if strings.Contains(fnChunk.Text, "@decorator") {
		t.Fatalf("decorator unexpectedly inside function chunk: %q", fnChunk.Text)
	}
	if fillerChunk == nil || !strings.Contains(fillerChunk.Text, "@decorator") {
		t.Fatalf("expected decorator in a preceding filler chunk, got: %v", breadcrumbs(chunks))
	}
}

func breadcrumbs(chunks []ChunkInfo) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.HeadingContext
	}
	return out
}

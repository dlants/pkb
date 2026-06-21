//go:build augmentbench

// Prompt-iteration harness for the contextual-retrieval augmentation prompt.
// It is gated behind the `augmentbench` build tag because it talks to a live
// Ollama server; run it while tuning augmentPrompt:
//
//	go test ./internal/index -tags augmentbench -run TestAugmentBench -v
//
// Override the server or models via env:
//
//	PKB_BENCH_URL   (default http://localhost:11434)
//	PKB_BENCH_MODELS (comma-separated, default qwen3:4b-instruct-2507-q4_K_M,qwen3:0.6b)
package index

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dlants/pkb/internal/chunk"
	"github.com/dlants/pkb/internal/infer"
)

// benchDoc is a whole document; every chunk it produces is augmented. The
// documents are deliberately small and varied so the whole run stays under a
// minute while covering different chunk shapes (prose, lists, nested headings).
type benchDoc struct {
	name string
	body string
}

//go:embed testdata/bench_context.md
var benchContext string

func benchDocs() []benchDoc {
	return []benchDoc{
		{name: "context", body: benchContext},
	}
}

func TestAugmentBench(t *testing.T) {
	url := os.Getenv("PKB_BENCH_URL")
	if url == "" {
		url = "http://localhost:11434"
	}
	region := os.Getenv("PKB_BENCH_BEDROCK_REGION")
	profile := os.Getenv("PKB_BENCH_BEDROCK_PROFILE")
	haiku := os.Getenv("PKB_BENCH_HAIKU_MODEL")
	if haiku == "" {
		haiku = "us.anthropic.claude-haiku-4-5-20251001-v1:0"
	}

	// Each entry pairs a display label with a built model. Defaults compare the
	// local Ollama qwen3:4b against Bedrock Haiku.
	type benchModel struct {
		label string
		model infer.InferenceModel
	}
	mk := func(label string, m infer.InferenceModel, err error) benchModel {
		if err != nil {
			t.Fatalf("build model %s: %v", label, err)
		}
		return benchModel{label: label, model: m}
	}
	ollama, err := infer.NewOpenAICompatible(url, "", "qwen3:4b-instruct-2507-q4_K_M")
	bedrock, berr := infer.NewBedrockClaude(context.Background(), region, profile, haiku)
	benchModels := []benchModel{
		mk("qwen3:4b-instruct (ollama)", ollama, err),
		mk("haiku (bedrock)", bedrock, berr),
	}

	docs := benchDocs()

	models := make([]string, len(benchModels))
	built := make([]infer.InferenceModel, len(benchModels))
	for i, bm := range benchModels {
		models[i] = bm.label
		built[i] = bm.model
		// Warm each model so the first timed chunk doesn't eat model-load cost.
		if _, err := bm.model.Complete("ok"); err != nil {
			t.Fatalf("warmup %s: %v", bm.label, err)
		}
	}

	var report strings.Builder
	report.WriteString("# Augmentation prompt bench\n\n")
	report.WriteString("Models: " + strings.Join(models, ", ") + "\n")

	totals := make([]time.Duration, len(models))
	// Group by chunk so each model's blurb sits side by side for comparison.
	for _, d := range docs {
		cs := chunk.ChunkMarkdown(d.body, d.name, chunk.TargetChunkSize)
		for idx, c := range cs {
			fmt.Fprintf(&report, "\n## %s [chunk %d]\n\nbreadcrumb: `%s`\n\n```\n%s\n```\n\n",
				d.name, idx, c.HeadingContext, c.Text)
			for i, m := range built {
				start := time.Now()
				out, err := m.Complete(augmentPrompt(d.body, c.HeadingContext, c.Text))
				elapsed := time.Since(start)
				totals[i] += elapsed
				if err != nil {
					t.Fatalf("%s %s chunk %d: %v", models[i], d.name, idx, err)
				}
				out = stripThinking(out)
				fmt.Fprintf(&report, "- **%s** (%s): %s\n", models[i], elapsed.Round(time.Millisecond), out)
			}
		}
	}

	report.WriteString("\n## Totals\n\n")
	for i, modelID := range models {
		fmt.Fprintf(&report, "- %s: %s\n", modelID, totals[i].Round(time.Millisecond))
	}

	outPath := os.Getenv("PKB_BENCH_OUT")
	if outPath == "" {
		outPath = "augment_bench_report.md"
	}
	if err := os.WriteFile(outPath, []byte(report.String()), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("wrote bench report to %s", outPath)
	t.Log("\n" + report.String())
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return strings.TrimSpace(s)
}

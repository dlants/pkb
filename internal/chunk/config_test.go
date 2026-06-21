package chunk

import (
	"strings"
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// pathAt parses src and returns the config path of the smallest window covering
// the first occurrence of marker.
func pathAt(t *testing.T, grammar, src, marker string) string {
	t.Helper()
	p := tree_sitter.NewParser()
	defer p.Close()
	if err := p.SetLanguage(languageFor(grammar)); err != nil {
		t.Fatalf("set language: %v", err)
	}
	tree := p.Parse([]byte(src), nil)
	defer tree.Close()
	lo := strings.Index(src, marker)
	if lo < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	return configPath(tree.RootNode(), []byte(src), lo, lo+len(marker))
}

func TestConfigPathJSON(t *testing.T) {
	src := `{"deps":{"nested":{"x":{"y":1}}},"list":[10,20,30]}`
	cases := map[string]string{
		`"nested"`: "deps.nested",
		`"y":1`:    "deps.nested.x.y",
		`20`:       "list[1]",
		`30`:       "list[2]",
	}
	for marker, want := range cases {
		if got := pathAt(t, "json", src, marker); got != want {
			t.Errorf("marker %q: got %q want %q", marker, got, want)
		}
	}
}

func TestConfigPathYAML(t *testing.T) {
	src := "services:\n  web:\n    ports:\n      - 80\n      - 443\n"
	cases := map[string]string{
		"web:": "services.web",
		"80":   "services.web.ports[0]",
		"443":  "services.web.ports[1]",
	}
	for marker, want := range cases {
		if got := pathAt(t, "yaml", src, marker); got != want {
			t.Errorf("marker %q: got %q want %q", marker, got, want)
		}
	}
}

func TestConfigPathTOML(t *testing.T) {
	src := "[servers.alpha]\nip = \"1\"\n\n[[items]]\nname = \"a\"\n\n[[items]]\nname = \"b\"\n"
	cases := map[string]string{
		`ip = "1"`:   "servers.alpha.ip",
		`name = "a"`: "items[0].name",
		`name = "b"`: "items[1].name",
	}
	for marker, want := range cases {
		if got := pathAt(t, "toml", src, marker); got != want {
			t.Errorf("marker %q: got %q want %q", marker, got, want)
		}
	}
}

func TestChunkConfigSingleChunkRootBreadcrumb(t *testing.T) {
	chunks, err := ChunkConfig([]byte(`{"a":1,"b":2}`), "json", "f.json", TargetChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks want 1", len(chunks))
	}
	if chunks[0].HeadingContext != "f.json" {
		t.Errorf("root breadcrumb: got %q want %q", chunks[0].HeadingContext, "f.json")
	}
}

func TestChunkConfigPathBreadcrumb(t *testing.T) {
	// A long leaf value forces an isolated chunk whose window is wholly inside
	// services.web.image, so the breadcrumb carries the full structural path.
	val := strings.Repeat("x", 200)
	src := "services:\n  web:\n    image: " + val + "\n  db:\n    image: y\n"
	chunks, err := ChunkConfig([]byte(src), "yaml", "f.yaml", 60)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range chunks {
		if strings.Contains(c.Text, "xxxx") {
			found = true
			if !strings.HasPrefix(c.HeadingContext, "f.yaml > services.web.image") {
				t.Errorf("breadcrumb: got %q want prefix %q", c.HeadingContext, "f.yaml > services.web.image")
			}
		}
	}
	if !found {
		t.Fatal("long-value chunk not found")
	}
}

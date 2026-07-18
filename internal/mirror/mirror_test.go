package mirror

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/dlants/pkb/internal/embed"
)

func fixture() Artifact {
	return Artifact{
		BlobSha:   "deadbeef",
		ModelName: "mock@8",
		Chunks: []Chunk{
			{
				Start:          10,
				End:            42,
				HeadingContext: "pkg > Foo",
				Embedding:      embed.Embedding{0.1, -0.2, 3.5, 0},
			},
			{
				Start:          64,
				End:            68,
				HeadingContext: "",
				Embedding:      embed.Embedding{-1, 2, -3, 4},
			},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	in := fixture()
	meta, vec, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(meta, vec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, in)
	}
}

func TestMetaHasNoChunkText(t *testing.T) {
	meta, _, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	s := string(meta)
	for _, forbidden := range []string{"line one", "solo", "ctx:", "\"text\"", "contextualizedText"} {
		if bytes.Contains(meta, []byte(forbidden)) {
			t.Errorf("meta must not contain %q, got:\n%s", forbidden, s)
		}
	}
	for _, want := range []string{"start", "end", "headingContext"} {
		if !bytes.Contains(meta, []byte(want)) {
			t.Errorf("meta must contain offset field %q, got:\n%s", want, s)
		}
	}
}

func TestOffsetsRoundTrip(t *testing.T) {
	in := fixture()
	meta, vec, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(meta, vec)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range in.Chunks {
		if got.Chunks[i].Start != c.Start || got.Chunks[i].End != c.End {
			t.Errorf("chunk %d offsets: got [%d,%d) want [%d,%d)", i, got.Chunks[i].Start, got.Chunks[i].End, c.Start, c.End)
		}
		if got.Chunks[i].HeadingContext != c.HeadingContext {
			t.Errorf("chunk %d heading: got %q want %q", i, got.Chunks[i].HeadingContext, c.HeadingContext)
		}
	}
}

func TestDeterministic(t *testing.T) {
	in := fixture()
	m1, v1, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	m2, v2, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m1, m2) {
		t.Error("meta bytes not deterministic")
	}
	if !bytes.Equal(v1, v2) {
		t.Error("vec bytes not deterministic")
	}
}

func TestVecStandalone(t *testing.T) {
	in := fixture()
	_, vec, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeVec(vec)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(in.Chunks) {
		t.Fatalf("got %d vectors, want %d", len(got), len(in.Chunks))
	}
	for i, c := range in.Chunks {
		if !reflect.DeepEqual(got[i], c.Embedding) {
			t.Errorf("vector %d mismatch: got %v want %v", i, got[i], c.Embedding)
		}
	}
}

func TestEmptyArtifact(t *testing.T) {
	in := Artifact{BlobSha: "abc", ModelName: "mock@8"}
	meta, vec, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(meta, vec)
	if err != nil {
		t.Fatal(err)
	}
	if got.BlobSha != in.BlobSha || got.ModelName != in.ModelName || len(got.Chunks) != 0 {
		t.Fatalf("empty round-trip mismatch: %+v", got)
	}
}

func TestTornPairDetected(t *testing.T) {
	meta, _, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	_, vec, err := Encode(Artifact{BlobSha: "x", ModelName: "m", Chunks: fixture().Chunks[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(meta, vec); err == nil {
		t.Fatal("expected mismatch error for torn pair")
	}
}

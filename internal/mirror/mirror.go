// Package mirror defines the on-disk mirror-artifact format: the committed,
// per-source-file index record split across two sibling files. For a source
// file `src/foo.ts` the artifact is `src/foo.ts.meta` (diffable text metadata)
// and `src/foo.ts.vec` (packed embeddings). The encoding is deterministic so
// re-embedding a file whose chunks are unchanged yields byte-identical
// artifacts, and the vectors are loadable without decoding the metadata.
package mirror

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"github.com/dlants/pkb/internal/embed"
)

// MetaExt and VecExt are the suffixes appended to a source path to name its two
// sibling mirror files.
const (
	MetaExt = ".meta"
	VecExt  = ".vec"
)

// vecMagic tags the .vec binary format; a version byte follows so the header is
// self-describing and future format changes are detectable.
var vecMagic = [4]byte{'P', 'K', 'B', 'V'}

const vecFormatVersion uint32 = 1

// Chunk is one chunk's record within an artifact: the byte span of the chunk
// within the source file (identified by the artifact's BlobSha), its heading
// breadcrumb, and its embedding. The chunk text and contextualized text are not
// stored; they are reconstructed at cache-sync time by slicing the source blob.
type Chunk struct {
	Start          int // byte offset into the source blob, inclusive
	End            int // byte offset into the source blob, exclusive
	HeadingContext string
	Embedding      embed.Embedding
}

// Artifact is the complete index record for a single source file.
type Artifact struct {
	BlobSha   string
	ModelName string
	Chunks    []Chunk
}

// metaFile is the canonical JSON shape of the .meta file. Field order is fixed
// by the struct, and json.MarshalIndent is deterministic, so equal inputs
// serialize to byte-identical output.
type metaFile struct {
	BlobSha   string      `json:"blobSha"`
	ModelName string      `json:"modelName"`
	Chunks    []metaChunk `json:"chunks"`
}

// metaChunk is offset-first: it stores only the chunk's byte span within the
// source blob plus its heading breadcrumb. The chunk text and contextualized
// text are reconstructed from the blob at sync time, so they are never
// duplicated on disk.
type metaChunk struct {
	Start          int    `json:"start"`
	End            int    `json:"end"`
	HeadingContext string `json:"headingContext"`
}

// EncodeMeta serializes the artifact's file-level fields and per-chunk metadata
// (everything except the embeddings) into the deterministic .meta bytes.
func EncodeMeta(a Artifact) ([]byte, error) {
	m := metaFile{
		BlobSha:   a.BlobSha,
		ModelName: a.ModelName,
		Chunks:    make([]metaChunk, len(a.Chunks)),
	}
	for i, c := range a.Chunks {
		m.Chunks[i] = metaChunk{
			Start:          c.Start,
			End:            c.End,
			HeadingContext: c.HeadingContext,
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// EncodeVec packs the artifact's embeddings into the deterministic .vec bytes:
// a fixed header (magic, format version, dimensions, chunk count) followed by
// row-major little-endian float32 vectors in chunk-ordinal order. All vectors
// must share one dimension count.
func EncodeVec(a Artifact) ([]byte, error) {
	dims := 0
	if len(a.Chunks) > 0 {
		dims = len(a.Chunks[0].Embedding)
	}
	for i, c := range a.Chunks {
		if len(c.Embedding) != dims {
			return nil, fmt.Errorf("mirror: chunk %d has %d dims, expected %d", i, len(c.Embedding), dims)
		}
	}
	out := make([]byte, 0, 16+len(a.Chunks)*dims*4)
	out = append(out, vecMagic[:]...)
	var hdr [12]byte
	binary.LittleEndian.PutUint32(hdr[0:], vecFormatVersion)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(dims))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(a.Chunks)))
	out = append(out, hdr[:]...)
	var buf [4]byte
	for _, c := range a.Chunks {
		for _, f := range c.Embedding {
			binary.LittleEndian.PutUint32(buf[:], math.Float32bits(f))
			out = append(out, buf[:]...)
		}
	}
	return out, nil
}

// Encode serializes an artifact into its two sibling byte streams.
func Encode(a Artifact) (meta, vec []byte, err error) {
	meta, err = EncodeMeta(a)
	if err != nil {
		return nil, nil, err
	}
	vec, err = EncodeVec(a)
	if err != nil {
		return nil, nil, err
	}
	return meta, vec, nil
}

// DecodeVec parses the packed embeddings from .vec bytes without any dependence
// on the metadata. The header is self-describing, so vectors load standalone.
func DecodeVec(b []byte) ([]embed.Embedding, error) {
	if len(b) < 16 {
		return nil, fmt.Errorf("mirror: vec too short (%d bytes)", len(b))
	}
	if [4]byte{b[0], b[1], b[2], b[3]} != vecMagic {
		return nil, fmt.Errorf("mirror: bad vec magic")
	}
	if v := binary.LittleEndian.Uint32(b[4:]); v != vecFormatVersion {
		return nil, fmt.Errorf("mirror: unsupported vec format version %d", v)
	}
	dims := int(binary.LittleEndian.Uint32(b[8:]))
	count := int(binary.LittleEndian.Uint32(b[12:]))
	need := 16 + count*dims*4
	if len(b) != need {
		return nil, fmt.Errorf("mirror: vec length %d, expected %d", len(b), need)
	}
	out := make([]embed.Embedding, count)
	off := 16
	for i := range out {
		e := make(embed.Embedding, dims)
		for j := range e {
			e[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[off:]))
			off += 4
		}
		out[i] = e
	}
	return out, nil
}

// DecodeMeta parses the .meta bytes back into an artifact with no embeddings
// populated (Chunk.Embedding is nil).
func DecodeMeta(b []byte) (Artifact, error) {
	var m metaFile
	if err := json.Unmarshal(b, &m); err != nil {
		return Artifact{}, err
	}
	a := Artifact{
		BlobSha:   m.BlobSha,
		ModelName: m.ModelName,
		Chunks:    make([]Chunk, len(m.Chunks)),
	}
	for i, mc := range m.Chunks {
		a.Chunks[i] = Chunk{
			Start:          mc.Start,
			End:            mc.End,
			HeadingContext: mc.HeadingContext,
		}
	}
	return a, nil
}

// Decode reconstructs a full artifact from its two sibling byte streams,
// aligning each embedding to its metadata record by ordinal. It errors if the
// counts disagree, which is how a torn write is caught.
func Decode(meta, vec []byte) (Artifact, error) {
	a, err := DecodeMeta(meta)
	if err != nil {
		return Artifact{}, err
	}
	embeddings, err := DecodeVec(vec)
	if err != nil {
		return Artifact{}, err
	}
	if len(embeddings) != len(a.Chunks) {
		return Artifact{}, fmt.Errorf("mirror: %d vectors but %d metadata chunks", len(embeddings), len(a.Chunks))
	}
	for i := range a.Chunks {
		a.Chunks[i].Embedding = embeddings[i]
	}
	return a, nil
}

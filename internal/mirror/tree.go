// This file adds the on-disk mirror tree: filesystem read/write/enumerate of
// per-source-file artifacts rooted at `.pkb/index/` under the repo root. The
// tree is the committed source of truth for the index; the SQLite store is a
// derived cache synced from it.
package mirror

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/dlants/pkb/internal/paths"
)

// IndexDir is the repo-root-relative directory holding the mirror tree.
const IndexDir = ".pkb/index"

// Tree provides filesystem access to the mirror artifacts under a repo root.
type Tree struct {
	root paths.AbsPath
}

// NewTree returns a Tree rooted at the given absolute repo root.
func NewTree(repoRoot paths.AbsPath) *Tree { return &Tree{root: repoRoot} }

// dir is the absolute path of the mirror tree root.
func (t *Tree) dir() string {
	return filepath.Join(string(t.root), filepath.FromSlash(IndexDir))
}

// base is the absolute path prefix (without extension) for a source path's
// artifacts.
func (t *Tree) base(rel paths.GitRootRelativePath) string {
	return filepath.Join(t.dir(), filepath.FromSlash(string(rel)))
}

func (t *Tree) metaPath(rel paths.GitRootRelativePath) string { return t.base(rel) + MetaExt }
func (t *Tree) vecPath(rel paths.GitRootRelativePath) string  { return t.base(rel) + VecExt }

// Entry is the lightweight per-artifact record produced by List: enough to
// decide staleness and report counts without loading vectors.
type Entry struct {
	BlobSha   string
	ModelName string
	Chunks    int
}

// List enumerates every artifact in the tree by its source path, reading only
// the `.meta` sibling. A missing tree yields an empty map.
func (t *Tree) List() (map[paths.GitRootRelativePath]Entry, error) {
	dir := t.dir()
	out := map[paths.GitRootRelativePath]Entry{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, MetaExt) {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		a, derr := DecodeMeta(b)
		if derr != nil {
			return derr
		}
		relOS := strings.TrimSuffix(strings.TrimPrefix(p, dir+string(filepath.Separator)), MetaExt)
		rel := paths.GitRootRelativePath(filepath.ToSlash(relOS))
		out[rel] = Entry{BlobSha: a.BlobSha, ModelName: a.ModelName, Chunks: len(a.Chunks)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TryRead loads a full artifact (metadata + vectors) for a source path. The
// second result is false when the artifact is absent or torn/corrupt (a torn
// pair or decode failure is reported as "not present" so the caller simply
// reindexes the file); only unexpected IO errors are returned.
func (t *Tree) TryRead(rel paths.GitRootRelativePath) (Artifact, bool, error) {
	meta, err := os.ReadFile(t.metaPath(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, err
	}
	vec, err := os.ReadFile(t.vecPath(rel))
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, err
	}
	a, derr := Decode(meta, vec)
	if derr != nil {
		return Artifact{}, false, nil
	}
	return a, true, nil
}

// Write persists an artifact's two sibling files atomically (temp file +
// rename each), creating parent directories as needed. Each file is written
// atomically so a crash leaves previously written artifacts intact.
func (t *Tree) Write(rel paths.GitRootRelativePath, a Artifact) error {
	meta, vec, err := Encode(a)
	if err != nil {
		return err
	}
	if err := writeAtomic(t.metaPath(rel), meta); err != nil {
		return err
	}
	return writeAtomic(t.vecPath(rel), vec)
}

// Delete removes both sibling files for a source path, ignoring absent files.
func (t *Tree) Delete(rel paths.GitRootRelativePath) error {
	if err := os.Remove(t.metaPath(rel)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(t.vecPath(rel)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

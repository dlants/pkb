// Package paths defines nominal path types so the compiler distinguishes a
// path relative to the git repository root (as produced by git plumbing and
// stored in the index) from an absolute filesystem path (used when touching the
// working tree or reporting results to a user).
package paths

import "path/filepath"

// GitRootRelativePath is a forward-slashed path relative to the git repository
// root, exactly as emitted by `git ls-tree` / `git diff`. This is the canonical
// identity stored in the index, so it is stable across checkouts and machines.
type GitRootRelativePath string

// AbsPath is an absolute filesystem path.
type AbsPath string

// Join resolves a root-relative path against an absolute repo root, producing
// an absolute path suitable for filesystem access or for reporting to a client.
func (root AbsPath) Join(rel GitRootRelativePath) AbsPath {
	return AbsPath(filepath.Join(string(root), filepath.FromSlash(string(rel))))
}

func (p GitRootRelativePath) String() string { return string(p) }

func (p AbsPath) String() string { return string(p) }

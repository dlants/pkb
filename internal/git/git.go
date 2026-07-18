// Package git wraps the subset of git plumbing PKB needs to discover files and
// detect changes: repo root resolution, ref/sha resolution, tree listing, and
// materializing the staging area as a tree.
package git

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/dlants/pkb/internal/paths"
)

// Repo is a handle to a git working tree rooted at Root.
type Repo struct {
	Root paths.AbsPath
}

// RepoFile is a single tracked file: its repo-relative path and git blob sha.
type RepoFile struct {
	Path    paths.GitRootRelativePath
	BlobSha string
}

func (r *Repo) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = string(r.Root)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// Open finds the repo root containing dir and returns a Repo handle.
func Open(dir string) (*Repo, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository (or git not found): %w", err)
	}
	return &Repo{Root: paths.AbsPath(strings.TrimSpace(string(out)))}, nil
}

// ResolveRef resolves a ref (e.g. HEAD, a branch, a sha) to a full commit sha.
func (r *Repo) ResolveRef(ref string) (string, error) {
	out, err := r.run("rev-parse", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// LsTree lists all files tracked at ref with their blob shas.
func (r *Repo) LsTree(ref string) ([]RepoFile, error) {
	out, err := r.run("ls-tree", "-r", ref)
	if err != nil {
		return nil, err
	}
	var files []RepoFile
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// format: "<mode> <type> <sha>\t<path>"
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(line[:tab])
		if len(meta) < 3 || meta[1] != "blob" {
			continue
		}
		files = append(files, RepoFile{Path: paths.GitRootRelativePath(line[tab+1:]), BlobSha: meta[2]})
	}
	return files, nil
}

// CatBlob returns the raw bytes of a blob object by its sha. It reads from the
// object database, never the working tree, so offsets recorded against a blob
// resolve against exactly that content regardless of the working tree state.
func (r *Repo) CatBlob(sha string) ([]byte, error) {
	cmd := exec.Command("git", "cat-file", "blob", sha)
	cmd.Dir = string(r.Root)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file blob %s: %v: %s", sha, err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// WriteTree writes the current staging area (index) to the object database as a
// tree and returns its sha. It does not create a commit or mutate the working
// tree or index. The returned sha is a tree-ish consumable by LsTree.
func (r *Repo) WriteTree() (string, error) {
	out, err := r.run("write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Package git wraps the subset of git plumbing PKB needs to discover files and
// detect changes: repo root resolution, ref/sha resolution, tree listing,
// name-status diffs, object reachability, and merge-base queries.
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

// Change is one entry of `git diff --name-status`.
type Change struct {
	// Status is the first letter of the status code (A, M, D, R, C, T).
	Status  string
	Path    paths.GitRootRelativePath
	OldPath paths.GitRootRelativePath // set only for renames/copies
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

// DiffNameStatus returns the changes between two commits.
func (r *Repo) DiffNameStatus(from, to string) ([]Change, error) {
	out, err := r.run("diff", "--name-status", from, to)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		code := status[:1]
		switch code {
		case "R", "C":
			if len(fields) < 3 {
				continue
			}
			changes = append(changes, Change{Status: code, OldPath: paths.GitRootRelativePath(fields[1]), Path: paths.GitRootRelativePath(fields[2])})
		default:
			changes = append(changes, Change{Status: code, Path: paths.GitRootRelativePath(fields[1])})
		}
	}
	return changes, nil
}

// ObjectExists reports whether the given object sha is present in the repo.
func (r *Repo) ObjectExists(sha string) bool {
	cmd := exec.Command("git", "cat-file", "-e", sha)
	cmd.Dir = string(r.Root)
	return cmd.Run() == nil
}

// IsAncestor reports whether commit a is an ancestor of commit b.
func (r *Repo) IsAncestor(a, b string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = string(r.Root)
	return cmd.Run() == nil
}

// MergeBase returns the best common ancestor of a and b.
func (r *Repo) MergeBase(a, b string) (string, error) {
	out, err := r.run("merge-base", a, b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

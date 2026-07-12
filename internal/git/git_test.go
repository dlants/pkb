package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestWriteTreeListsStagedFiles(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q")

	if err := os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "foo.txt")

	repo, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	statusBefore := gitCmd(t, dir, "status", "--porcelain")

	treeSha, err := repo.WriteTree()
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	if treeSha == "" {
		t.Fatal("WriteTree returned empty sha")
	}

	files, err := repo.LsTree(treeSha)
	if err != nil {
		t.Fatalf("LsTree: %v", err)
	}
	if len(files) != 1 || string(files[0].Path) != "foo.txt" {
		t.Fatalf("unexpected files: %+v", files)
	}
	if files[0].BlobSha == "" {
		t.Fatal("empty blob sha")
	}

	if statusAfter := gitCmd(t, dir, "status", "--porcelain"); statusAfter != statusBefore {
		t.Fatalf("status changed: before=%q after=%q", statusBefore, statusAfter)
	}
}

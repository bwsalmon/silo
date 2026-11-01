package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRepository_LocalDir(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "local-repo")
	if err := os.Mkdir(repoDir, 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	res, err := resolveRepository(repoDir, tmpDir)
	if err != nil {
		t.Fatalf("resolveRepository failed: %v", err)
	}
	if res != repoDir {
		t.Errorf("expected %s, got %s", repoDir, res)
	}
}

func TestResolveRepository_NonExistentPath(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistent := filepath.Join(tmpDir, "does-not-exist")

	_, err := resolveRepository(nonExistent, tmpDir)
	if err == nil {
		t.Errorf("expected error for non-existent repo, got nil")
	}
}

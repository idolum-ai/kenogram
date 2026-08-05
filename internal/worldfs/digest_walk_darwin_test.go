//go:build darwin

package worldfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinDigestRejectsInRootDirectorySymlinkReplacement(t *testing.T) {
	workspace := t.TempDir()
	original := filepath.Join(workspace, "original")
	replacement := filepath.Join(workspace, "replacement")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "unexpected"), []byte("outside observation"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	observed, err := root.Lstat("original")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, filepath.Join(workspace, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("replacement", original); err != nil {
		t.Fatal(err)
	}
	state := &digestWalkState{}
	err = walkDigestRootDirectory(context.Background(), root, "original", "original", 1, observed, state)
	if err == nil || !IsChanging(err) || len(state.entries) != 0 {
		t.Fatalf("replacement error=%v entries=%#v", err, state.entries)
	}
}

//go:build linux

package worldfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDigestDirectoryDescriptorRejectsAncestorSymlinkSwap(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "inside"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	observed, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(directory, filepath.Join(workspace, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, directory); err != nil {
		t.Fatal(err)
	}
	state := &digestWalkState{}
	if err := walkDigestChildDirectory(context.Background(), root, "directory", "directory", observed, 0, state); err == nil || !IsChanging(err) {
		t.Fatalf("ancestor symlink swap error = %v entries=%#v", err, state.entries)
	}
}

func TestDigestContextEnforcesObservationWorkLimits(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		limits DigestLimits
		want   string
	}{
		{name: "entries", limits: DigestLimits{MaxEntries: 2}, want: "2 entries"},
		{name: "metadata", limits: DigestLimits{MaxMetadataBytes: 1}, want: "1 metadata bytes"},
		{name: "regular bytes", limits: DigestLimits{MaxFileBytes: 1}, want: "1 regular-file bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DigestContextWithLimits(context.Background(), workspace, test.limits)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDigestRejectsDistinctInvalidUTF8NamesBeforeHashing(t *testing.T) {
	for _, value := range []byte{0xff, 0xfe, 0x80} {
		t.Run(fmt.Sprintf("byte-%x", value), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, string([]byte{value})), []byte("content"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tree, err := Digest(workspace); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
				t.Fatalf("digest = %#v, error = %v", tree, err)
			}
		})
	}
}

func TestDigestRejectsInvalidUTF8NameBeforeFileWorkAccounting(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, string([]byte{0xff})), []byte("too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := DigestContextWithLimits(context.Background(), workspace, DigestLimits{MaxFileBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("error = %v", err)
	}
}

func TestDigestRejectsInvalidUTF8SymlinkTarget(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Symlink(string([]byte{0xff}), filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if tree, err := Digest(workspace); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("digest = %#v, error = %v", tree, err)
	}
}

func TestDigestPersistsGenuineReplacementRuneName(t *testing.T) {
	workspace := t.TempDir()
	wantPath := "\uFFFD"
	if err := os.WriteFile(filepath.Join(workspace, wantPath), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := Digest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	layout := For(t.TempDir(), "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.WriteDigest(1, tree); err != nil {
		t.Fatal(err)
	}
	read, err := layout.ReadDigest(1)
	if err != nil {
		t.Fatal(err)
	}
	if read.Root != tree.Root || len(read.Entries) != 2 || read.Entries[1].Path != wantPath {
		t.Fatalf("persisted digest = %#v, want path %q and root %s", read, wantPath, tree.Root)
	}
}

//go:build linux

package worldfs

import (
	"context"
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

package sourcetree

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSharedWalkerEnforcesEveryTreeBound(t *testing.T) {
	t.Run("20001 entries", func(t *testing.T) {
		root := t.TempDir()
		for index := int64(1); index < MaxEntries+1; index++ {
			path := filepath.Join(root, entryName(index))
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := Inspect(context.Background(), root, nil); err == nil || !strings.Contains(err.Error(), "20000 entries") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("over byte", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sparse")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(MaxBytes + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		if err := Inspect(context.Background(), path, nil); err == nil || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("over depth", func(t *testing.T) {
		root := t.TempDir()
		current := root
		for index := 0; index < MaxDepth+1; index++ {
			current = filepath.Join(current, "d")
			if err := os.Mkdir(current, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := Inspect(context.Background(), root, nil); err == nil || !strings.Contains(err.Error(), "path exceeds bound") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("special", func(t *testing.T) {
		root, err := os.MkdirTemp("/tmp", "kenogram-tree-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		path := filepath.Join(root, "innocuous.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := Inspect(context.Background(), filepath.Dir(path), nil); err == nil || !strings.Contains(err.Error(), "special node") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCanceledCopyLeavesNoPartialSnapshot(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "input"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Copy(ctx, source, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("partial snapshot remains: %v", err)
	}
}

func TestCopyDoesNotRemovePreexistingTarget(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("preexisting"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Copy(context.Background(), source, target); err == nil {
		t.Fatal("preexisting target was replaced")
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "preexisting" {
		t.Fatalf("target=%q error=%v", raw, err)
	}
}

func TestCopyRejectsSpecialNodesWithoutPartialTarget(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func(string) error
	}{
		{name: "symlink", build: func(root string) error { return os.Symlink("ordinary", filepath.Join(root, "z-special")) }},
		{name: "fifo", build: func(root string) error { return syscall.Mkfifo(filepath.Join(root, "z-special"), 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "ordinary"), []byte("ordinary"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.build(source); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "snapshot")
			if err := Copy(context.Background(), source, target); err == nil || !strings.Contains(err.Error(), "non-regular") {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("partial target remains: %v", err)
			}
		})
	}
}

func entryName(index int64) string {
	const digits = "0123456789abcdef"
	buffer := []byte("entry-000000")
	for position := len(buffer) - 1; index > 0; position-- {
		buffer[position] = digits[index&15]
		index >>= 4
	}
	return string(buffer)
}

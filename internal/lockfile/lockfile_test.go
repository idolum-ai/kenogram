package lockfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireExclusiveAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("second acquisition succeeded")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireReclaimsStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world.lock")
	if err := os.WriteFile(path, []byte("999999 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
}

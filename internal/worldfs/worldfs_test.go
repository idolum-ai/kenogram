package worldfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateAppliedAndGeneration(t *testing.T) {
	l := For(t.TempDir(), "x")
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if l.NextGeneration() != 1 {
		t.Fatal("first generation")
	}
	s := State{Name: "x", Generation: 4}
	if err := l.WriteState(s); err != nil {
		t.Fatal(err)
	}
	if l.NextGeneration() != 5 {
		t.Fatal("next generation")
	}
	if err := l.WriteApplied([]byte("version = 1\n")); err != nil {
		t.Fatal(err)
	}
}
func TestDigestDeterministicAndDrift(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Root != second.Root {
		t.Fatal("nondeterministic")
	}
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	if ChangedFiles(first, third) != 1 {
		t.Fatalf("changed=%d", ChangedFiles(first, third))
	}
}

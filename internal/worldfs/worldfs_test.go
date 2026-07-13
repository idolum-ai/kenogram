package worldfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/idolum-ai/kenogram/internal/plan"
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
	for _, entry := range first.Entries {
		if entry.Type == "directory" && entry.Size != 0 {
			t.Fatalf("directory %q contributes filesystem size %d", entry.Path, entry.Size)
		}
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

func TestDigestRetriesAChangingWorkspace(t *testing.T) {
	want := DigestTree{Root: "stable"}
	attempts := 0
	got, err := digestRetry("workspace", func(root string) (DigestTree, error) {
		attempts++
		if root != "workspace" {
			t.Fatalf("root = %q", root)
		}
		if attempts < 3 {
			return DigestTree{}, &treeChangedError{path: "state.sqlite-wal"}
		}
		return want, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != want.Root || attempts != 3 {
		t.Fatalf("digest = %#v after %d attempts", got, attempts)
	}
}

func TestDigestDoesNotRetryPermanentErrors(t *testing.T) {
	attempts := 0
	_, err := digestRetry("workspace", func(string) (DigestTree, error) {
		attempts++
		return DigestTree{}, os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) || attempts != 1 {
		t.Fatalf("err = %v after %d attempts", err, attempts)
	}
}

func TestTransitionRoundTripAndClear(t *testing.T) {
	l := For(t.TempDir(), "x")
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	want := Transition{Version: 1, Phase: "rollback", Prior: &State{Name: "x", Generation: 1}, Successor: State{Name: "x", Generation: 2}, SuccessorDeclaration: []byte("version = 1\n")}
	if err := l.WriteTransition(want); err != nil {
		t.Fatal(err)
	}
	got, err := l.ReadTransition()
	if err != nil || got.Prior.Generation != 1 || got.Successor.Generation != 2 {
		t.Fatalf("transition=%#v err=%v", got, err)
	}
	if err := l.ClearTransition(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReadTransition(); !os.IsNotExist(err) {
		t.Fatalf("transition remains: %v", err)
	}
}

func TestStagingPreservesSourceIdentityBeforeTargetMode(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := For(t.TempDir(), "x")
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	stage, err := l.StageSource(1, 0, source, "0644")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(stage)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("staged source mode=%v", info.Mode().Perm())
	}
	sourceDigest, err := plan.DigestSource(source)
	if err != nil {
		t.Fatal(err)
	}
	stagedDigest, err := plan.DigestSource(stage)
	if err != nil {
		t.Fatal(err)
	}
	if stagedDigest != sourceDigest {
		t.Fatalf("staged source digest = %s, want %s", stagedDigest, sourceDigest)
	}
	if err := l.ApplyStageMode(stage, "0644"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(stage)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("materialized target mode=%v", info.Mode().Perm())
	}
}

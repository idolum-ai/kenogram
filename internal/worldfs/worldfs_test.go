package worldfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/kenogram/internal/plan"
)

func TestBaseDirUsesExplicitStateOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom", "worlds")
	t.Setenv("KENOGRAM_STATE_DIR", want)
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "ignored"))
	got, err := BaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("BaseDir() = %q, want %q", got, want)
	}
}

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

func TestReadDigestRejectsNoncanonicalEvidence(t *testing.T) {
	rootEntry := DigestEntry{Path: "", Type: "directory", Mode: 0o700}
	validFile := DigestEntry{Path: "a", Type: "file", Mode: 0o600, Size: 1, SHA256: strings.Repeat("a", 64)}
	withRoot := func(entries []DigestEntry) DigestTree {
		root, err := digestEntriesRoot(entries)
		if err != nil {
			t.Fatal(err)
		}
		return DigestTree{Root: root, Entries: entries}
	}
	tests := []struct {
		name string
		tree DigestTree
	}{
		{name: "empty", tree: DigestTree{}},
		{name: "root mismatch", tree: DigestTree{Root: strings.Repeat("0", 64), Entries: []DigestEntry{rootEntry}}},
		{name: "duplicate path", tree: withRoot([]DigestEntry{rootEntry, validFile, validFile})},
		{name: "noncanonical order", tree: withRoot([]DigestEntry{rootEntry, {Path: "b", Type: "directory"}, {Path: "a", Type: "directory"}})},
		{name: "noncanonical path", tree: withRoot([]DigestEntry{rootEntry, {Path: "../escape", Type: "directory"}})},
		{name: "invalid file digest", tree: withRoot([]DigestEntry{rootEntry, {Path: "a", Type: "file", Mode: 0o600, Size: 1, SHA256: "not-a-digest"}})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := For(t.TempDir(), "w")
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			if _, err := layout.WriteDigest(1, test.tree); err != nil {
				t.Fatal(err)
			}
			if _, err := layout.ReadDigest(1); err == nil {
				t.Fatalf("accepted digest tree %#v", test.tree)
			}
		})
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

func TestDigestClassifiesChangingWorkspace(t *testing.T) {
	_, err := digestRetry("workspace", func(string) (DigestTree, error) {
		return DigestTree{}, &treeChangedError{path: "workspace/live.db-wal"}
	})
	if !IsChanging(err) || !errors.Is(err, ErrWorkspaceChanging) {
		t.Fatalf("changing workspace error was not classified: %v", err)
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

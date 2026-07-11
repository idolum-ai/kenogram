package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDryRunExample(t *testing.T) {
	root := repoRoot(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", "--dry-run", filepath.Join(root, "kenogram.example.toml")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Kenogram plan") || !strings.Contains(stdout.String(), "plan digest:") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestUpWithoutConfirmationIsHonest(t *testing.T) {
	root := repoRoot(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", filepath.Join(root, "kenogram.example.toml")}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "without --yes") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestJSONDryRun(t *testing.T) {
	root := repoRoot(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", "--dry-run", "--json", filepath.Join(root, "kenogram.example.toml")}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"plan_digest"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("repository root not found")
		}
		wd = parent
	}
}

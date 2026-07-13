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

func TestSubcommandHelpIsSuccessful(t *testing.T) {
	for _, command := range []string{"up", "down", "destroy", "enter", "status", "allow", "revoke", "repair-history", "worlds"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{command, "--help"}, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if code != 0 || !strings.Contains(output, "usage: kenogram "+command) {
				t.Fatalf("code=%d output=%q", code, output)
			}
			if command == "down" && strings.Contains(output, "--yes") {
				t.Fatalf("down help advertises destroy-only confirmation: %q", output)
			}
		})
	}
}

func TestSubcommandUsageErrorsExplainTheFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing status world", args: []string{"status"}, want: "usage: kenogram status"},
		{name: "invalid status world", args: []string{"status", "INVALID!"}, want: "invalid world name"},
		{name: "missing enter world", args: []string{"enter"}, want: "usage: kenogram enter"},
		{name: "extra worlds argument", args: []string{"worlds", "extra"}, want: "usage: kenogram worlds"},
		{name: "down rejects destroy flag", args: []string{"down", "--yes", "world"}, want: "flag provided but not defined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestDryRunRejectsMountContainingStateRoot(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("KENOGRAM_STATE_DIR", state)
	declaration := filepath.Join(root, "dangerous.toml")
	raw := `version = 1
name = "dry-run-mount"
[world]
hostname = "dry-run-mount"
base = "example@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "0"
[resources]
cpus = 1
memory_bytes = 268435456
pids = 32
[workspace]
paths = ["/workspace"]
[[mounts]]
source = "."
target = "/host"
mode = "ro"
`
	if err := os.WriteFile(declaration, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", "--dry-run", declaration}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "protected host path") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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

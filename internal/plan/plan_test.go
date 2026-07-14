package plan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/kenogram/internal/decl"
)

func fixture(t *testing.T, comment string) (decl.Declaration, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte("version = 1 " + comment + "\nname = \"x\"\n")
	d := decl.Declaration{Version: 1, Name: "x", World: decl.World{Hostname: "x", Base: "base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Workdir: "/workspace", User: "agent"}, Resources: decl.Resources{CPUs: 1, MemoryBytes: 2, PIDs: 3}, Workspace: decl.Workspace{Paths: []string{"/workspace"}}, Mounts: []decl.Mount{{Source: "repo", Target: "/workspace/repo", Mode: "rw"}}}
	return d, filepath.Join(dir, "kenogram.toml"), data
}

func TestBuildDigestSeparatesSemanticsFromProvenance(t *testing.T) {
	d, path, firstBytes := fixture(t, "# one")
	first, err := Build(d, path, firstBytes)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(d, path, []byte("# differently formatted\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanDigest != second.PlanDigest {
		t.Fatalf("semantic digest changed: %s != %s", first.PlanDigest, second.PlanDigest)
	}
	if first.DeclarationDigest == second.DeclarationDigest {
		t.Fatal("byte provenance digest did not change")
	}
	if first.Plan.Mounts[0].Source != filepath.Join(filepath.Dir(path), "repo") {
		t.Fatalf("source not resolved: %s", first.Plan.Mounts[0].Source)
	}
}

func TestBuildWarnsForExplicitlyAllowedUnpinnedImage(t *testing.T) {
	d, path, data := fixture(t, "")
	d.World.Base = "ubuntu:latest"
	d.AllowUnpinned = true
	result, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "UNPINNED") {
		t.Fatalf("warnings: %#v", result.Warnings)
	}
}

func TestBuildDoesNotWarnForExactLocalImageID(t *testing.T) {
	d, path, data := fixture(t, "")
	d.World.Base = "sha256:" + strings.Repeat("b", 64)
	result, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings: %#v", result.Warnings)
	}
}

func TestRenderDoesNotReadOrPrintSourceContents(t *testing.T) {
	d, path, data := fixture(t, "")
	secret := "CONTENT-MUST-NOT-APPEAR"
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "source"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Copies = []decl.Copy{{Source: "source", Target: "/home/agent/token", Mode: "0600", Secret: true}}
	result, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RenderText(&out, result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("render exposed source content")
	}
	if strings.Contains(out.String(), result.Plan.Copies[0].SourceDigest) {
		t.Fatal("render exposed secret digest")
	}
	encoded, err := JSON(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), result.Plan.Copies[0].SourceDigest) {
		t.Fatal("JSON exposed secret digest")
	}
}

func TestCanonicalHasTrailingNewline(t *testing.T) {
	b, err := Canonical(Plan{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("canonical bytes lack newline: %q", b)
	}
}

func TestCopiedContentChangesPlanIdentity(t *testing.T) {
	d, path, data := fixture(t, "")
	source := filepath.Join(filepath.Dir(path), "source")
	if err := os.WriteFile(source, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Copies = []decl.Copy{{Source: "source", Target: "/config", Mode: "0600"}}
	first, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanDigest == second.PlanDigest {
		t.Fatal("copy drift did not change plan digest")
	}
}

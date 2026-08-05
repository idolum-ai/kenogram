package plan

import (
	"bytes"
	"encoding/json"
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
	expectedSource, err := decl.ResolveSource(filepath.Dir(path), "repo")
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.Mounts[0].Source != expectedSource {
		t.Fatalf("source not resolved: %s", first.Plan.Mounts[0].Source)
	}
	if first.Plan.Mounts[0].SourceType != "directory" {
		t.Fatalf("source type=%q", first.Plan.Mounts[0].SourceType)
	}
}

func TestDigestRegularCopyBytesMatchesCanonicalSourceDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy")
	raw := []byte("exact descriptor bytes")
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	want, err := DigestSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := DigestRegularCopyBytes(raw, 0o640); got != want {
		t.Fatalf("got=%s want=%s", got, want)
	}
}

func TestBuildRejectsAmbiguousMountGrammar(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*decl.Declaration, string)
	}{
		{name: "source", edit: func(value *decl.Declaration, directory string) {
			if err := os.Rename(filepath.Join(directory, "repo"), filepath.Join(directory, "repo,alias")); err != nil {
				t.Fatal(err)
			}
			value.Mounts[0].Source = "repo,alias"
		}},
		{name: "target", edit: func(value *decl.Declaration, _ string) { value.Mounts[0].Target = "/workspace/repo,alias" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			declaration, path, raw := fixture(t, "")
			test.edit(&declaration, filepath.Dir(path))
			if _, err := Build(declaration, path, raw); err == nil || !strings.Contains(err.Error(), "--mount") {
				t.Fatalf("error=%v", err)
			}
		})
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

func TestInterfaceIsRenderedAndChangesSemanticIdentity(t *testing.T) {
	d, path, data := fixture(t, "")
	without, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	d.Interfaces = []decl.Interface{{Name: "ssh", Address: "127.0.0.1:2222"}}
	with, err := Build(d, path, data)
	if err != nil {
		t.Fatal(err)
	}
	if with.PlanDigest == without.PlanDigest {
		t.Fatal("declared interface did not change plan identity")
	}
	var output bytes.Buffer
	if err := RenderText(&output, with); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "interface: ssh address=127.0.0.1:2222") {
		t.Fatalf("rendered plan lacks interface: %s", output.String())
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
	var retained Result
	if err := json.Unmarshal(encoded, &retained); err != nil {
		t.Fatal(err)
	}
	_, evidenceDigest, err := EvidenceCanonicalWithAnchor(retained.Plan, retained.SourceAnchor)
	if err != nil {
		t.Fatal(err)
	}
	if retained.EvidenceDigest != evidenceDigest || retained.EvidenceDigest == result.PlanDigest {
		t.Fatalf("retained evidence digest=%q operational digest=%q recomputed=%q", retained.EvidenceDigest, result.PlanDigest, evidenceDigest)
	}
	if retained.PlanDigest != retained.EvidenceDigest {
		t.Fatalf("retained plan digest=%q evidence digest=%q", retained.PlanDigest, retained.EvidenceDigest)
	}
	projected, err := ProjectEvidence(d, data, retained)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mustJSON(t, retained), mustJSON(t, projected)) {
		t.Fatalf("retained plan does not equal its declaration projection\nretained: %s\nprojected: %s", mustJSON(t, retained), mustJSON(t, projected))
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
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

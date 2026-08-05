package releaseprovenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

func TestVerifyBindsEveryReleaseCoordinate(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "kenogram")
	provenancePath := filepath.Join(directory, "provenance.json")
	executableRaw := []byte("exact executable bytes")
	if err := os.WriteFile(executable, executableRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(executableRaw)
	expected := Expected{
		Version: "v1.2.3", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceDate: "2026-08-05T21:00:00Z", GOOS: "linux", GOARCH: "amd64",
	}
	value := jobcontract.Provenance{
		Schema: jobcontract.ProvenanceSchema, BuildKind: "release",
		Version: expected.Version, Commit: expected.Commit, SourceDate: expected.SourceDate,
		GoVersion: "go1.26.5", GOOS: expected.GOOS, GOARCH: expected.GOARCH,
		ExecutableSHA256: "sha256:" + hex.EncodeToString(sum[:]),
	}
	writeProvenance(t, provenancePath, value)
	if _, err := Verify(executable, provenancePath, expected); err != nil {
		t.Fatalf("verify exact release: %v", err)
	}

	tests := map[string]func(*Expected, *jobcontract.Provenance){
		"version": func(want *Expected, _ *jobcontract.Provenance) { want.Version = "v1.2.4" },
		"commit": func(want *Expected, _ *jobcontract.Provenance) {
			want.Commit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"source date": func(want *Expected, _ *jobcontract.Provenance) { want.SourceDate = "2026-08-05T21:00:01Z" },
		"GOOS":        func(want *Expected, _ *jobcontract.Provenance) { want.GOOS = "darwin" },
		"GOARCH":      func(want *Expected, _ *jobcontract.Provenance) { want.GOARCH = "arm64" },
		"executable digest": func(_ *Expected, got *jobcontract.Provenance) {
			got.ExecutableSHA256 = "sha256:" + string(make([]byte, 64))
		},
		"build kind": func(_ *Expected, got *jobcontract.Provenance) { got.BuildKind = "development" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			want, got := expected, value
			mutate(&want, &got)
			writeProvenance(t, provenancePath, got)
			if _, err := Verify(executable, provenancePath, want); err == nil {
				t.Fatal("mismatched release identity was accepted")
			}
		})
	}
}

func TestVerifyRejectsDuplicateOrUnknownProvenanceFields(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "kenogram")
	provenancePath := filepath.Join(directory, "provenance.json")
	if err := os.WriteFile(executable, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	expected := Expected{Version: "v1.2.3", Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceDate: "2026-08-05T21:00:00Z", GOOS: "linux", GOARCH: "amd64"}
	for _, raw := range []string{
		`{"schema":"kenogram.executable-provenance.v1","schema":"kenogram.executable-provenance.v1"}`,
		`{"schema":"kenogram.executable-provenance.v1","unknown":true}`,
	} {
		if err := os.WriteFile(provenancePath, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(executable, provenancePath, expected); err == nil {
			t.Fatal("malformed provenance was accepted")
		}
	}
}

func writeProvenance(t *testing.T, path string, value jobcontract.Provenance) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

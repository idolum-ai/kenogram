package jobcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestContractDocumentsAcceptCanonicalFixtures(t *testing.T) {
	t.Parallel()
	request := validRequest()
	result := validResult()
	manifest := validManifest()
	provenance := validProvenance()
	for _, test := range []struct {
		name  string
		value any
		parse func([]byte) error
	}{
		{name: "request", value: request, parse: func(raw []byte) error { _, err := ParseRequest(raw); return err }},
		{name: "result", value: result, parse: func(raw []byte) error { _, err := ParseResult(raw); return err }},
		{name: "manifest", value: manifest, parse: func(raw []byte) error { _, err := ParseManifest(raw); return err }},
		{name: "provenance", value: provenance, parse: func(raw []byte) error { _, err := ParseProvenance(raw); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.parse(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStrictDecodeRejectsAmbiguousDocuments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "duplicate top-level key", raw: `{"schema":"kenogram.job-request.v1","schema":"kenogram.job-request.v1"}`, want: "duplicate object key"},
		{name: "duplicate nested key", raw: `{"schema":"kenogram.job-request.v1","job_id":"x","declaration":{"path":"/x","path":"/y"}}`, want: "duplicate object key"},
		{name: "unknown field", raw: requestJSONWith(`,"ambient_authority":true`), want: "unknown field"},
		{name: "trailing JSON", raw: requestJSONWith("") + `{}`, want: "trailing"},
		{name: "fractional integer", raw: strings.Replace(requestJSONWith(""), `"timeout_ns":1000000`, `"timeout_ns":1000000.5`, 1), want: "cannot unmarshal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	oversized := make([]byte, MaximumRequestBytes+1)
	for index := range oversized {
		oversized[index] = ' '
	}
	if _, err := ParseRequest(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized error = %v", err)
	}
}

func TestRequestRejectsAuthorityAndBoundViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Request)
		want string
	}{
		{name: "bidi declaration path", edit: func(value *Request) { value.Declaration.Path = "/tmp/\u202erequest" }, want: "declaration binding"},
		{name: "relative working directory", edit: func(value *Request) { value.Command.WorkingDirectory = "workspace" }, want: "working_directory"},
		{name: "duplicate environment name", edit: func(value *Request) {
			value.Command.Environment = append(value.Command.Environment, value.Command.Environment[0])
		}, want: "duplicate"},
		{name: "environment mixes public and secret", edit: func(value *Request) {
			value.Command.Environment[0].SecretFile = "/run/secrets/lang"
		}, want: "environment"},
		{name: "timeout too large", edit: func(value *Request) { value.Limits.TimeoutNS = MaximumWireInteger }, want: "limits"},
		{name: "artifact count too large", edit: func(value *Request) { value.Artifacts.MaxEntries = maximumArtifactEntries + 1 }, want: "artifact"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validRequest()
			test.edit(&value)
			if err := ValidateRequest(value); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRequestRetainsOnlySecretFileReference(t *testing.T) {
	t.Parallel()
	value := validRequest()
	value.Command.Environment = append(value.Command.Environment, EnvironmentItem{Name: "TOKEN", SecretFile: "/run/secrets/token"})
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret bytes") || !strings.Contains(string(raw), `"secret_file":"/run/secrets/token"`) {
		t.Fatalf("secret environment binding = %s", raw)
	}
	if _, err := ParseRequest(raw); err != nil {
		t.Fatal(err)
	}
}

func TestResultCannotStrengthenIncompleteEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Result)
		want string
	}{
		{name: "complete truncated stdout", edit: func(value *Result) { value.Stdout.TotalBytes = 2; value.Stdout.Truncated = true }, want: "complete"},
		{name: "complete cleanup missing container proof", edit: func(value *Result) { value.Cleanup.ContainerAbsent = false }, want: "cleanup"},
		{name: "complete missing runtime identity", edit: func(value *Result) { value.Identity.RuntimeSHA256 = "" }, want: "identity"},
		{name: "incomplete lacks reason", edit: func(value *Result) { value.Status = "incomplete" }, want: "reason"},
		{name: "refused invents target", edit: func(value *Result) { value.Status = "refused"; value.Reasons = []string{"IMAGE_UNAVAILABLE"} }, want: "refused"},
		{name: "stream count contradiction", edit: func(value *Result) { value.Stdout.TotalBytes = 2 }, want: "stream"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validResult()
			test.edit(&value)
			if err := ValidateResult(value); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestManifestRejectsSubstitutionAndInventoryAmbiguity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{name: "request digest substitution", edit: func(value *Manifest) { value.Entries[3].SHA256 = "sha256:" + strings.Repeat("b", 64) }, want: "request digest"},
		{name: "unordered entries", edit: func(value *Manifest) { value.Entries[0], value.Entries[1] = value.Entries[1], value.Entries[0] }, want: "unordered"},
		{name: "path traversal", edit: func(value *Manifest) { value.Entries[0].Path = "../declaration.toml" }, want: "entry"},
		{name: "manifest lists itself", edit: func(value *Manifest) { value.Entries[0].Path = "manifest.json" }, want: "entry"},
		{name: "wrong fixed kind", edit: func(value *Manifest) { value.Entries[0].Kind = "target_artifact" }, want: "want declaration"},
		{name: "missing mandatory entry", edit: func(value *Manifest) { value.Entries = value.Entries[1:] }, want: "omits declaration"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validManifest()
			test.edit(&value)
			if err := ValidateManifest(value); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReleaseProvenanceRejectsPlaceholders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*Provenance)
	}{
		{name: "development version", edit: func(value *Provenance) { value.Version = "dev" }},
		{name: "unknown commit", edit: func(value *Provenance) { value.Commit = "unknown" }},
		{name: "short commit", edit: func(value *Provenance) { value.Commit = strings.Repeat("a", 12) }},
		{name: "unknown date", edit: func(value *Provenance) { value.SourceDate = "unknown" }},
		{name: "offset date", edit: func(value *Provenance) { value.SourceDate = "2026-08-04T08:00:00-04:00" }},
		{name: "numeric prerelease leading zero", edit: func(value *Provenance) { value.Version = "v0.2.0-01" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validProvenance()
			test.edit(&value)
			if err := ValidateProvenance(value); err == nil {
				t.Fatalf("release provenance accepted %#v", value)
			}
		})
	}
	development := validProvenance()
	development.BuildKind = "development"
	development.Version = "dev"
	development.Commit = "unknown"
	development.SourceDate = "unknown"
	if err := ValidateProvenance(development); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedSchemasAreClosedJSONDocuments(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	tests := map[string]string{
		"kenogram.job-request.v1.schema.json":                RequestSchema,
		"kenogram.job-result.v1.schema.json":                 ResultSchema,
		"kenogram.job-evidence-manifest.v1.schema.json":      ManifestSchema,
		"kenogram.executable-provenance.v1.schema.json":      ProvenanceSchema,
		"kenogram.podman-runtime-observation.v1.schema.json": RuntimeObservationSchema,
	}
	for name, identifier := range tests {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "schemas", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := rejectDuplicateKeys(raw); err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Fatalf("root schema is not closed: %#v", schema)
			}
			if !strings.Contains(schema["$id"].(string), identifier) {
				t.Fatalf("schema id %q does not bind %q", schema["$id"], identifier)
			}
			assertObjectSchemasClosed(t, schema, "root")
		})
	}
}

func assertObjectSchemasClosed(t *testing.T, value any, location string) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		if value["type"] == "object" && value["additionalProperties"] != false {
			t.Fatalf("object schema at %s is not closed", location)
		}
		for key, child := range value {
			assertObjectSchemasClosed(t, child, location+"/"+key)
		}
	case []any:
		for index, child := range value {
			assertObjectSchemasClosed(t, child, location+"/"+strconv.Itoa(index))
		}
	}
}

func validRequest() Request {
	lang := "C.UTF-8"
	return Request{
		Schema: RequestSchema, JobID: "ergograph-proof-1",
		Declaration: DeclarationBinding{Path: "/tmp/world.toml", SHA256: testDigest},
		Command:     Command{Argv: []string{"/bin/probe", "--json"}, WorkingDirectory: "/workspace", Environment: []EnvironmentItem{{Name: "LANG", PublicValue: &lang}}},
		Limits:      Limits{TimeoutNS: 1_000_000_000, FinalizeNS: 5_000_000_000, StdoutMaxBytes: 1024, StderrMaxBytes: 1024},
		Artifacts:   &ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 16, MaxBytes: 1 << 20},
	}
}

func validResult() Result {
	exit := int64(0)
	duration := int64(1_000_000_000)
	return Result{
		Schema: ResultSchema, JobID: "ergograph-proof-1", Status: "complete", RequestSHA256: testDigest, EvidenceManifest: "manifest.json",
		Identity:     ExecutionIdentity{DeclarationSHA256: testDigest, PlanSHA256: testDigest, Generation: 1, ImageReference: "example.test/probe@" + testDigest, ImageDigest: testDigest, RuntimeSHA256: testDigest, ProvenanceSHA256: testDigest, RuntimeProvider: "podman-cli"},
		Target:       TargetResult{Kind: "exited", ExitStatus: &exit, StartedAt: "2026-08-04T12:00:00Z", FinishedAt: "2026-08-04T12:00:01Z", DurationNS: &duration},
		Stdout:       StreamResult{Path: "stdout.bin", SHA256: testDigest, CapturedBytes: 1, TotalBytes: 1},
		Stderr:       StreamResult{Path: "stderr.bin", SHA256: testDigest},
		Finalization: FinalizationResult{StartedAt: "2026-08-04T12:00:01Z", FinishedAt: "2026-08-04T12:00:02Z", DurationNS: duration},
		Cleanup:      CleanupResult{Status: "complete", ContainerAbsent: true, ProxyAbsent: true, ProcessGroupEmpty: true, DurationNS: duration, Reasons: []string{}},
		Reasons:      []string{},
	}
}

func validManifest() Manifest {
	entries := []ManifestEntry{
		{Path: "declaration.toml", Kind: "declaration", Size: 1, SHA256: testDigest},
		{Path: "plan.json", Kind: "plan", Size: 1, SHA256: testDigest},
		{Path: "provenance.json", Kind: "provenance", Size: 1, SHA256: testDigest},
		{Path: "request.json", Kind: "request", Size: 1, SHA256: testDigest},
		{Path: "result.json", Kind: "result", Size: 1, SHA256: testDigest},
		{Path: "runtime-after.json", Kind: "runtime", Size: 1, SHA256: testDigest},
		{Path: "runtime-before.json", Kind: "runtime", Size: 1, SHA256: testDigest},
		{Path: "stderr.bin", Kind: "stderr", Size: 0, SHA256: testDigest},
		{Path: "stdout.bin", Kind: "stdout", Size: 1, SHA256: testDigest},
	}
	return Manifest{Schema: ManifestSchema, JobID: "ergograph-proof-1", RequestSHA256: testDigest, ResultSHA256: testDigest, ContentSHA256: testDigest, SealedAt: "2026-08-04T12:00:03Z", Entries: entries}
}

func validProvenance() Provenance {
	return Provenance{Schema: ProvenanceSchema, BuildKind: "release", Version: "v0.2.0", Commit: strings.Repeat("a", 40), SourceDate: "2026-08-04T12:00:00Z", GoVersion: "go1.26.5", GOOS: "linux", GOARCH: "arm64", ExecutableSHA256: testDigest}
}

func requestJSONWith(extra string) string {
	return `{"schema":"kenogram.job-request.v1","job_id":"x","declaration":{"path":"/tmp/world.toml","sha256":"` + testDigest + `"},"command":{"argv":["/bin/true"],"working_directory":"/workspace","environment":[]},"limits":{"timeout_ns":1000000,"finalize_timeout_ns":1000000,"stdout_max_bytes":0,"stderr_max_bytes":0}` + extra + `}`
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("repository root not found")
		}
		root = parent
	}
}

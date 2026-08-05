package job

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

type fakeRuntime struct {
	process  *fakeProcess
	startErr error
	cleanup  jobcontract.CleanupResult
	cleaned  bool
}

func (f *fakeRuntime) Start(_ context.Context, _ Invocation, stdout, stderr io.Writer) (Process, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	_, _ = stdout.Write([]byte("answer\n"))
	_, _ = stderr.Write([]byte("note\n"))
	return f.process, nil
}

func (f *fakeRuntime) Cleanup(context.Context, Invocation) jobcontract.CleanupResult {
	f.cleaned = true
	return f.cleanup
}

type fakeProcess struct {
	waitErr   error
	finalErr  error
	target    jobcontract.TargetResult
	block     bool
	artifacts []Artifact
	beforeRaw []byte
}

func (f *fakeProcess) Identity() RuntimeIdentity {
	before := f.beforeRaw
	if before == nil {
		before = []byte("{\"running\":true}\n")
	}
	return RuntimeIdentity{Generation: 1, ImageReference: "example.invalid/job@" + testDigest(), ImageDigest: testDigest(), Before: before}
}

func (f *fakeProcess) Wait(ctx context.Context) (jobcontract.TargetResult, error) {
	if f.block {
		<-ctx.Done()
		return jobcontract.TargetResult{}, ctx.Err()
	}
	return f.target, f.waitErr
}
func (f *fakeProcess) Finalize(context.Context) (RuntimeFinalization, error) {
	return RuntimeFinalization{After: []byte("{\"absent\":true}\n"), Artifacts: f.artifacts}, f.finalErr
}

func TestExecutorSealsAndVerifierRederivesEvidence(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Sealed || outcome.Result.Status != "complete" || !runtime.cleaned {
		t.Fatalf("outcome=%#v cleaned=%t", outcome, runtime.cleaned)
	}
	verification, err := Verify(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if verification.Status != "complete" || verification.JobID != "job-1" {
		t.Fatalf("verification=%#v", verification)
	}
	if err := os.WriteFile(filepath.Join(evidence, "stdout.bin"), []byte("substituted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(evidence); err == nil || !(strings.Contains(err.Error(), "changed") || strings.Contains(err.Error(), "bound")) {
		t.Fatalf("substitution verification error=%v", err)
	}
}

func TestExecutorRefusesExistingEvidenceLeaf(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err == nil || !strings.Contains(err.Error(), "without replacement") {
		t.Fatalf("error=%v", err)
	}
}

func TestPublicationFailureCannotLeaveASeal(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	executor.AfterFileSync = func(name string) error {
		if name == "provenance.json" {
			return errors.New("injected publication failure")
		}
		return nil
	}
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err == nil {
		t.Fatal("publication failure was ignored")
	}
	if _, err := os.Lstat(filepath.Join(evidence, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("failed publication left a seal: %v", err)
	}
}

func TestVerifierRejectsEvidenceOutsideSealedInventory(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "unsealed.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "unsealed") {
		t.Fatalf("error=%v", err)
	}
}

func TestFinalizationFailureAndIncompleteCleanupCannotBecomeComplete(t *testing.T) {
	for _, test := range []struct {
		name   string
		edit   func(*fakeRuntime)
		reason string
	}{
		{name: "partial finalization", edit: func(runtime *fakeRuntime) { runtime.process.finalErr = errors.New("flush failed") }, reason: "FINALIZATION_FAILED"},
		{name: "cleanup unproved", edit: func(runtime *fakeRuntime) {
			runtime.cleanup = jobcontract.CleanupResult{Status: "incomplete", DurationNS: 1, Reasons: []string{"CONTAINER_PRESENT"}}
		}, reason: "CLEANUP_INCOMPLETE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, requestRaw, evidence, runtime := fixture(t)
			test.edit(runtime)
			outcome, err := executor.Run(context.Background(), requestRaw, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Result.Status != "incomplete" || !contains(outcome.Result.Reasons, test.reason) {
				t.Fatalf("result=%#v", outcome.Result)
			}
			if _, err := Verify(evidence); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputTruncationIsRetainedButFailsClosed(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	request.Limits.StdoutMaxBytes = 2
	requestRaw, _ = json.Marshal(request)
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != "incomplete" || !outcome.Result.Stdout.Truncated || outcome.Result.Stdout.CapturedBytes != 2 {
		t.Fatalf("result=%#v", outcome.Result)
	}
}

func TestTargetTimeoutStillRunsCleanupAndSealsUnknownOutcome(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	runtime.process.block = true
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	request.Limits.TimeoutNS = int64(time.Millisecond)
	requestRaw, _ = json.Marshal(request)
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != "incomplete" || outcome.Result.Target.Kind != "unknown" || !runtime.cleaned {
		t.Fatalf("result=%#v cleaned=%t", outcome.Result, runtime.cleaned)
	}
	if _, err := Verify(evidence); err != nil {
		t.Fatal(err)
	}
}

func TestRequestedArtifactsAreBoundedInventoriedAndReverified(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 2, MaxBytes: 16}
	requestRaw, _ = json.Marshal(request)
	runtime.process.artifacts = []Artifact{{Path: "report.json", Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("{\"ok\":true}")), nil
	}}}
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != "complete" {
		t.Fatalf("result=%#v", outcome.Result)
	}
	if _, err := Verify(evidence); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "target-artifacts", "report.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(evidence); err == nil {
		t.Fatal("artifact substitution passed verification")
	}
}

func TestRequestedArtifactInventoryRemainsVerifiableWhenFinalizationFails(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 2, MaxBytes: 16}
	requestRaw, _ = json.Marshal(request)
	runtime.process.finalErr = errors.New("provider lost artifact boundary")
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != "incomplete" {
		t.Fatalf("result=%#v", outcome.Result)
	}
	if _, err := Verify(evidence); err != nil {
		t.Fatal(err)
	}
}

func TestAdmittedProcessWithMalformedRuntimeEvidenceIsNeverCalledNotStarted(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	runtime.process.beforeRaw = []byte(`{"running":true,"running":false}`)
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Result.Status != "incomplete" || outcome.Result.Target.Kind != "unknown" || !contains(outcome.Result.Reasons, "RUNTIME_START_OBSERVATION_FAILED") {
		t.Fatalf("result=%#v", outcome.Result)
	}
	if _, err := Verify(evidence); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (Executor, []byte, string, *fakeRuntime) {
	t.Helper()
	dir := t.TempDir()
	declaration := []byte(`version = 1
name = "job"

[world]
hostname = "job"
base = "example.invalid/job@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "agent"

[resources]
cpus = 1
memory_bytes = 1073741824
pids = 64

[workspace]
paths = ["/workspace"]
`)
	declarationPath := filepath.Join(dir, "kenogram.toml")
	if err := os.WriteFile(declarationPath, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	lang := "C.UTF-8"
	request := jobcontract.Request{
		Schema: jobcontract.RequestSchema, JobID: "job-1",
		Declaration: jobcontract.DeclarationBinding{Path: declarationPath, SHA256: digest(declaration)},
		Command:     jobcontract.Command{Argv: []string{"/bin/true"}, WorkingDirectory: "/workspace", Environment: []jobcontract.EnvironmentItem{{Name: "LANG", PublicValue: &lang}}},
		Limits:      jobcontract.Limits{TimeoutNS: int64(time.Second), FinalizeNS: int64(time.Second), StdoutMaxBytes: 1024, StderrMaxBytes: 1024},
	}
	requestRaw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	exit := int64(0)
	duration := int64(time.Second)
	runtime := &fakeRuntime{
		process: &fakeProcess{target: jobcontract.TargetResult{Kind: "exited", ExitStatus: &exit, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: &duration}},
		cleanup: jobcontract.CleanupResult{Status: "complete", ContainerAbsent: true, ProxyAbsent: true, ProcessGroupEmpty: true, DurationNS: 1, Reasons: []string{}},
	}
	now := time.Date(2026, 8, 5, 12, 0, 2, 0, time.UTC)
	return Executor{Runtime: runtime, Build: BuildIdentity{Version: "dev", Commit: "unknown", SourceDate: "unknown"}, Now: func() time.Time { return now }}, requestRaw, filepath.Join(dir, "evidence"), runtime
}

func testDigest() string { return "sha256:" + strings.Repeat("a", 64) }
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

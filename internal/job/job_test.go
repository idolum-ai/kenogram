package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/app"
	"github.com/idolum-ai/kenogram/internal/jobcontract"
	"github.com/idolum-ai/kenogram/internal/plan"
)

type fakeRuntime struct {
	process      *fakeProcess
	startErr     error
	cleanup      jobcontract.CleanupResult
	cleaned      atomic.Bool
	startBlock   <-chan struct{}
	cleanupBlock <-chan struct{}
}

func (f *fakeRuntime) Start(_ context.Context, _ Invocation, stdout, stderr io.Writer) (Process, error) {
	if f.startBlock != nil {
		<-f.startBlock
	}
	if f.startErr != nil {
		return nil, f.startErr
	}
	_, _ = stdout.Write([]byte("answer\n"))
	_, _ = stderr.Write([]byte("note\n"))
	return f.process, nil
}

func (f *fakeRuntime) Cleanup(context.Context, Invocation) jobcontract.CleanupResult {
	if f.cleanupBlock != nil {
		<-f.cleanupBlock
	}
	f.cleaned.Store(true)
	return f.cleanup
}

type fakeProcess struct {
	waitErr        error
	finalErr       error
	target         jobcontract.TargetResult
	block          bool
	waitBlock      <-chan struct{}
	finalBlock     <-chan struct{}
	identityBlock  <-chan struct{}
	artifacts      []Artifact
	beforeRaw      []byte
	imageReference string
	imageDigest    string
}

func (f *fakeProcess) Identity(context.Context) (RuntimeIdentity, error) {
	if f.identityBlock != nil {
		<-f.identityBlock
	}
	before := f.beforeRaw
	if before == nil {
		before = []byte("{\"running\":true}\n")
	}
	reference := f.imageReference
	if reference == "" {
		reference = "example.invalid/job@" + testDigest()
	}
	digest := f.imageDigest
	if digest == "" {
		digest = testDigest()
	}
	return RuntimeIdentity{Generation: 1, ImageReference: reference, ImageDigest: digest, Before: before}, nil
}

func (f *fakeProcess) Wait(ctx context.Context) (jobcontract.TargetResult, error) {
	if f.waitBlock != nil {
		<-f.waitBlock
	}
	if f.block {
		<-ctx.Done()
		return jobcontract.TargetResult{}, ctx.Err()
	}
	return f.target, f.waitErr
}
func (f *fakeProcess) Finalize(context.Context) (RuntimeFinalization, error) {
	if f.finalBlock != nil {
		<-f.finalBlock
	}
	return RuntimeFinalization{After: []byte("{\"absent\":true}\n"), Artifacts: f.artifacts}, f.finalErr
}

func TestExecutorSealsAndVerifierRederivesEvidence(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Sealed || outcome.Result.Status != "complete" || !runtime.cleaned.Load() {
		t.Fatalf("outcome=%#v cleaned=%t", outcome, runtime.cleaned.Load())
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
	if outcome.Result.Status != "incomplete" || outcome.Result.Target.Kind != "unknown" || !runtime.cleaned.Load() {
		t.Fatalf("result=%#v cleaned=%t", outcome.Result, runtime.cleaned.Load())
	}
	if _, err := Verify(evidence); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorBoundsContextIgnoringRuntimePhases(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*fakeRuntime, chan struct{})
	}{
		{name: "start", edit: func(runtime *fakeRuntime, release chan struct{}) { runtime.startBlock = release }},
		{name: "identity", edit: func(runtime *fakeRuntime, release chan struct{}) { runtime.process.identityBlock = release }},
		{name: "wait", edit: func(runtime *fakeRuntime, release chan struct{}) { runtime.process.waitBlock = release }},
		{name: "finalize", edit: func(runtime *fakeRuntime, release chan struct{}) { runtime.process.finalBlock = release }},
		{name: "cleanup", edit: func(runtime *fakeRuntime, release chan struct{}) { runtime.cleanupBlock = release }},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, requestRaw, evidence, runtime := fixture(t)
			release := make(chan struct{})
			test.edit(runtime, release)
			var request jobcontract.Request
			if err := json.Unmarshal(requestRaw, &request); err != nil {
				t.Fatal(err)
			}
			request.Limits.TimeoutNS = int64(20 * time.Millisecond)
			request.Limits.FinalizeNS = int64(20 * time.Millisecond)
			requestRaw, _ = json.Marshal(request)
			started := time.Now()
			outcome, err := executor.Run(context.Background(), requestRaw, evidence)
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			if time.Since(started) > 500*time.Millisecond {
				t.Fatalf("context-ignoring %s exceeded bound", test.name)
			}
			if outcome.Result.Status != "incomplete" {
				t.Fatalf("result=%#v", outcome.Result)
			}
			if _, err := Verify(evidence); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type blockingArtifactReader struct {
	read  <-chan struct{}
	close <-chan struct{}
}

func (r *blockingArtifactReader) Read([]byte) (int, error) { <-r.read; return 0, io.EOF }
func (r *blockingArtifactReader) Close() error             { <-r.close; return nil }

func TestArtifactOpenReadAndFailureAreBoundedAndAlwaysCleaned(t *testing.T) {
	for _, mode := range []string{"open-blocks", "read-blocks", "open-fails"} {
		t.Run(mode, func(t *testing.T) {
			executor, requestRaw, evidence, runtime := fixture(t)
			var request jobcontract.Request
			if err := json.Unmarshal(requestRaw, &request); err != nil {
				t.Fatal(err)
			}
			request.Limits.FinalizeNS = int64(20 * time.Millisecond)
			request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 1, MaxBytes: 16}
			requestRaw, _ = json.Marshal(request)
			releaseOpen := make(chan struct{})
			releaseRead := make(chan struct{})
			releaseClose := make(chan struct{})
			runtime.process.artifacts = []Artifact{{Path: "report", Open: func() (io.ReadCloser, error) {
				switch mode {
				case "open-blocks":
					<-releaseOpen
					return io.NopCloser(strings.NewReader("ok")), nil
				case "read-blocks":
					return &blockingArtifactReader{read: releaseRead, close: releaseClose}, nil
				default:
					return nil, errors.New("artifact unavailable")
				}
			}}}
			started := time.Now()
			outcome, err := executor.Run(context.Background(), requestRaw, evidence)
			if err != nil {
				t.Fatal(err)
			}
			close(releaseOpen)
			close(releaseRead)
			close(releaseClose)
			if time.Since(started) > 500*time.Millisecond || outcome.Result.Status != "incomplete" || !runtime.cleaned.Load() || !contains(outcome.Result.Reasons, "ARTIFACT_COLLECTION_FAILED") {
				t.Fatalf("outcome=%#v cleaned=%t", outcome, runtime.cleaned.Load())
			}
			if _, err := Verify(evidence); err != nil {
				t.Fatal(err)
			}
		})
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

func TestRuntimeImageMismatchIsIncompleteAndRetainsDeclaredAuthority(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*fakeProcess)
	}{
		{name: "reference", edit: func(process *fakeProcess) { process.imageReference = "example.invalid/other@" + testDigest() }},
		{name: "digest", edit: func(process *fakeProcess) { process.imageDigest = "sha256:" + strings.Repeat("b", 64) }},
		{name: "malformed digest", edit: func(process *fakeProcess) { process.imageDigest = "not-a-digest" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, requestRaw, evidence, runtime := fixture(t)
			test.edit(runtime.process)
			outcome, err := executor.Run(context.Background(), requestRaw, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Result.Status != "incomplete" || outcome.Result.Identity.ImageReference != "example.invalid/job@"+testDigest() || !contains(outcome.Result.Reasons, "RUNTIME_START_OBSERVATION_FAILED") {
				t.Fatalf("result=%#v", outcome.Result)
			}
			if _, err := Verify(evidence); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSecretFileRequiresOneDeclaredSecretCopyAndRetainedPlanVerifies(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(filepath.Dir(request.Declaration.Path), "secret")
	if err := os.WriteFile(secretPath, []byte("do-not-retain"), 0o600); err != nil {
		t.Fatal(err)
	}
	declaration, err := os.ReadFile(request.Declaration.Path)
	if err != nil {
		t.Fatal(err)
	}
	declaration = append(declaration, []byte("\n[[copies]]\nsource = \"secret\"\ntarget = \"/run/token\"\nmode = \"0600\"\nsecret = true\n")...)
	if err := os.WriteFile(request.Declaration.Path, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Declaration.SHA256 = digest(declaration)
	request.Command.Environment = append(request.Command.Environment, jobcontract.EnvironmentItem{Name: "TOKEN", SecretFile: "/run/token"})
	requestRaw, _ = json.Marshal(request)
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
	planRaw, err := os.ReadFile(filepath.Join(evidence, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := app.PrepareBytes(declaration, request.Declaration.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planRaw), prepared.Result.Plan.Copies[0].SourceDigest) || strings.Contains(string(planRaw), "do-not-retain") {
		t.Fatalf("retained plan exposed secret evidence: %s", planRaw)
	}

	badRequest := request
	badRequest.JobID = "job-2"
	badRequest.Command.Environment[len(badRequest.Command.Environment)-1].SecretFile = "/run/unbound"
	badRaw, _ := json.Marshal(badRequest)
	_, err = executor.Run(context.Background(), badRaw, filepath.Join(filepath.Dir(evidence), "bad-evidence"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("unbound secret_file error=%v", err)
	}
}

func TestVerifierRejectsPlanDeclarationCrossBindingMismatch(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(evidence, "plan.json")
	var retained plan.Result
	raw, err := os.ReadFile(planPath)
	if err != nil || json.Unmarshal(raw, &retained) != nil {
		t.Fatal(err)
	}
	retained.DeclarationDigest = strings.Repeat("b", 64)
	changed, err := json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	changed = append(changed, '\n')
	if err := os.WriteFile(planPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, evidence, func(manifest *jobcontract.Manifest) {
		for index := range manifest.Entries {
			if manifest.Entries[index].Path == "plan.json" {
				manifest.Entries[index].Size = int64(len(changed))
				manifest.Entries[index].SHA256 = digest(changed)
			}
		}
	})
	if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "declaration semantics") {
		t.Fatalf("error=%v", err)
	}
}

func TestVerifierRejectsSelfConsistentForgedPlanProjection(t *testing.T) {
	executor, requestRaw, evidence, _ := fixture(t)
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(evidence, "plan.json")
	var retained plan.Result
	planRaw, err := os.ReadFile(planPath)
	if err != nil || json.Unmarshal(planRaw, &retained) != nil {
		t.Fatal(err)
	}
	forgedDigest := "sha256:" + strings.Repeat("b", 64)
	retained.Plan.World.Base = "example.invalid/forged@" + forgedDigest
	_, evidenceDigest, err := plan.EvidenceCanonical(retained.Plan)
	if err != nil {
		t.Fatal(err)
	}
	retained.PlanDigest = evidenceDigest
	retained.EvidenceDigest = evidenceDigest
	changedPlan, err := json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	changedPlan = append(changedPlan, '\n')
	if err := os.WriteFile(planPath, changedPlan, 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(evidence, "result.json")
	resultRaw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := jobcontract.ParseResult(resultRaw)
	if err != nil {
		t.Fatal(err)
	}
	result.Identity.PlanSHA256 = prefixedDigest(evidenceDigest)
	result.Identity.ImageReference = retained.Plan.World.Base
	result.Identity.ImageDigest = forgedDigest
	changedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	changedResult = append(changedResult, '\n')
	if err := os.WriteFile(resultPath, changedResult, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, evidence, func(manifest *jobcontract.Manifest) {
		for index := range manifest.Entries {
			switch manifest.Entries[index].Path {
			case "plan.json":
				manifest.Entries[index].Size = int64(len(changedPlan))
				manifest.Entries[index].SHA256 = digest(changedPlan)
			case "result.json":
				manifest.Entries[index].Size = int64(len(changedResult))
				manifest.Entries[index].SHA256 = digest(changedResult)
				manifest.ResultSHA256 = digest(changedResult)
			}
		}
	})
	if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "declaration semantics") {
		t.Fatalf("error=%v", err)
	}
}

func TestVerifierStrictlyDecodesRetainedPlan(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		want   string
	}{
		{
			name: "unknown authority field",
			mutate: func(raw []byte) []byte {
				return append(bytes.TrimSuffix(raw, []byte("}\n")), []byte(",\"authority_override\":true}\n")...)
			},
			want: "unknown field",
		},
		{
			name: "duplicate authority field",
			mutate: func(raw []byte) []byte {
				return append(bytes.TrimSuffix(raw, []byte("}\n")), []byte(",\"plan_digest\":\""+strings.Repeat("0", 64)+"\"}\n")...)
			},
			want: "duplicate object key",
		},
		{
			name: "trailing document",
			mutate: func(raw []byte) []byte {
				return append(append([]byte{}, raw...), []byte("{}\n")...)
			},
			want: "trailing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, requestRaw, evidence, _ := fixture(t)
			if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(evidence, "plan.json")
			raw, err := os.ReadFile(planPath)
			if err != nil {
				t.Fatal(err)
			}
			changed := test.mutate(raw)
			if err := os.WriteFile(planPath, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			rewriteManifest(t, evidence, func(manifest *jobcontract.Manifest) {
				for index := range manifest.Entries {
					if manifest.Entries[index].Path == "plan.json" {
						manifest.Entries[index].Size = int64(len(changed))
						manifest.Entries[index].SHA256 = digest(changed)
					}
				}
			})
			if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestVerifierRejectsManifestAuthorityBeforePayloadWork(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*jobcontract.Manifest)
		want string
	}{
		{name: "oversized request", edit: func(manifest *jobcontract.Manifest) {
			for index := range manifest.Entries {
				if manifest.Entries[index].Path == "request.json" {
					manifest.Entries[index].Size = int64(jobcontract.MaximumRequestBytes + 1)
				}
			}
		}, want: "semantic bound"},
		{name: "wrong kind", edit: func(manifest *jobcontract.Manifest) {
			for index := range manifest.Entries {
				if manifest.Entries[index].Path == "stdout.bin" {
					manifest.Entries[index].Kind = "opaque"
				}
			}
		}, want: "manifest entry is invalid"},
		{name: "missing fixed entry", edit: func(manifest *jobcontract.Manifest) {
			for index := range manifest.Entries {
				if manifest.Entries[index].Path == "result.json" {
					manifest.Entries = append(manifest.Entries[:index], manifest.Entries[index+1:]...)
					break
				}
			}
		}, want: "omits result.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, requestRaw, evidence, _ := fixture(t)
			if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
				t.Fatal(err)
			}
			rewriteManifest(t, evidence, test.edit)
			if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestVerifierAppliesRequestArtifactBoundsBeforeHashing(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 1, MaxBytes: 4}
	requestRaw, _ = json.Marshal(request)
	runtime.process.artifacts = []Artifact{{Path: "report", Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("data")), nil
	}}}
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, evidence, func(manifest *jobcontract.Manifest) {
		for index := range manifest.Entries {
			if manifest.Entries[index].Kind == "target_artifact" {
				manifest.Entries[index].Size = 5
			}
		}
	})
	if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "requested byte bound") {
		t.Fatalf("error=%v", err)
	}
}

func TestVerifierParsesBoundedRequestBeforeOpeningTargetArtifacts(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	var request jobcontract.Request
	if err := json.Unmarshal(requestRaw, &request); err != nil {
		t.Fatal(err)
	}
	request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 1, MaxBytes: 16}
	requestRaw, _ = json.Marshal(request)
	runtime.process.artifacts = []Artifact{{Path: "report", Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("data")), nil
	}}}
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(evidence, "target-artifacts", "report")
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), artifactPath); err != nil {
		t.Fatal(err)
	}
	invalidRequest := []byte("{}\n")
	if err := os.WriteFile(filepath.Join(evidence, "request.json"), invalidRequest, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, evidence, func(manifest *jobcontract.Manifest) {
		manifest.RequestSHA256 = digest(invalidRequest)
		for index := range manifest.Entries {
			if manifest.Entries[index].Path == "request.json" {
				manifest.Entries[index].Size = int64(len(invalidRequest))
				manifest.Entries[index].SHA256 = digest(invalidRequest)
			}
		}
	})
	if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "job request schema") {
		t.Fatalf("error=%v", err)
	}
}

func TestSealRejectsManifestPathReplacement(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	executor.AfterFileSync = func(name string) error {
		if name != "manifest.json" {
			return nil
		}
		manifest := filepath.Join(evidence, name)
		if err := os.Rename(manifest, manifest+".replaced"); err != nil {
			return err
		}
		return os.WriteFile(manifest, []byte("{}\n"), 0o600)
	}
	if _, err := executor.Run(context.Background(), requestRaw, evidence); err == nil || !IsPostIdentityError(err) || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("error=%v", err)
	}
	if !runtime.cleaned.Load() {
		t.Fatal("runtime was not cleaned before seal failure")
	}
}

func rewriteManifest(t *testing.T, evidence string, edit func(*jobcontract.Manifest)) {
	t.Helper()
	path := filepath.Join(evidence, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := jobcontract.ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	edit(&manifest)
	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	manifest.ContentSHA256 = contentDigest(manifest.Entries)
	changed, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	changed = append(changed, '\n')
	if err := os.WriteFile(path, changed, 0o600); err != nil {
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

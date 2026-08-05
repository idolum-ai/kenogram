package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
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

func (f *fakeRuntime) Start(_ context.Context, invocation Invocation, stdout, stderr io.Writer) (Process, error) {
	if f.startBlock != nil {
		<-f.startBlock
	}
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.process.invocation = invocation
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
	provider       string
	invocation     Invocation
	afterRaw       []byte
	finalizeMu     sync.Mutex
	finalizeDone   chan struct{}
	finalizeClosed bool
	waitMu         sync.Mutex
	waitDone       chan struct{}
	waitClosed     bool
	egress         bool
	egressMutate   func(*jobcontract.EgressEvidence)
}

func (f *fakeProcess) BeginWait() {
	f.waitMu.Lock()
	if f.waitDone == nil {
		f.waitDone = make(chan struct{})
	}
	f.waitMu.Unlock()
}

func (f *fakeProcess) JoinWait(ctx context.Context) error {
	f.waitMu.Lock()
	done := f.waitDone
	f.waitMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeProcess) finishWait() {
	f.waitMu.Lock()
	if !f.waitClosed {
		close(f.waitDone)
		f.waitClosed = true
	}
	f.waitMu.Unlock()
}

func (f *fakeProcess) BeginFinalization() {
	f.finalizeMu.Lock()
	if f.finalizeDone == nil {
		f.finalizeDone = make(chan struct{})
	}
	f.finalizeMu.Unlock()
}

func (f *fakeProcess) JoinFinalization(ctx context.Context) error {
	f.finalizeMu.Lock()
	done := f.finalizeDone
	f.finalizeMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeProcess) finishFinalization() {
	f.finalizeMu.Lock()
	if !f.finalizeClosed {
		close(f.finalizeDone)
		f.finalizeClosed = true
	}
	f.finalizeMu.Unlock()
}

func (f *fakeProcess) Identity(context.Context) (RuntimeIdentity, error) {
	if f.identityBlock != nil {
		<-f.identityBlock
	}
	before := f.beforeRaw
	if before == nil {
		before = fakeRuntimeObservation(f.invocation, "before")
	}
	reference := f.imageReference
	if reference == "" {
		reference = "example.invalid/job@" + testDigest()
	}
	digest := f.imageDigest
	if digest == "" {
		digest = testDigest()
	}
	provider := f.provider
	if provider == "" {
		provider = "podman-cli"
	}
	return RuntimeIdentity{Provider: provider, Generation: 1, ImageReference: reference, ImageDigest: digest, Before: before}, nil
}

func (f *fakeProcess) Wait(ctx context.Context) (jobcontract.TargetResult, error) {
	f.BeginWait()
	defer f.finishWait()
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
	f.BeginFinalization()
	defer f.finishFinalization()
	if f.finalBlock != nil {
		<-f.finalBlock
	}
	after := f.afterRaw
	if after == nil {
		after = fakeRuntimeObservation(f.invocation, "after")
	}
	var egress []byte
	if f.egress {
		value := jobcontract.EgressEvidence{
			Schema: jobcontract.EgressEvidenceSchema, Status: "complete", AllowlistSHA256: EgressAllowlistDigest(f.invocation.Prepared.Result.Plan.NetworkAllow),
			ListenerAddress: "127.0.0.1:3128", OwnerID: strings.Repeat("d", 32), ContainerID: strings.Repeat("c", 64), Generation: 1,
			PID: 42, ProcessStart: "start", UserNamespace: jobcontract.NamespaceIdentity{Device: 1, Inode: 2}, NetworkNamespace: jobcontract.NamespaceIdentity{Device: 3, Inode: 4},
			ReadyAt: "2026-08-05T11:59:59Z", EnvironmentKeys: append([]string{}, EgressEnvironmentKeys...),
			RevokedAt: "2026-08-05T12:00:01Z", ListenerClosed: true, ActiveConnectionsZero: true, Joined: true, Reasons: []string{},
		}
		if f.egressMutate != nil {
			f.egressMutate(&value)
		}
		egress, _ = json.Marshal(value)
	}
	return RuntimeFinalization{After: after, Egress: egress, Artifacts: f.artifacts}, f.finalErr
}

func fakeRuntimeObservation(invocation Invocation, phase string) []byte {
	source := func(role, target, mode, authority, content string) string {
		value, err := jobcontract.RuntimeMountSource(role, target, mode, authority, content)
		if err != nil {
			panic(err)
		}
		return value
	}
	running := phase == "before"
	mounts := []jobcontract.RuntimeMountObservation{
		{Role: "helper", Source: source("helper", "/etc/kenogram/job-exec", "ro", "", invocation.Provenance.ExecutableSHA256), Target: "/etc/kenogram/job-exec", Mode: "ro", Device: 1, Inode: 1, FileType: "file", SHA256: invocation.Provenance.ExecutableSHA256, IdentityVerified: true},
		{Role: "lifecycle", Source: source("lifecycle", "/etc/kenogram/target-lifecycle.json", "rw", "", ""), Target: "/etc/kenogram/target-lifecycle.json", Mode: "rw", Device: 1, Inode: 2, FileType: "file", IdentityVerified: true},
	}
	for index, target := range invocation.Prepared.Result.Plan.Workspace {
		mounts = append(mounts, jobcontract.RuntimeMountObservation{Role: "workspace", PermissionPolicy: jobcontract.RuntimeWorkspacePermissionPolicy, Source: source("workspace", target, "rw", "", ""), Target: target, Mode: "rw", Device: 1, Inode: uint64(index + 3), FileType: "directory", IdentityVerified: true})
	}
	for index, mount := range invocation.Prepared.Result.Plan.Mounts {
		fact := jobcontract.RuntimeMountObservation{Role: "declared", AuthoritySource: mount.Source, Target: mount.Target, Mode: mount.Mode, Device: 2, Inode: uint64(index + 100), FileType: mount.SourceType, IdentityVerified: true}
		if mount.Mode == "ro" {
			fact.SHA256 = testDigest()
			fact.AuthoritySHA256 = testDigest()
			fact.PermissionPolicy = jobcontract.RuntimeReadOnlyPermissionPolicy
		}
		fact.Source = source(fact.Role, fact.Target, fact.Mode, fact.AuthoritySource, fact.SHA256)
		mounts = append(mounts, fact)
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Target < mounts[j].Target })
	value := jobcontract.RuntimeObservation{
		Schema: jobcontract.RuntimeObservationSchema, Phase: phase, ObservedAt: "2026-08-05T12:00:00Z", Provider: "podman-cli",
		ContainerID: strings.Repeat("c", 64), ContainerName: "kenogram-job-test", Running: running,
		ImageReference: invocation.Prepared.Result.Plan.World.Base, ImageDigest: testDigest(), PlanSHA256: prefixedDigest(invocation.Prepared.Result.EvidenceDigest), DeclarationSHA256: prefixedDigest(invocation.Prepared.Result.DeclarationDigest), Generation: 1,
		NetworkMode: "none", IPCMode: "private", PIDMode: "private", UTSMode: "private", UserNSMode: "keep-id", User: invocation.Prepared.Result.Plan.World.User, Hostname: invocation.Prepared.Result.Plan.World.Hostname, WorkingDirectory: invocation.Request.Command.WorkingDirectory,
		BoundingCaps: []string{}, MemoryBytes: invocation.Prepared.Result.Plan.Resources.MemoryBytes, NanoCPUs: invocation.Prepared.Result.Plan.Resources.CPUs * 1_000_000_000, PIDs: invocation.Prepared.Result.Plan.Resources.PIDs, Mounts: mounts,
	}
	if running {
		value.IPCIsolated, value.UIDIdentity, value.GIDIdentity, value.NoNewPrivileges, value.SeccompMode = true, true, true, true, 2
		if len(invocation.Prepared.Result.Plan.NetworkAllow) != 0 {
			value.EgressAdmission = &jobcontract.RuntimeEgressAdmission{
				AllowlistSHA256: EgressAllowlistDigest(invocation.Prepared.Result.Plan.NetworkAllow), ListenerAddress: "127.0.0.1:3128", OwnerID: strings.Repeat("d", 32),
				PID: 42, ProcessStart: "start", UserNamespace: jobcontract.NamespaceIdentity{Device: 1, Inode: 2}, NetworkNamespace: jobcontract.NamespaceIdentity{Device: 3, Inode: 4},
			}
		}
	}
	raw, _ := json.Marshal(value)
	return raw
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

func TestExecutorSealsAndReplaysScopedEgressEvidence(t *testing.T) {
	outcome, evidence := runScopedEgressFixture(t, nil)
	if !outcome.Sealed || outcome.Result.Status != "complete" || outcome.Result.Identity.EgressSHA256 == "" {
		t.Fatalf("outcome=%#v", outcome)
	}
	egressRaw, err := os.ReadFile(filepath.Join(evidence, "egress.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobcontract.ParseEgressEvidence(egressRaw); err != nil {
		t.Fatal(err)
	}
	verification, err := Verify(evidence)
	if err != nil || verification.Status != "complete" {
		t.Fatalf("verification=%#v error=%v", verification, err)
	}
	resealEgressMutation(t, evidence, func(egress *jobcontract.EgressEvidence) {
		egress.AllowlistSHA256 = "sha256:" + strings.Repeat("b", 64)
	})
	if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("cross-bound egress substitution error=%v", err)
	}
}

func TestVerifierRejectsResealedEgressAdmissionIdentitySubstitution(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*jobcontract.EgressEvidence)
	}{
		{name: "listener address", mutate: func(value *jobcontract.EgressEvidence) { value.ListenerAddress = "127.0.0.1:3129" }},
		{name: "owner identity", mutate: func(value *jobcontract.EgressEvidence) { value.OwnerID = strings.Repeat("e", 32) }},
		{name: "pid", mutate: func(value *jobcontract.EgressEvidence) { value.PID++ }},
		{name: "process start", mutate: func(value *jobcontract.EgressEvidence) { value.ProcessStart = "replacement" }},
		{name: "user namespace device", mutate: func(value *jobcontract.EgressEvidence) { value.UserNamespace.Device++ }},
		{name: "user namespace inode", mutate: func(value *jobcontract.EgressEvidence) { value.UserNamespace.Inode++ }},
		{name: "network namespace device", mutate: func(value *jobcontract.EgressEvidence) { value.NetworkNamespace.Device++ }},
		{name: "network namespace inode", mutate: func(value *jobcontract.EgressEvidence) { value.NetworkNamespace.Inode++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, evidence := runScopedEgressFixture(t, nil)
			resealEgressMutation(t, evidence, test.mutate)
			if _, err := Verify(evidence); err == nil || !strings.Contains(err.Error(), "runtime admission") {
				t.Fatalf("resealed %s substitution error=%v", test.name, err)
			}
		})
	}
}

func TestExecutorOmitsInvalidOrUnrequestedEgressFromVerifiableIncompleteSeal(t *testing.T) {
	t.Run("wrong allowlist", func(t *testing.T) {
		outcome, evidence := runScopedEgressFixture(t, func(value *jobcontract.EgressEvidence) {
			value.AllowlistSHA256 = "sha256:" + strings.Repeat("b", 64)
		})
		if !outcome.Sealed || outcome.Result.Status != "incomplete" || !slices.Contains(outcome.Result.Reasons, "EGRESS_EVIDENCE_INVALID") || outcome.Result.Identity.EgressSHA256 != "" {
			t.Fatalf("outcome=%#v", outcome)
		}
		if _, err := os.Stat(filepath.Join(evidence, "egress.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid egress artifact remains: %v", err)
		}
		verification, err := Verify(evidence)
		if err != nil || verification.Status != "incomplete" {
			t.Fatalf("verification=%#v error=%v", verification, err)
		}
	})

	t.Run("networkless runtime output", func(t *testing.T) {
		executor, requestRaw, evidence, runtime := fixture(t)
		runtime.process.egress = true
		outcome, err := executor.Run(context.Background(), requestRaw, evidence)
		if err != nil || !outcome.Sealed || outcome.Result.Status != "incomplete" || !slices.Contains(outcome.Result.Reasons, "UNREQUESTED_EGRESS_EVIDENCE") || outcome.Result.Identity.EgressSHA256 != "" {
			t.Fatalf("outcome=%#v error=%v", outcome, err)
		}
		if _, err := os.Stat(filepath.Join(evidence, "egress.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unrequested egress artifact remains: %v", err)
		}
		verification, err := Verify(evidence)
		if err != nil || verification.Status != "incomplete" {
			t.Fatalf("verification=%#v error=%v", verification, err)
		}
	})
}

func runScopedEgressFixture(t *testing.T, mutate func(*jobcontract.EgressEvidence)) (Outcome, string) {
	t.Helper()
	executor, requestRaw, evidence, runtime := fixture(t)
	request, err := jobcontract.ParseRequest(requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := os.ReadFile(request.Declaration.Path)
	if err != nil {
		t.Fatal(err)
	}
	declaration = append(declaration, []byte("\n[[network.allow]]\nhost = \"example.test\"\nport = 443\n")...)
	if err := os.WriteFile(request.Declaration.Path, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Declaration.SHA256 = digest(declaration)
	requestRaw, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	runtime.process.egress = true
	runtime.process.egressMutate = mutate
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil {
		t.Fatalf("outcome=%#v error=%v", outcome, err)
	}
	return outcome, evidence
}

func resealEgressMutation(t *testing.T, evidence string, mutate func(*jobcontract.EgressEvidence)) {
	t.Helper()
	egressRaw, err := os.ReadFile(filepath.Join(evidence, "egress.json"))
	if err != nil {
		t.Fatal(err)
	}
	egress, err := jobcontract.ParseEgressEvidence(egressRaw)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&egress)
	egressRaw, _ = json.Marshal(egress)
	if err := os.WriteFile(filepath.Join(evidence, "egress.json"), egressRaw, 0o600); err != nil {
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
	result.Identity.EgressSHA256 = digest(egressRaw)
	resultRaw, _ = json.Marshal(result)
	if err := os.WriteFile(resultPath, resultRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	rewriteManifest(t, evidence, func(manifest *jobcontract.Manifest) {
		manifest.ResultSHA256 = digest(resultRaw)
		for index := range manifest.Entries {
			switch manifest.Entries[index].Path {
			case "egress.json":
				manifest.Entries[index].Size = int64(len(egressRaw))
				manifest.Entries[index].SHA256 = digest(egressRaw)
			case "result.json":
				manifest.Entries[index].Size = int64(len(resultRaw))
				manifest.Entries[index].SHA256 = digest(resultRaw)
			}
		}
	})
}

func TestRequestFileReadIsDescriptorBoundedRegularAndCancelable(t *testing.T) {
	t.Run("exact bound", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(path, bytes.Repeat([]byte{' '}, jobcontract.MaximumRequestBytes), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, err := ReadRequestFile(context.Background(), path)
		if err != nil || len(raw) != jobcontract.MaximumRequestBytes {
			t.Fatalf("bytes=%d error=%v", len(raw), err)
		}
	})
	t.Run("over bound", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "request.json")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(jobcontract.MaximumRequestBytes + 1); err != nil {
			t.Fatal(err)
		}
		file.Close()
		if _, err := ReadRequestFile(context.Background(), path); err == nil || !strings.Contains(err.Error(), "bounded regular") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("special input", func(t *testing.T) {
		root, err := os.MkdirTemp("/tmp", "kenogram-request-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(root)
		path := filepath.Join(root, "request.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if _, err := ReadRequestFile(context.Background(), path); err == nil || !strings.Contains(err.Error(), "bounded regular") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ReadRequestFile(ctx, filepath.Join(t.TempDir(), "absent")); !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestExecutorCancellationPrecedesPreparationAndRuntimeAdmission(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Run(ctx, requestRaw, evidence); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if runtime.process.invocation.Request.JobID != "" {
		t.Fatalf("runtime received canceled invocation: %#v", runtime.process.invocation)
	}
	if _, err := os.Lstat(evidence); !os.IsNotExist(err) {
		t.Fatalf("canceled preparation created evidence: %v", err)
	}
}

func TestVerifierRejectsGenericJSONSubstitutedForPodmanRuntimeContract(t *testing.T) {
	executor, requestRaw, evidence, runtime := fixture(t)
	runtime.process.provider = "podman-cli"
	runtime.process.beforeRaw = []byte("{\"running\":true}\n")
	runtime.process.afterRaw = []byte("{\"absent\":true}\n")
	outcome, err := executor.Run(context.Background(), requestRaw, evidence)
	if err != nil || outcome.Result.Status != "incomplete" || !contains(outcome.Result.Reasons, "RUNTIME_OBSERVATION_INVALID") {
		t.Fatalf("outcome=%#v error=%v", outcome, err)
	}
	verification, err := Verify(evidence)
	if err != nil || verification.Status != "incomplete" {
		t.Fatalf("generic provider evidence was not downgraded: verification=%#v error=%v", verification, err)
	}
}

func TestRuntimeVerifierCrossBindsDeclaredMountSourcesAndRuntimeRoles(t *testing.T) {
	_, requestRaw, _, _ := fixture(t)
	request, err := jobcontract.ParseRequest(requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := os.ReadFile(request.Declaration.Path)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := app.PrepareBytes(declaration, request.Declaration.Path)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Result.Plan.Mounts = append(prepared.Result.Plan.Mounts, plan.Mount{Source: "/retained/input", SourceType: "directory", Target: "/input", Mode: "ro"})
	prepared.Result.Plan.Mounts = append(prepared.Result.Plan.Mounts, plan.Mount{Source: "/retained/output", SourceType: "directory", Target: "/output", Mode: "rw"})
	provenance, _, err := Provenance("", BuildIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	invocation := Invocation{Request: request, Prepared: prepared, Provenance: provenance}
	parse := func(phase string) jobcontract.RuntimeObservation {
		value, parseErr := jobcontract.ParseRuntimeObservation(fakeRuntimeObservation(invocation, phase))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		return value
	}
	result := jobcontract.Result{Identity: jobcontract.ExecutionIdentity{RuntimeProvider: "podman-cli", Generation: 1, ImageReference: prepared.Result.Plan.World.Base, ImageDigest: testDigest(), PlanSHA256: prefixedDigest(prepared.Result.EvidenceDigest), DeclarationSHA256: prefixedDigest(prepared.Result.DeclarationDigest)}}
	for _, test := range []struct {
		name   string
		mutate func(*jobcontract.RuntimeObservation)
		want   string
	}{
		{name: "declared source substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "declared" {
					value.Mounts[index].AuthoritySource = "/substituted/input"
				}
			}
		}, want: "undeclared"},
		{name: "identical two-phase semantic source substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "declared" {
					value.Mounts[index].Source = "kenogram-snapshot:sha256:" + strings.Repeat("b", 64)
				}
			}
		}, want: "semantic source"},
		{name: "identical two-phase writable source substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "declared" && value.Mounts[index].Mode == "rw" {
					value.Mounts[index].Source = "/retained/substitute"
				}
			}
		}, want: "semantic source"},
		{name: "workspace role substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "workspace" {
					value.Mounts[index].Role = "declared"
				}
			}
		}, want: "undeclared"},
		{name: "helper type substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "helper" {
					value.Mounts[index].FileType = "directory"
				}
			}
		}, want: "helper mount is not a file"},
		{name: "lifecycle type substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "lifecycle" {
					value.Mounts[index].FileType = "directory"
				}
			}
		}, want: "lifecycle mount is not a file"},
		{name: "declared type substitution", mutate: func(value *jobcontract.RuntimeObservation) {
			for index := range value.Mounts {
				if value.Mounts[index].Role == "declared" {
					value.Mounts[index].FileType = "file"
				}
			}
		}, want: "type disagrees"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, after := parse("before"), parse("after")
			test.mutate(&before)
			test.mutate(&after)
			if err := verifyRuntimeObservations(before, after, result, request, prepared.Result, provenance); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	t.Run("read-only authority digest phase drift", func(t *testing.T) {
		before, after := parse("before"), parse("after")
		for index := range before.Mounts {
			if before.Mounts[index].Role == "declared" && before.Mounts[index].Mode == "ro" {
				before.Mounts[index].AuthoritySHA256 = "sha256:" + strings.Repeat("b", 64)
			}
		}
		if err := verifyRuntimeObservations(before, after, result, request, prepared.Result, provenance); err == nil || !strings.Contains(err.Error(), "changed across phases") {
			t.Fatalf("error=%v", err)
		}
	})
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
			type execution struct {
				outcome Outcome
				err     error
			}
			done := make(chan execution, 1)
			go func() {
				outcome, err := executor.Run(context.Background(), requestRaw, evidence)
				done <- execution{outcome: outcome, err: err}
			}()
			var observed execution
			select {
			case observed = <-done:
			case <-time.After(10 * time.Second):
				close(release)
				t.Fatalf("context-ignoring %s did not honor its configured deadline", test.name)
			}
			close(release)
			if observed.err != nil {
				t.Fatal(observed.err)
			}
			outcome := observed.outcome
			if outcome.Result.Status != "incomplete" {
				t.Fatalf("result=%#v", outcome.Result)
			}
			if (test.name == "wait" || test.name == "finalize") && runtime.cleaned.Load() {
				t.Fatalf("cleanup raced a context-ignoring %s worker", test.name)
			}
			if _, err := Verify(evidence); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type blockingArtifactReader struct {
	read         <-chan struct{}
	close        <-chan struct{}
	readEntered  chan struct{}
	closeEntered chan struct{}
	readOnce     sync.Once
	closeOnce    sync.Once
}

func (r *blockingArtifactReader) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.readEntered) })
	<-r.read
	return 0, io.EOF
}
func (r *blockingArtifactReader) Close() error {
	r.closeOnce.Do(func() { close(r.closeEntered) })
	<-r.close
	return nil
}

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
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() {
					close(releaseOpen)
					close(releaseRead)
					close(releaseClose)
				})
			}
			defer release()
			openEntered := make(chan struct{})
			readEntered := make(chan struct{})
			closeEntered := make(chan struct{})
			runtime.process.artifacts = []Artifact{{Path: "report", Open: func() (io.ReadCloser, error) {
				switch mode {
				case "open-blocks":
					close(openEntered)
					<-releaseOpen
					return io.NopCloser(strings.NewReader("ok")), nil
				case "read-blocks":
					return &blockingArtifactReader{read: releaseRead, close: releaseClose, readEntered: readEntered, closeEntered: closeEntered}, nil
				default:
					return nil, errors.New("artifact unavailable")
				}
			}}}
			type execution struct {
				outcome Outcome
				err     error
			}
			done := make(chan execution, 1)
			go func() {
				outcome, err := executor.Run(context.Background(), requestRaw, evidence)
				done <- execution{outcome: outcome, err: err}
			}()
			if mode == "open-blocks" {
				select {
				case <-openEntered:
				case <-time.After(10 * time.Second):
					t.Fatal("artifact open was never entered")
				}
			}
			if mode == "read-blocks" {
				select {
				case <-readEntered:
				case <-time.After(10 * time.Second):
					t.Fatal("artifact read was never entered")
				}
			}
			var observed execution
			select {
			case observed = <-done:
			case <-time.After(10 * time.Second):
				release()
				t.Fatal("blocked artifact operation did not honor its configured deadline")
			}
			release()
			if observed.err != nil {
				t.Fatal(observed.err)
			}
			outcome := observed.outcome
			if outcome.Result.Status != "incomplete" || !runtime.cleaned.Load() || !contains(outcome.Result.Reasons, "ARTIFACT_COLLECTION_FAILED") {
				t.Fatalf("outcome=%#v cleaned=%t", outcome, runtime.cleaned.Load())
			}
			if mode == "read-blocks" {
				select {
				case <-closeEntered:
				case <-time.After(10 * time.Second):
					t.Fatal("timed-out artifact reader was not closed")
				}
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

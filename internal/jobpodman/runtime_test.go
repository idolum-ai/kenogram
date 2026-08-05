package jobpodman

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/app"
	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/job"
	"github.com/idolum-ai/kenogram/internal/jobcontract"
	"github.com/idolum-ai/kenogram/internal/jobenv"
	"github.com/idolum-ai/kenogram/internal/joblifecycle"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/sourcetree"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

type fakeAttachedStarter struct {
	process *fakeAttachedProcess
	args    []string
	stdin   []byte
}

func (f *fakeAttachedStarter) BindLifecycle(path string, key []byte) {
	f.process.lifecyclePath, f.process.lifecycleKey = path, append([]byte{}, key...)
}

func (f *fakeAttachedStarter) Start(_ string, args []string, stdin io.Reader, _, _ io.Writer) (attachedProcess, error) {
	f.args = append([]string{}, args...)
	f.stdin, _ = io.ReadAll(stdin)
	return f.process, nil
}

type fakeAttachedProcess struct {
	once          sync.Once
	done          chan error
	lifecyclePath string
	lifecycleKey  []byte
}

func newFakeAttached() *fakeAttachedProcess { return &fakeAttachedProcess{done: make(chan error, 1)} }
func (f *fakeAttachedProcess) Wait() error  { return <-f.done }
func (f *fakeAttachedProcess) Kill() error {
	f.finish(errors.New("killed attached client"))
	return nil
}
func (f *fakeAttachedProcess) finish(err error) {
	f.once.Do(func() {
		if err == nil && f.lifecyclePath != "" {
			status := int64(0)
			_ = joblifecycle.Write(f.lifecyclePath, joblifecycle.Record{Schema: joblifecycle.Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}, f.lifecycleKey)
		}
		f.done <- err
	})
}

type fakePodmanRunner struct {
	mu              sync.Mutex
	exists          bool
	running         bool
	name            string
	labels          map[string]string
	mounts          []map[string]any
	calls           [][]string
	onStop          func()
	failRemove      bool
	failCreateAfter bool
	ownerOverride   string
	artifactSymlink bool
	imageDigest     string
	imageReference  string
	omitImageDigest bool
	containerID     string
	cancelOnCreate  func()
	afterStart      func()
}

func (f *fakePodmanRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	switch args[0] {
	case "info":
		return []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}}}`), nil
	case "ps":
		if f.exists {
			if len(args) > 2 && args[2] == "--no-trunc" {
				return []byte(f.containerIDValue() + "\n"), nil
			}
			return []byte(f.name + "\n"), nil
		}
		return []byte{}, nil
	case "create":
		f.exists = true
		f.name = args[2]
		f.labels = map[string]string{}
		for index := 0; index+1 < len(args); index++ {
			switch args[index] {
			case "--label":
				parts := strings.SplitN(args[index+1], "=", 2)
				f.labels[parts[0]] = parts[1]
			case "--mount":
				fields := strings.Split(args[index+1], ",")
				mount := map[string]any{"RW": strings.Contains(args[index+1], ",rw"), "Mode": fields[3], "Options": fields[3:]}
				for _, field := range fields {
					if strings.HasPrefix(field, "src=") {
						mount["Source"] = strings.TrimPrefix(field, "src=")
					}
					if strings.HasPrefix(field, "dst=") {
						mount["Destination"] = strings.TrimPrefix(field, "dst=")
					}
				}
				f.mounts = append(f.mounts, mount)
			}
		}
		if f.cancelOnCreate != nil {
			f.cancelOnCreate()
			return nil, ctx.Err()
		}
		if f.failCreateAfter {
			if f.ownerOverride != "" {
				f.labels["io.kenogram.job-owner"] = f.ownerOverride
			}
			return nil, errors.New("create response lost after ownership committed")
		}
		return []byte(f.containerIDValue() + "\n"), nil
	case "start":
		f.running = true
		if f.afterStart != nil {
			f.afterStart()
		}
		return nil, nil
	case "inspect":
		if !f.exists {
			return nil, errors.New("container absent")
		}
		return f.inspect(), nil
	case "exec":
		return []byte{}, nil
	case "unshare":
		if len(args) != 10 || args[2] != "_job-collect" || args[3] != f.containerIDValue() || args[4] != f.name || args[5] != f.labels["io.kenogram.job-owner"] {
			return nil, errors.New("artifact collector authority mismatch")
		}
		if err := os.Mkdir(args[7], 0o700); err != nil {
			return nil, err
		}
		if f.artifactSymlink {
			return nil, os.Symlink("/etc/passwd", filepath.Join(args[7], "report.json"))
		}
		return nil, os.WriteFile(filepath.Join(args[7], "report.json"), []byte(`{"ok":true}`), 0o600)
	case "stop", "kill":
		f.running = false
		if f.onStop != nil {
			f.onStop()
		}
		return nil, nil
	case "rm":
		if f.failRemove {
			return nil, errors.New("remove failed")
		}
		f.running, f.exists = false, false
		return nil, nil
	case "cp":
		return nil, nil
	default:
		return nil, errors.New("unexpected podman call")
	}
}

func (f *fakePodmanRunner) inspect() []byte {
	uid, gid := os.Getuid(), os.Getgid()
	imageDigest := f.imageDigest
	if imageDigest == "" {
		imageDigest = "sha256:" + strings.Repeat("a", 64)
	}
	imageReference := imageDigest
	if f.imageReference != "" {
		imageReference = f.imageReference
	}
	if f.omitImageDigest {
		imageDigest = ""
	}
	containerID := f.containerIDValue()
	doc := map[string]any{
		"Id": containerID, "Name": f.name, "Image": imageReference, "ImageDigest": imageDigest, "BoundingCaps": []string{},
		"State":      map[string]any{"Running": f.running, "Pid": map[bool]int{true: 4242, false: 0}[f.running]},
		"IDMappings": map[string]any{"UidMap": []map[string]any{{"ContainerID": uid, "HostID": uid, "Size": 1}}, "GidMap": []map[string]any{{"ContainerID": gid, "HostID": gid, "Size": 1}}},
		"Config":     map[string]any{"Labels": f.labels, "User": "agent", "Hostname": "job", "WorkingDir": "/workspace"},
		"HostConfig": map[string]any{"NetworkMode": "none", "IpcMode": "private", "PidMode": "private", "UTSMode": "private", "UsernsMode": "keep-id", "Memory": 1073741824, "NanoCpus": 1000000000, "PidsLimit": 64, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges"}, "Devices": []any{}},
		"Mounts":     f.mounts,
	}
	raw, _ := json.Marshal([]any{doc})
	return raw
}

func (f *fakePodmanRunner) containerIDValue() string {
	if f.containerID != "" {
		return f.containerID
	}
	return strings.Repeat("c", 64)
}

func (*fakePodmanRunner) Start(context.Context, string, ...string) error       { return nil }
func (*fakePodmanRunner) Interactive(context.Context, string, ...string) error { return nil }

func TestDirectRuntimeOwnsAttachedLifecycleAndCleanupProof(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	var stdout, stderr strings.Builder
	process, err := runtime.Start(context.Background(), invocation, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	target, err := process.Wait(context.Background())
	if err != nil || target.Kind != "exited" || target.ExitStatus == nil || *target.ExitStatus != 0 {
		t.Fatalf("target=%#v err=%v", target, err)
	}
	final, err := process.Finalize(context.Background())
	if err != nil || len(final.After) == 0 {
		t.Fatalf("final=%#v err=%v", final, err)
	}
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "complete" || !cleanup.ContainerAbsent || !cleanup.ProcessGroupEmpty || runner.exists {
		t.Fatalf("cleanup=%#v exists=%t", cleanup, runner.exists)
	}
	joined := strings.Join(runtime.starter.(*fakeAttachedStarter).args, " ")
	if !strings.Contains(joined, "exec --interactive --workdir /workspace ") || !strings.HasSuffix(joined, jobHelperPath+" _job-exec /bin/true --probe") {
		t.Fatalf("attached argv=%q", joined)
	}
	if strings.Contains(joined, " --env ") || strings.Contains(joined, "C.UTF-8") {
		t.Fatalf("ambient podman environment injection remained in argv=%q", joined)
	}
	items, key, err := jobenv.DecodeLaunch(bytes.NewReader(runtime.starter.(*fakeAttachedStarter).stdin))
	if err != nil || len(items) != 1 || items[0].Name != "LANG" || string(items[0].Value) != "C.UTF-8" || len(key) != jobenv.LifecycleKeyBytes || bytes.Equal(key, make([]byte, len(key))) {
		t.Fatalf("handoff=%#v key_bytes=%d err=%v", items, len(key), err)
	}
}

func TestDirectRuntimeCancellationEscalatesAndReportsUnknown(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	runner.onStop = func() { attached.finish(errors.New("container stopped")) }
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target, err := process.Wait(ctx)
	if err == nil || target.Kind != "unknown" {
		t.Fatalf("target=%#v err=%v", target, err)
	}
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "complete" || !cleanup.ContainerAbsent {
		t.Fatalf("cleanup=%#v", cleanup)
	}
	if !hasCall(runner.calls, "stop", "--time", "1") {
		t.Fatalf("calls=%v", runner.calls)
	}
}

func TestAttachedClientCancellationTargetsTheOwnedProcessGroup(t *testing.T) {
	var pid int
	var signal syscall.Signal
	process := &execProcess{
		command: &exec.Cmd{Process: &os.Process{Pid: 4242}},
		killGroup: func(observedPID int, observedSignal syscall.Signal) error {
			pid, signal = observedPID, observedSignal
			return nil
		},
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	if pid != -4242 || signal != syscall.SIGKILL {
		t.Fatalf("pid=%d signal=%v", pid, signal)
	}
}

func TestProviderAmbiguousExitStatusCannotBecomeACompleteTargetExit(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", "exit 125")
	err := command.Run()
	process := &process{runtime: &Runtime{now: time.Now}}
	target, observedErr := process.recordTerminal(err)
	if observedErr == nil || target.Kind != "unknown" {
		t.Fatalf("target=%#v error=%v", target, observedErr)
	}
}

func TestDirectRuntimeRefusesUnsupportedAuthorityBeforeCreation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*job.Invocation)
	}{
		{name: "network", edit: func(value *job.Invocation) {
			value.Prepared.Result.Plan.NetworkAllow = append(value.Prepared.Result.Plan.NetworkAllow, plan.NetworkAllow{Host: "example.test", Port: 443})
		}},
		{name: "relative command", edit: func(value *job.Invocation) {
			value.Request.Command.Argv[0] = "probe"
		}},
		{name: "helper workspace overlap", edit: func(value *job.Invocation) {
			value.Prepared.Result.Plan.Workspace = []string{"/etc"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, runner, _, invocation := runtimeFixture(t)
			test.edit(&invocation)
			if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil {
				t.Fatal("unsupported authority was accepted")
			}
			if hasCall(runner.calls, "create") {
				t.Fatalf("container created: %v", runner.calls)
			}
		})
	}
}

func TestDirectRuntimeMakesNoLocalDarwinIsolationClaim(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	runtime.goos = "darwin"
	if process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || process != nil || !strings.Contains(err.Error(), "require Linux") {
		t.Fatalf("process=%#v error=%v", process, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Darwin refusal contacted provider: %v", runner.calls)
	}
}

func TestDirectRuntimeCannotBeReusedAcrossAuthorities(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || second != nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("process=%#v error=%v", second, err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestDirectRuntimeHandsSecretToTargetOnlyThroughStdinProtocol(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	secret := []byte("opaque\nsecret\xff")
	source := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(source, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := plan.DigestSource(source)
	if err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.Plan.Copies = append(invocation.Prepared.Result.Plan.Copies, plan.Copy{Source: source, SourceDigest: digest, Target: "/run/token", Mode: "0600", Secret: true})
	_, publicDigest, err := plan.EvidenceCanonical(invocation.Prepared.Result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.PlanDigest = strings.Repeat("f", 64)
	invocation.Prepared.Result.EvidenceDigest = publicDigest
	invocation.Request.Command.Environment = []jobcontract.EnvironmentItem{{Name: "TOKEN", SecretFile: "/run/token"}}
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	joined := fmt.Sprint(runner.calls, runtime.starter.(*fakeAttachedStarter).args)
	if strings.Contains(joined, string(secret)) || strings.Contains(joined, invocation.Prepared.Result.PlanDigest) {
		t.Fatalf("secret appeared in provider argv: %q", joined)
	}
	items, err := jobenv.Decode(bytes.NewReader(runtime.starter.(*fakeAttachedStarter).stdin))
	if err != nil || len(items) != 1 || items[0].Name != "TOKEN" || !bytes.Equal(items[0].Value, secret) {
		t.Fatalf("handoff=%#v err=%v", items, err)
	}
	staging := worldfs.For(runtime.scratch, "ephemeral").Staging
	if _, err := os.Lstat(filepath.Join(staging, "g1", "copy-0")); !os.IsNotExist(err) {
		t.Fatalf("secret staging survived provider copy: %v", err)
	}
	runtime.process.attached.(*fakeAttachedProcess).finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestSecretReadBindsAndDeliversExactOpenedDescriptorBytes(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "token")
	original := []byte("opened-secret")
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	copy := plan.Copy{Source: source, SourceDigest: plan.DigestRegularCopyBytes(original, 0o600), Mode: "0600", Secret: true}
	got, err := readSecretSourceWithHook(copy, func() {
		if renameErr := os.Rename(source, source+".old"); renameErr != nil {
			t.Fatal(renameErr)
		}
		if writeErr := os.WriteFile(source, []byte("replacement"), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("got=%q error=%v", got, err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretSourceWithHook(copy, func() { _ = os.WriteFile(source, []byte("mutated-open!"), 0o600) }); err == nil || (!strings.Contains(err.Error(), "planned copy identity") && !strings.Contains(err.Error(), "changed during read")) {
		t.Fatalf("mutation error=%v", err)
	}
}

func TestRuntimeMountAdmissionBoundIsSharedAndExact(t *testing.T) {
	if err := validateRuntimeMountCount(1, jobcontract.MaxRuntimeMounts-3); err != nil {
		t.Fatalf("512 mounts rejected: %v", err)
	}
	if err := validateRuntimeMountCount(1, jobcontract.MaxRuntimeMounts-2); err == nil || !strings.Contains(err.Error(), "512") {
		t.Fatalf("513 mounts accepted: %v", err)
	}
	runtime, runner, _, invocation := runtimeFixture(t)
	invocation.Prepared.Result.Plan.Workspace = make([]string, jobcontract.MaxRuntimeMounts-1)
	for index := range invocation.Prepared.Result.Plan.Workspace {
		invocation.Prepared.Result.Plan.Workspace[index] = fmt.Sprintf("/workspace/%03d", index)
	}
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "512") {
		t.Fatalf("513-mount admission error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("over-limit admission contacted provider: %v", runner.calls)
	}
}

func TestMixedReadOnlyWritableSourceAliasesAreRejected(t *testing.T) {
	t.Run("same inode", func(t *testing.T) {
		dir := t.TempDir()
		left, right := filepath.Join(dir, "left"), filepath.Join(dir, "right")
		if err := os.WriteFile(left, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(left, right); err != nil {
			t.Fatal(err)
		}
		mounts := []plan.Mount{{Source: left, SourceType: "file", Target: "/ro", Mode: "ro"}, {Source: right, SourceType: "file", Target: "/rw", Mode: "rw"}}
		runtime, runner, _, invocation := runtimeFixture(t)
		invocation.Prepared.Result.Plan.Mounts = mounts
		if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Fatalf("admission error=%v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("aliased admission contacted provider: %v", runner.calls)
		}
	})
	t.Run("canonical parent child", func(t *testing.T) {
		root := t.TempDir()
		child := filepath.Join(root, "child")
		if err := os.Mkdir(child, 0o700); err != nil {
			t.Fatal(err)
		}
		mounts := []plan.Mount{{Source: root, SourceType: "directory", Target: "/ro", Mode: "ro"}, {Source: child, SourceType: "directory", Target: "/rw", Mode: "rw"}}
		if err := validateReadOnlyWritableAliases(mounts); err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestBoundedSourceFailuresNeverCreateProviderStateAndCleanScratch(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) string
		want  string
	}{
		{name: "20001 entries", want: "20000 entries", build: func(t *testing.T) string {
			root := t.TempDir()
			for index := int64(1); index < sourcetree.MaxEntries+1; index++ {
				file, err := os.OpenFile(filepath.Join(root, fmt.Sprintf("entry-%05d", index)), os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			return root
		}},
		{name: "over byte bound", want: "bytes", build: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "sparse")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(sourcetree.MaxBytes + 1); err != nil {
				t.Fatal(err)
			}
			file.Close()
			return path
		}},
		{name: "special node", want: "special node", build: func(t *testing.T) string {
			root, err := os.MkdirTemp("/tmp", "kenogram-job-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			listener, err := net.Listen("unix", filepath.Join(root, "innocuous.sock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			return root
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, runner, _, invocation := runtimeFixture(t)
			originalTempDir := runtime.tempDir
			var scratch string
			runtime.tempDir = func(directory, pattern string) (string, error) {
				var err error
				scratch, err = originalTempDir(directory, pattern)
				return scratch, err
			}
			source := test.build(t)
			info, err := os.Lstat(source)
			if err != nil {
				t.Fatal(err)
			}
			sourceType := "file"
			if info.IsDir() {
				sourceType = "directory"
			}
			invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: source, SourceType: sourceType, Target: "/bounded", Mode: "ro"})
			if process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || process != nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("process=%#v error=%v", process, err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("bounded failure contacted provider: %v", runner.calls)
			}
			if scratch == "" {
				t.Fatal("bounded failure did not reach owned scratch")
			}
			if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
				t.Fatalf("scratch remains after refusal: %v", err)
			}
		})
	}
}

func TestSecretValidationSharesSourceBoundBeforeProviderPreflight(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret-tree")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := int64(1); index < sourcetree.MaxEntries+1; index++ {
		file, err := os.OpenFile(filepath.Join(secret, fmt.Sprintf("entry-%05d", index)), os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	declaration := []byte(`version = 1
name = "secret-bound"
[world]
hostname = "secret-bound"
base = "example.invalid/job@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "agent"
[resources]
cpus = 1
memory_bytes = 1073741824
pids = 64
[workspace]
paths = ["/workspace"]
[[copies]]
source = "secret-tree"
target = "/run/secret-tree"
mode = "0600"
secret = true
`)
	declarationPath := filepath.Join(dir, "kenogram.toml")
	if err := os.WriteFile(declarationPath, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	requestRaw, err := json.Marshal(jobcontract.Request{
		Schema: jobcontract.RequestSchema,
		JobID:  "secret-bound",
		Declaration: jobcontract.DeclarationBinding{
			Path:   declarationPath,
			SHA256: digestBytes(declaration),
		},
		Command: jobcontract.Command{Argv: []string{"/bin/true"}, WorkingDirectory: "/workspace", Environment: []jobcontract.EnvironmentItem{}},
		Limits:  jobcontract.Limits{TimeoutNS: int64(time.Second), FinalizeNS: int64(time.Second), StdoutMaxBytes: 1024, StderrMaxBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, runner, _, _ := runtimeFixture(t)
	evidenceDir := filepath.Join(dir, "evidence")
	_, err = (job.Executor{Runtime: runtime}).Run(context.Background(), requestRaw, evidenceDir)
	if err == nil || !strings.Contains(err.Error(), "20000 entries") {
		t.Fatalf("secret bound error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("secret bound contacted provider: %v", runner.calls)
	}
	if _, statErr := os.Stat(evidenceDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pre-identity evidence path stat = %v", statErr)
	}
}

func TestCanceledSourceRestagingNeverCreatesAndCleansPromptly(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	originalTempDir := runtime.tempDir
	var scratch string
	runtime.tempDir = func(directory, pattern string) (string, error) {
		var err error
		scratch, err = originalTempDir(directory, pattern)
		cancel()
		return scratch, err
	}
	source := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(source, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: source, SourceType: "file", Target: "/cancel", Mode: "ro"})
	started := time.Now()
	if process, err := runtime.Start(ctx, invocation, io.Discard, io.Discard); !errors.Is(err, context.Canceled) || process != nil {
		t.Fatalf("process=%#v error=%v", process, err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("canceled staging cleanup was not prompt: %s", time.Since(started))
	}
	if len(runner.calls) != 0 {
		t.Fatalf("canceled staging contacted provider: %v", runner.calls)
	}
	if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch remains after cancellation: %v", err)
	}
}

func TestWritableSourceTreesRejectSocketsAndRuntimeEndpointAliasesBeforeProviderContact(t *testing.T) {
	t.Run("innocuous socket descendant", func(t *testing.T) {
		runtime, runner, _, invocation := runtimeFixture(t)
		root, err := os.MkdirTemp("/tmp", "kenogram-job-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(root)
		listener, err := net.Listen("unix", filepath.Join(root, "callback.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: root, SourceType: "directory", Target: "/writable", Mode: "rw"})
		if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "socket node") {
			t.Fatalf("error=%v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("socket-bearing source contacted provider: %v", runner.calls)
		}
	})
	t.Run("known endpoint inode alias", func(t *testing.T) {
		runtime, runner, _, invocation := runtimeFixture(t)
		endpoint := filepath.Join(t.TempDir(), "podman.sock")
		if err := os.WriteFile(endpoint, []byte("identity"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONTAINER_HOST", "unix://"+endpoint)
		root := t.TempDir()
		if err := os.Link(endpoint, filepath.Join(root, "innocuous")); err != nil {
			t.Fatal(err)
		}
		invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: root, SourceType: "directory", Target: "/writable", Mode: "rw"})
		if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "runtime-endpoint identity") {
			t.Fatalf("error=%v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("endpoint alias contacted provider: %v", runner.calls)
		}
	})
}

func TestMountArgumentMetacharactersFailBeforeProviderContact(t *testing.T) {
	for _, test := range []struct {
		name   string
		source bool
	}{
		{name: "source comma", source: true},
		{name: "target comma"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, runner, _, invocation := runtimeFixture(t)
			source := filepath.Join(t.TempDir(), "input")
			if test.source {
				source = filepath.Join(t.TempDir(), "input,alias")
			}
			if err := os.WriteFile(source, []byte("value"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := "/input"
			if !test.source {
				target = "/input,alias"
			}
			invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: source, SourceType: "file", Target: target, Mode: "ro"})
			if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--mount") {
				t.Fatalf("error=%v", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("ambiguous mount contacted provider: %v", runner.calls)
			}
		})
	}
}

func TestDirectRuntimeRejectsPinnedImageSubstitutionAsAdmittedUnknown(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	runner.imageDigest = "sha256:" + strings.Repeat("b", 64)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err == nil || process == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("process=%#v error=%v", process, err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" || !cleanup.ContainerAbsent {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestDirectRuntimeAcceptsPodman49BareLocalImageIdentity(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	runner.imageReference = strings.Repeat("a", 64)
	runner.omitImageDigest = true
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil || process == nil {
		t.Fatalf("process=%#v error=%v", process, err)
	}
	identity, err := process.Identity(context.Background())
	if err != nil || identity.ImageDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("identity=%#v error=%v", identity, err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" || !cleanup.ContainerAbsent {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestObservedImageBindingSelectsTheDeclaredDigestFromProviderFacts(t *testing.T) {
	local := "sha256:" + strings.Repeat("a", 64)
	repository := "sha256:" + strings.Repeat("b", 64)
	observed, err := validateObservedImage(local, backend.Evidence{ImageReference: local, ImageDigest: repository})
	if err != nil || observed != local {
		t.Fatalf("observed=%q error=%v", observed, err)
	}
}

func TestDirectRuntimeDoesNotDestroyPreexistingName(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	runner.exists = true
	runner.name = jobContainerName(invocation.Request.JobID, strings.Repeat("d", 32))
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error=%v", err)
	}
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if !cleanup.ContainerAbsent || hasCall(runner.calls, "rm") {
		t.Fatalf("cleanup=%#v calls=%v", cleanup, runner.calls)
	}
}

func TestDirectRuntimeAdoptsOnlyMatchingPartiallyCreatedContainer(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	runner.failCreateAfter = true
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil {
		t.Fatal("lost create response was accepted")
	}
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "complete" || runner.exists || !hasCall(runner.calls, "rm", "--force") {
		t.Fatalf("cleanup=%#v calls=%v", cleanup, runner.calls)
	}
}

func TestDirectRuntimeNeverAdoptsMismatchedPartiallyCreatedContainer(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	runner.failCreateAfter = true
	runner.ownerOverride = "somebody-else"
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil {
		t.Fatal("lost create response was accepted")
	}
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "incomplete" || cleanup.ContainerAbsent || !runner.exists || hasCall(runner.calls, "rm") {
		t.Fatalf("cleanup=%#v calls=%v", cleanup, runner.calls)
	}
}

func TestCanceledCreateUsesFreshBoundedReconciliationAndCleanup(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	runner.cancelOnCreate = cancel
	process, err := runtime.Start(ctx, invocation, io.Discard, io.Discard)
	if err == nil || process == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("process=%#v error=%v", process, err)
	}
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "complete" || !cleanup.ContainerAbsent || !hasCall(runner.calls, "rm", "--force", runner.containerIDValue()) {
		t.Fatalf("cleanup=%#v calls=%v", cleanup, runner.calls)
	}
}

func TestDirectRuntimeNeverDeletesUnrelatedReplacementAtFormerName(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	runner.containerID = strings.Repeat("d", 64)
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "complete" || !cleanup.ContainerAbsent || !runner.exists || hasCall(runner.calls, "rm") {
		t.Fatalf("cleanup=%#v calls=%v", cleanup, runner.calls)
	}
}

func TestOwnedContainerRenameCannotEscapeImmutableIDCleanup(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	runner.name = "externally-renamed-owned-container"
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "complete" || !cleanup.ContainerAbsent || runner.exists || !hasCall(runner.calls, "rm", "--force", strings.Repeat("c", 64)) {
		t.Fatalf("cleanup=%#v calls=%v", cleanup, runner.calls)
	}
}

func TestAllPostCreateProviderCallsUseImmutableContainerID(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
	for _, call := range runner.calls {
		switch call[0] {
		case "start", "inspect", "cp", "stop", "kill", "rm", "mount", "unmount":
			for _, argument := range call[1:] {
				if argument == runtime.name {
					t.Fatalf("post-create call used mutable name: %v", call)
				}
			}
		}
	}
}

func TestStagedHelperMustMatchExecutableProvenance(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	invocation.Provenance.ExecutableSHA256 = "sha256:" + strings.Repeat("0", 64)
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("error=%v", err)
	}
	if hasCall(runner.calls, "create") {
		t.Fatalf("container admitted: %v", runner.calls)
	}
}

func TestStageHelperAcceptsKernelResolvedProcStyleExecutable(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-executable")
	if err := os.WriteFile(real, []byte("kernel-resolved-bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "self-exe")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	target, fact, err := stageHelper(context.Background(), func() (string, error) { return link, nil }, scratch)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := os.ReadFile(target)
	if readErr != nil || string(raw) != "kernel-resolved-bytes" || fact.Role != "helper" || fact.SHA256 != digestBytes(raw) {
		t.Fatalf("target=%q fact=%#v raw=%q error=%v", target, fact, raw, readErr)
	}
}

func TestMountInodeReplacementInvalidatesFinalization(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace := mountedSource(runtime, "/workspace")
	old := workspace + ".old"
	if err := os.Rename(workspace, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "source identity changed") {
		t.Fatalf("error=%v", err)
	}
}

func TestWritableMountContentIsNeverTraversedDuringFinalization(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace := mountedSource(runtime, "/workspace")
	for _, fact := range runtime.mountFacts {
		if fact.Target == "/workspace" {
			if fact.SHA256 != "" {
				t.Fatalf("writable fact claims content=%q", fact.SHA256)
			}
		}
	}
	if err := syscall.Mkfifo(filepath.Join(workspace, "target-owned-fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err != nil {
		t.Fatalf("target-writable content was traversed: %v", err)
	}
}

func TestStagedReadOnlyMountContentIsRevalidatedAtFinalization(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	readonly := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(readonly, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: readonly, SourceType: "file", Target: "/readonly", Mode: "ro"})
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	staged := mountedSource(runtime, "/readonly")
	if staged == "" || staged == readonly {
		t.Fatalf("read-only source was not staged: %q", staged)
	}
	if err := os.WriteFile(staged, []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "source identity changed") {
		t.Fatalf("error=%v", err)
	}
}

func TestHostMutationAfterReadOnlySnapshotCannotChangeTargetBytes(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	readonly := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(readonly, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: readonly, SourceType: "file", Target: "/readonly", Mode: "ro"})
	runner.afterStart = func() { _ = os.WriteFile(readonly, []byte("after!"), 0o600) }
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil || process == nil {
		t.Fatalf("process=%#v error=%v", process, err)
	}
	staged := mountedSource(runtime, "/readonly")
	for _, fact := range runtime.mountFacts {
		if fact.Target == "/readonly" && (!strings.HasPrefix(fact.Source, "kenogram-snapshot:sha256:") || fact.Source == staged || filepath.IsAbs(fact.Source)) {
			t.Fatalf("retained semantic source leaked or aliased staging path: fact=%q staged=%q", fact.Source, staged)
		}
	}
	raw, readErr := os.ReadFile(staged)
	if readErr != nil || string(raw) != "before" || staged == readonly {
		t.Fatalf("staged=%q raw=%q error=%v", staged, raw, readErr)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestStagedReadOnlyContentIsRevalidatedImmediatelyBeforeTargetUse(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	readonly := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(readonly, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: readonly, SourceType: "file", Target: "/readonly", Mode: "ro"})
	runner.afterStart = func() {
		for _, mount := range runner.mounts {
			if mount["Destination"] == "/readonly" {
				_ = os.WriteFile(mount["Source"].(string), []byte("tampered"), 0o600)
			}
		}
	}
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err == nil || process == nil || !strings.Contains(err.Error(), "source identity changed") {
		t.Fatalf("process=%#v error=%v", process, err)
	}
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestAuthenticatedLifecyclePreservesProviderReservedTargetExit(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{9}, 32)
	status := int64(125)
	path := filepath.Join(dir, joblifecycle.FileName)
	if err := joblifecycle.Write(path, joblifecycle.Record{Schema: joblifecycle.Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}, key); err != nil {
		t.Fatal(err)
	}
	p := &process{runtime: &Runtime{lifecycleFile: path, lifecycleKey: key}}
	target, err := p.recordTerminal(nil)
	if err != nil || target.ExitStatus == nil || *target.ExitStatus != 125 {
		t.Fatalf("target=%#v error=%v", target, err)
	}
}

func TestDirectRuntimeReportsUnprovenCleanupAbsence(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.failRemove = true
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "incomplete" || cleanup.ContainerAbsent || !hasReason(cleanup.Reasons, "CONTAINER_PRESENT") || !hasReason(cleanup.Reasons, "CONTAINER_REMOVE_FAILED") {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestDirectRuntimeRejectsProtectedControlSocketParent(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	dir := t.TempDir()
	socket := filepath.Join(dir, "opaque-endpoint")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_HOST", "unix://"+socket)
	invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{Source: dir, SourceType: "directory", Target: "/runtime", Mode: "ro"})
	if _, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "control socket") {
		t.Fatalf("error=%v", err)
	}
	if hasCall(runner.calls, "create") {
		t.Fatalf("container created: %v", runner.calls)
	}
}

func TestDirectRuntimeExtractsRequestedRegularArtifacts(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	invocation.Request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 2, MaxBytes: 1024}
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	final, err := process.Finalize(context.Background())
	if err != nil || len(final.Artifacts) != 1 || final.Artifacts[0].Path != "report.json" {
		t.Fatalf("final=%#v err=%v", final, err)
	}
	reader, err := final.Artifacts[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(reader)
	reader.Close()
	if string(raw) != `{"ok":true}` {
		t.Fatalf("artifact=%q", raw)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestDirectRuntimeRejectsArtifactSymlink(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	runner.artifactSymlink = true
	invocation.Request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 2, MaxBytes: 1024}
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := process.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("error=%v", err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestArtifactCollectorCopiesOnlyBoundedRegularTrees(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		source, destination := t.TempDir(), filepath.Join(t.TempDir(), "collected")
		if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "nested", "proof"), []byte("proof"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := collectArtifactTree(context.Background(), source, destination, 1, 5); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(destination, "nested", "proof"))
		if err != nil || string(raw) != "proof" {
			t.Fatalf("raw=%q error=%v", raw, err)
		}
	})
	t.Run("symlink in root prefix", func(t *testing.T) {
		mounted, destination := t.TempDir(), filepath.Join(t.TempDir(), "collected")
		if err := os.Mkdir(filepath.Join(mounted, "real"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(mounted, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := collectArtifactTreeFromMountedRoot(context.Background(), mounted, "/escape", destination, 1, 1024); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		source, destination := t.TempDir(), filepath.Join(t.TempDir(), "collected")
		if err := os.Symlink("/etc/passwd", filepath.Join(source, "escape")); err != nil {
			t.Fatal(err)
		}
		if err := collectArtifactTree(context.Background(), source, destination, 1, 1024); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("byte bound", func(t *testing.T) {
		source, destination := t.TempDir(), filepath.Join(t.TempDir(), "collected")
		if err := os.WriteFile(filepath.Join(source, "large"), []byte("too large"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := collectArtifactTree(context.Background(), source, destination, 1, 3); err == nil || !strings.Contains(err.Error(), "bounds") {
			t.Fatalf("error=%v", err)
		}
	})
}

func runtimeFixture(t *testing.T) (*Runtime, *fakePodmanRunner, *fakeAttachedProcess, job.Invocation) {
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
	path := filepath.Join(dir, "kenogram.toml")
	if err := os.WriteFile(path, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := app.PrepareBytes(declaration, path)
	if err != nil {
		t.Fatal(err)
	}
	lang := "C.UTF-8"
	invocation := job.Invocation{Prepared: prepared, Request: jobcontract.Request{
		Schema: jobcontract.RequestSchema, JobID: "job-1",
		Command: jobcontract.Command{Argv: []string{"/bin/true", "--probe"}, WorkingDirectory: "/workspace", Environment: []jobcontract.EnvironmentItem{{Name: "LANG", PublicValue: &lang}}},
		Limits:  jobcontract.Limits{TimeoutNS: int64(time.Second), FinalizeNS: int64(time.Second), StdoutMaxBytes: 1024, StderrMaxBytes: 1024},
	}}
	provenance, _, err := job.Provenance("", job.BuildIdentity{})
	if err != nil {
		t.Fatal(err)
	}
	invocation.Provenance = provenance
	runner := &fakePodmanRunner{}
	podman := backend.New(runner)
	podman.ReadProcStatus = func(int) ([]byte, error) { return []byte("Seccomp:\t2\n"), nil }
	podman.ReadProcessStart = func(int) string { return "start" }
	podman.MountIdentity = func(int, string, string) (bool, error) { return true, nil }
	podman.IPCIsolatedFromHost = func(int) (bool, error) { return true, nil }
	attached := newFakeAttached()
	runtime := New(podman)
	runtime.goos = "linux"
	temporaryRoot := t.TempDir()
	runtime.tempDir = func(_, pattern string) (string, error) { return os.MkdirTemp(temporaryRoot, pattern) }
	runtime.token = func() (string, error) { return strings.Repeat("d", 32), nil }
	runtime.starter = &fakeAttachedStarter{process: attached}
	runtime.now = func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) }
	return runtime, runner, attached, invocation
}

func hasCall(calls [][]string, parts ...string) bool {
	for _, call := range calls {
		for start := 0; start+len(parts) <= len(call); start++ {
			match := true
			for index, part := range parts {
				match = match && call[start+index] == part
			}
			if match {
				return true
			}
		}
	}
	return false
}

func mountedSource(runtime *Runtime, target string) string {
	for _, mount := range runtime.mounts {
		if mount.Target == target {
			return mount.Source
		}
	}
	return ""
}

func hasReason(reasons []string, wanted string) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}

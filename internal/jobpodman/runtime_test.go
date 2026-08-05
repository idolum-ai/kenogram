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
	"slices"
	"strconv"
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
			writeErr := joblifecycle.WriteSlot(f.lifecyclePath, joblifecycle.Record{Schema: joblifecycle.Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}, f.lifecycleKey)
			if writeErr != nil {
				err = writeErr
			}
		}
		f.done <- err
	})
}

type fakePodmanRunner struct {
	mu                   sync.Mutex
	exists               bool
	running              bool
	name                 string
	labels               map[string]string
	mounts               []map[string]any
	calls                [][]string
	onStop               func()
	failRemove           bool
	failCreateAfter      bool
	failWorkspaceCleanup bool
	ownerOverride        string
	artifactSymlink      bool
	imageDigest          string
	imageReference       string
	omitImageDigest      bool
	containerID          string
	cancelOnCreate       func()
	afterStart           func()
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
		switch {
		case len(args) == 15 && args[2] == "_job-collect" && args[3] == f.containerIDValue() && args[4] == f.name && args[5] == f.labels["io.kenogram.job-owner"]:
			if err := os.Mkdir(args[7], 0o700); err != nil {
				return nil, err
			}
			if f.artifactSymlink {
				return nil, os.Symlink("/etc/passwd", filepath.Join(args[7], "report.json"))
			}
			return nil, os.WriteFile(filepath.Join(args[7], "report.json"), []byte(`{"ok":true}`), 0o600)
		case len(args) == 11 && args[2] == "_job-clean-workspaces-after-container" && containerIDPattern.MatchString(args[3]) && args[4] == f.labels["io.kenogram.job-owner"]:
			if f.failWorkspaceCleanup {
				return nil, errors.New("post-container workspace cleanup failed")
			}
			if f.exists && f.containerIDValue() == args[3] {
				return nil, errors.New("container is still present")
			}
			device, deviceErr := strconv.ParseUint(args[8], 10, 64)
			inode, inodeErr := strconv.ParseUint(args[9], 10, 64)
			if deviceErr != nil || inodeErr != nil {
				return nil, errors.New("invalid cleanup identity")
			}
			identity, identityErr := filesystemIdentityAt(args[7])
			record, recordErr := readWorkspaceCleanupAuthority(args[7], args[10])
			if identityErr != nil || identity != (FilesystemIdentity{Device: device, Inode: inode}) || recordErr != nil ||
				record.ContainerID != args[3] || record.OwnerToken != args[4] || record.PlanDigest != args[5] || record.DeclarationDigest != args[6] {
				return nil, errors.Join(identityErr, recordErr, errors.New("post-container cleanup authority mismatch"))
			}
			for _, binding := range record.Bindings {
				if err := clearWorkspaceContents(ctx, binding); err != nil {
					return nil, err
				}
			}
			return nil, nil
		default:
			return nil, errors.New("governed helper authority mismatch")
		}
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

func TestCleanupDoesNotMutateWhileFinalizeWorkerIsUnjoined(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	started, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	process := started.(*process)
	process.BeginFinalization()
	before := len(runner.calls)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cleanup := runtime.Cleanup(ctx, invocation)
	if cleanup.Status != "incomplete" || cleanup.ContainerAbsent || !slices.Contains(cleanup.Reasons, "FINALIZATION_WORKER_PRESENT") || len(runner.calls) != before || !runner.exists {
		t.Fatalf("cleanup=%#v calls=%v before=%d exists=%t", cleanup, runner.calls, before, runner.exists)
	}
	if _, err := os.Lstat(runtime.scratch); err != nil {
		t.Fatalf("active finalization scratch was mutated: %v", err)
	}
	process.finishFinalization()
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup after join=%#v", cleanup)
	}
}

func TestCleanupDoesNotMutateWhileWaitWorkerIsUnjoined(t *testing.T) {
	runtime, runner, _, invocation := runtimeFixture(t)
	started, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	process := started.(*process)
	process.BeginWait()
	before := len(runner.calls)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cleanup := runtime.Cleanup(ctx, invocation)
	if cleanup.Status != "incomplete" || cleanup.ContainerAbsent || !slices.Contains(cleanup.Reasons, "WAIT_WORKER_PRESENT") || len(runner.calls) != before || !runner.exists {
		t.Fatalf("cleanup=%#v calls=%v before=%d exists=%t", cleanup, runner.calls, before, runner.exists)
	}
	if _, err := os.Lstat(runtime.scratch); err != nil {
		t.Fatalf("active Wait scratch was mutated: %v", err)
	}
	process.finishWait()
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup after Wait join=%#v", cleanup)
	}
}

func TestFinalizeDoesNotContactProviderWhileWaitWorkerIsActive(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	started, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	process := started.(*process)
	process.BeginWait()
	waitDone := make(chan error, 1)
	go func() {
		_, err := process.Wait(context.Background())
		waitDone <- err
	}()
	before := len(runner.calls)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := process.Finalize(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Finalize error=%v", err)
	}
	if len(runner.calls) != before {
		t.Fatalf("Finalize contacted provider while Wait remained active: before=%d calls=%v", before, runner.calls)
	}
	attached.finish(nil)
	if err := <-waitDone; err != nil {
		t.Fatal(err)
	}
	if _, err := process.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize after Wait join: %v", err)
	}
	if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func TestCleanupRetriesExactWorkspacesAfterContainerAbsence(t *testing.T) {
	runtime, runner, attached, invocation := runtimeFixture(t)
	invocation.Request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/workspace/artifacts", MaxEntries: 2, MaxBytes: 1024}
	started, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	attached.finish(nil)
	if _, err := started.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := started.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	scratch := runtime.scratch
	runner.failWorkspaceCleanup = true
	first := runtime.Cleanup(context.Background(), invocation)
	if first.Status != "incomplete" || !first.ContainerAbsent || !slices.Contains(first.Reasons, "WORKSPACE_NAMESPACE_CLEANUP_FAILED") || !slices.Contains(first.Reasons, "SCRATCH_RETAINED_FOR_NAMESPACE_CLEANUP") {
		t.Fatalf("first cleanup=%#v", first)
	}
	if _, err := os.Lstat(filepath.Join(scratch, workspaceCleanupAuthorityName)); err != nil {
		t.Fatalf("retry authority was not retained: %v", err)
	}
	runner.failWorkspaceCleanup = false
	second := runtime.Cleanup(context.Background(), invocation)
	if second.Status != "complete" || !second.ContainerAbsent {
		t.Fatalf("second cleanup=%#v calls=%v", second, runner.calls)
	}
	if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch remains after successful post-absence retry: %v", err)
	}
	removeIndex, helperIndex := -1, -1
	for index, call := range runner.calls {
		if len(call) != 0 && call[0] == "rm" && removeIndex < 0 {
			removeIndex = index
		}
		if len(call) > 2 && call[0] == "unshare" && call[2] == "_job-clean-workspaces-after-container" && helperIndex < 0 {
			helperIndex = index
		}
	}
	if removeIndex < 0 || helperIndex <= removeIndex {
		t.Fatalf("workspace retry did not occur after container destroy: calls=%v", runner.calls)
	}
	if helperIndex == 0 || len(runner.calls[helperIndex-1]) < 4 || !slices.Equal(runner.calls[helperIndex-1], []string{"ps", "--all", "--no-trunc", "--format", "{{.ID}}"}) {
		t.Fatalf("immutable absence was not proved immediately before namespace entry: calls=%v", runner.calls)
	}
}

func TestWorkspaceCleanupFailureClassificationIsStableAndNonSensitive(t *testing.T) {
	for _, test := range []struct {
		stage  string
		code   int
		reason string
	}{
		{stage: "container_absence", code: workspaceCleanupExitAbsence, reason: "WORKSPACE_NAMESPACE_CONTAINER_ABSENCE_UNPROVED"},
		{stage: "scratch_identity", code: workspaceCleanupExitScratch, reason: "WORKSPACE_NAMESPACE_SCRATCH_IDENTITY_FAILED"},
		{stage: "authority_record", code: workspaceCleanupExitAuthority, reason: "WORKSPACE_NAMESPACE_AUTHORITY_RECORD_FAILED"},
		{stage: "workspace_contents", code: workspaceCleanupExitContents, reason: "WORKSPACE_NAMESPACE_CONTENT_REMOVAL_FAILED"},
	} {
		err := workspaceCleanupError(test.stage, errors.New("sensitive/provider/path detail"))
		if code := WorkspaceCleanupExitCode(err); code != test.code {
			t.Fatalf("stage=%s code=%d want=%d", test.stage, code, test.code)
		}
		if reason := workspaceCleanupFailureReason(err); reason != test.reason || strings.Contains(reason, "sensitive") || strings.Contains(reason, "path") {
			t.Fatalf("stage=%s reason=%q", test.stage, reason)
		}
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

func TestPortableWritableWorkspaceIsPrivateOutsideAndPortableInside(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	declaredWritable := filepath.Join(t.TempDir(), "operator-owned")
	if err := os.Mkdir(declaredWritable, 0o700); err != nil {
		t.Fatal(err)
	}
	invocation.Prepared.Result.Plan.Mounts = append(invocation.Prepared.Result.Plan.Mounts, plan.Mount{
		Source: declaredWritable, SourceType: "directory", Target: "/operator-owned", Mode: "rw",
	})
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	scratchInfo, scratchErr := os.Stat(runtime.scratch)
	workspace := mountedSource(runtime, "/workspace")
	workspaceInfo, workspaceErr := os.Stat(workspace)
	declaredInfo, declaredErr := os.Stat(declaredWritable)
	if scratchErr != nil || scratchInfo.Mode().Perm() != 0o700 {
		t.Fatalf("outer scratch mode=%v error=%v", scratchInfo, scratchErr)
	}
	if workspaceErr != nil || workspaceInfo.Mode().Perm() != 0o777 {
		t.Fatalf("portable workspace mode=%v error=%v", workspaceInfo, workspaceErr)
	}
	relative, err := filepath.Rel(runtime.scratch, workspace)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("workspace %q is outside scratch %q: relative=%q error=%v", workspace, runtime.scratch, relative, err)
	}
	for parent := filepath.Dir(workspace); ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("workspace parent %q is not host-private: info=%v error=%v", parent, info, err)
		}
		if parent == runtime.scratch {
			break
		}
	}
	if declaredErr != nil || declaredInfo.Mode().Perm() != 0o700 {
		t.Fatalf("declared writable authority was reprojected: mode=%v error=%v", declaredInfo, declaredErr)
	}
	for _, fact := range runtime.mountFacts {
		switch fact.Target {
		case "/workspace":
			if fact.PermissionPolicy != jobcontract.RuntimeWorkspacePermissionPolicy || fact.AuthoritySHA256 != "" || fact.SHA256 != "" {
				t.Fatalf("workspace fact=%#v", fact)
			}
		case "/operator-owned":
			if fact.PermissionPolicy != "" || fact.AuthoritySHA256 != "" || fact.SHA256 != "" {
				t.Fatalf("declared writable fact invented a projection: %#v", fact)
			}
		}
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

func TestWorkspacePermissionSubstitutionInvalidatesFinalization(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "missing write bits", mode: 0o700},
		{name: "sticky bit", mode: 0o777 | os.ModeSticky},
		{name: "setgid bit", mode: 0o777 | os.ModeSetgid},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			if test.mode&os.ModeSetgid != 0 {
				// Some hosts refuse setgid when a newly created directory
				// inherits a group outside the caller's memberships.
				if err := os.Chown(workspace, -1, os.Getgid()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(workspace, test.mode); err != nil {
				t.Fatal(err)
			}
			observed, err := os.Stat(workspace)
			if err != nil || observed.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != test.mode {
				t.Fatalf("workspace mode substitution was not established: mode=%v error=%v", observed, err)
			}
			if _, err := process.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "source identity changed") {
				t.Fatalf("error=%v", err)
			}
		})
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

func TestReadOnlySnapshotProjectsPrivateSourcesWithoutMutatingAuthority(t *testing.T) {
	t.Run("0600 regular file", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "private-input")
		if err := os.WriteFile(source, []byte("private\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := sourcetree.Digest(context.Background(), source)
		if err != nil {
			t.Fatal(err)
		}
		authorityInfo, err := os.Lstat(source)
		if err != nil {
			t.Fatal(err)
		}
		authorityStat := authorityInfo.Sys().(*syscall.Stat_t)
		snapshots, err := snapshotReadOnlyMounts(context.Background(), t.TempDir(), []plan.Mount{{Source: source, SourceType: "file", Target: "/input", Mode: "ro"}})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := snapshots["/input"]
		staged, err := os.Stat(snapshot.path)
		if err != nil || staged.Mode().Perm() != 0o444 || staged.Mode().Perm()&0o222 != 0 {
			t.Fatalf("staged=%v error=%v", staged, err)
		}
		raw, err := os.ReadFile(snapshot.path)
		if err != nil || string(raw) != "private\n" {
			t.Fatalf("raw=%q error=%v", raw, err)
		}
		projected, err := sourcetree.Digest(context.Background(), snapshot.path)
		if err != nil || snapshot.authorityDigest != "sha256:"+before || snapshot.digest != "sha256:"+projected || snapshot.authorityDigest == snapshot.digest || snapshot.policy != jobcontract.RuntimeReadOnlyPermissionPolicy {
			t.Fatalf("snapshot=%#v digest=%q error=%v", snapshot, projected, err)
		}
		after, err := sourcetree.Digest(context.Background(), source)
		sourceInfo, statErr := os.Stat(source)
		if err != nil || statErr != nil {
			t.Fatalf("source digest/stat errors=%v/%v", err, statErr)
		}
		afterStat := sourceInfo.Sys().(*syscall.Stat_t)
		if before != after || sourceInfo.Mode().Perm() != 0o600 || !os.SameFile(authorityInfo, sourceInfo) || authorityStat.Dev != afterStat.Dev || authorityStat.Ino != afterStat.Ino || authorityStat.Uid != afterStat.Uid || authorityStat.Gid != afterStat.Gid {
			t.Fatalf("source before=%q after=%q mode=%v errors=%v/%v", before, after, sourceInfo.Mode().Perm(), err, statErr)
		}
	})

	t.Run("0700 nested tree", func(t *testing.T) {
		source := t.TempDir()
		if err := os.Chmod(source, 0o700); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(source, "nested")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "data"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "tool"), []byte("tool"), 0o700); err != nil {
			t.Fatal(err)
		}
		before, err := sourcetree.Digest(context.Background(), source)
		if err != nil {
			t.Fatal(err)
		}
		snapshots, err := snapshotReadOnlyMounts(context.Background(), t.TempDir(), []plan.Mount{{Source: source, SourceType: "directory", Target: "/input", Mode: "ro"}})
		if err != nil {
			t.Fatal(err)
		}
		snapshot := snapshots["/input"]
		for relative, want := range map[string]os.FileMode{".": 0o555, "nested": 0o555, "nested/data": 0o444, "nested/tool": 0o555} {
			info, err := os.Stat(filepath.Join(snapshot.path, filepath.FromSlash(relative)))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want || info.Mode().Perm()&0o222 != 0 {
				t.Fatalf("%s mode=%v want=%v error=%v", relative, info.Mode().Perm(), want, err)
			}
		}
		after, err := sourcetree.Digest(context.Background(), source)
		if err != nil || before != after {
			t.Fatalf("source before=%q after=%q error=%v", before, after, err)
		}
		for relative, want := range map[string]os.FileMode{".": 0o700, "nested": 0o700, "nested/data": 0o600, "nested/tool": 0o700} {
			info, err := os.Stat(filepath.Join(source, filepath.FromSlash(relative)))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != want {
				t.Fatalf("source %s mode=%v want=%v error=%v", relative, info.Mode().Perm(), want, err)
			}
		}
		if err := sourcetree.PrepareRemoval(context.Background(), snapshot.path); err != nil {
			t.Fatal(err)
		}
	})

	for _, test := range []struct {
		name  string
		build func(string) error
	}{
		{name: "symlink", build: func(root string) error { return os.Symlink("outside", filepath.Join(root, "link")) }},
		{name: "special node", build: func(root string) error { return syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			if err := test.build(source); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshotReadOnlyMounts(context.Background(), t.TempDir(), []plan.Mount{{Source: source, SourceType: "directory", Target: "/input", Mode: "ro"}}); err == nil {
				t.Fatal("invalid private source tree was projected")
			}
		})
	}

	t.Run("partial projection remains removable", func(t *testing.T) {
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, "data"), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		scratch := t.TempDir()
		_, err := snapshotReadOnlyMountsWithProject(context.Background(), scratch, []plan.Mount{{Source: source, SourceType: "directory", Target: "/input", Mode: "ro"}}, func(_ context.Context, staged string) error {
			if err := os.Chmod(staged, 0o555); err != nil {
				return err
			}
			return errors.New("injected projection failure")
		})
		if err == nil || !strings.Contains(err.Error(), "injected projection failure") {
			t.Fatalf("error=%v", err)
		}
		if err := os.RemoveAll(filepath.Join(scratch, "read-only-mounts")); err != nil {
			t.Fatalf("partially projected scratch is not removable: %v", err)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "projector byte mutation", mutate: func(staged string) error {
			path := filepath.Join(staged, "data")
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
				return err
			}
			return sourcetree.ProjectReadOnly(context.Background(), staged)
		}},
		{name: "projector path mutation", mutate: func(staged string) error {
			if err := os.Chmod(staged, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(staged, "extra"), []byte("extra"), 0o600); err != nil {
				return err
			}
			return sourcetree.ProjectReadOnly(context.Background(), staged)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "data"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			_, err := snapshotReadOnlyMountsWithProject(context.Background(), scratch, []plan.Mount{{Source: source, SourceType: "directory", Target: "/input", Mode: "ro"}}, func(ctx context.Context, staged string) error {
				if err := sourcetree.ProjectReadOnly(ctx, staged); err != nil {
					return err
				}
				return test.mutate(staged)
			})
			if err == nil || !strings.Contains(err.Error(), "changed content or inventory") {
				t.Fatalf("error=%v", err)
			}
			if err := os.RemoveAll(filepath.Join(scratch, "read-only-mounts")); err != nil {
				t.Fatalf("rejected projection scratch is not removable: %v", err)
			}
		})
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
	if err := os.Chmod(staged, 0o600); err != nil {
		t.Fatal(err)
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
	projectedDigest, digestErr := sourcetree.Digest(context.Background(), staged)
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	for _, fact := range runtime.mountFacts {
		if fact.Target == "/readonly" && (!strings.HasPrefix(fact.Source, "kenogram-snapshot:sha256:") || fact.Source == staged || filepath.IsAbs(fact.Source) || fact.SHA256 != "sha256:"+projectedDigest || fact.AuthoritySHA256 == "" || fact.PermissionPolicy != jobcontract.RuntimeReadOnlyPermissionPolicy) {
			t.Fatalf("retained snapshot authority is incomplete or aliases staging: fact=%#v staged=%q", fact, staged)
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
				_ = os.Chmod(mount["Source"].(string), 0o600)
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
	identity, err := joblifecycle.Prepare(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := joblifecycle.WriteSlot(path, joblifecycle.Record{Schema: joblifecycle.Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}, key); err != nil {
		t.Fatal(err)
	}
	p := &process{runtime: &Runtime{lifecycleFile: path, lifecycleKey: key, lifecycleID: identity}}
	target, err := p.recordTerminal(nil)
	if err != nil || target.ExitStatus == nil || *target.ExitStatus != 125 {
		t.Fatalf("target=%#v error=%v", target, err)
	}
}

func TestDirectRuntimeStagesOnlyOneWritableLifecycleInode(t *testing.T) {
	runtime, _, attached, invocation := runtimeFixture(t)
	process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := os.Stat(filepath.Dir(runtime.lifecycleFile))
	if err != nil || parent.Mode().Perm() != 0o700 {
		t.Fatalf("parent=%v error=%v", parent, err)
	}
	slot, err := os.Lstat(runtime.lifecycleFile)
	if err != nil || !slot.Mode().IsRegular() || slot.Mode().Perm() != 0o622 || slot.Size() != 0 {
		t.Fatalf("slot=%v error=%v", slot, err)
	}
	var observed bool
	for _, mount := range runtime.mounts {
		if mount.Target == jobLifecyclePath {
			observed = mount.Source == runtime.lifecycleFile && mount.Mode == "rw" && mount.NoExec
		}
	}
	if !observed {
		t.Fatalf("exact lifecycle file mount is absent: %#v", runtime.mounts)
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

func TestDirectRuntimeLifecycleTamperingFailsClosed(t *testing.T) {
	t.Run("prewrite", func(t *testing.T) {
		runtime, _, attached, invocation := runtimeFixture(t)
		process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runtime.lifecycleFile, []byte("target prewrite"), 0o622); err != nil {
			t.Fatal(err)
		}
		attached.finish(nil)
		if target, err := process.Wait(context.Background()); err == nil || target.Kind != "unknown" {
			t.Fatalf("target=%#v error=%v", target, err)
		}
		if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
			t.Fatalf("cleanup=%#v", cleanup)
		}
	})

	t.Run("after observation", func(t *testing.T) {
		runtime, _, attached, invocation := runtimeFixture(t)
		process, err := runtime.Start(context.Background(), invocation, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		attached.finish(nil)
		if _, err := process.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runtime.lifecycleFile, []byte("orphan corruption"), 0o622); err != nil {
			t.Fatal(err)
		}
		if _, err := process.Finalize(context.Background()); err == nil || !strings.Contains(err.Error(), "lifecycle changed") {
			t.Fatalf("error=%v", err)
		}
		if cleanup := runtime.Cleanup(context.Background(), invocation); cleanup.Status != "complete" {
			t.Fatalf("cleanup=%#v", cleanup)
		}
	})
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

func TestArtifactCollectionResolvesDeepestBoundMountWithoutMutation(t *testing.T) {
	outer := t.TempDir()
	inner := t.TempDir()
	if err := os.Mkdir(filepath.Join(inner, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	wanted := []byte("bound artifact\n")
	artifact := filepath.Join(inner, "artifacts", "report.txt")
	if err := os.WriteFile(artifact, wanted, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(artifact)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := filesystemIdentityAt(inner)
	if err != nil {
		t.Fatal(err)
	}
	runner := stoppedArtifactRunner([]map[string]any{
		{"Source": outer, "Destination": "/workspace", "RW": true, "Mode": "rw,nodev,nosuid", "Options": []string{"rw", "nodev", "nosuid"}},
		{"Source": inner, "Destination": "/workspace/nested", "RW": true, "Mode": "rw,nodev,nosuid", "Options": []string{"rw", "nodev", "nosuid"}},
	})
	destination := filepath.Join(t.TempDir(), "collected")
	binding := ArtifactMountBinding{Role: "declared", Target: "/workspace/nested", Device: identity.Device, Inode: identity.Inode}
	err = CollectArtifacts(context.Background(), backend.New(runner), runner.containerIDValue(), runner.name, runner.labels["io.kenogram.job-owner"], "/workspace/nested/artifacts", destination, 2, 1024, binding)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := os.ReadFile(filepath.Join(destination, "report.txt"))
	after, statErr := os.Stat(artifact)
	if readErr != nil || statErr != nil || !bytes.Equal(got, wanted) || before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("got=%q read=%v stat=%v before=%v after=%v", got, readErr, statErr, before, after)
	}
	if hasCall(runner.calls, "mount") || hasCall(runner.calls, "unmount") {
		t.Fatalf("bind-aware collection consulted storage root: %v", runner.calls)
	}
}

func TestArtifactCollectionRejectsChangedOrEscapingBindAuthority(t *testing.T) {
	source := t.TempDir()
	identity, err := filesystemIdentityAt(source)
	if err != nil {
		t.Fatal(err)
	}
	runner := stoppedArtifactRunner([]map[string]any{{"Source": source, "Destination": "/workspace", "RW": true, "Mode": "rw,nodev,nosuid", "Options": []string{"rw", "nodev", "nosuid"}}})
	base := ArtifactMountBinding{Role: "declared", Target: "/workspace", Device: identity.Device, Inode: identity.Inode}
	wrong := base
	wrong.Inode++
	if err := CollectArtifacts(context.Background(), backend.New(runner), runner.containerIDValue(), runner.name, runner.labels["io.kenogram.job-owner"], "/workspace/artifacts", filepath.Join(t.TempDir(), "changed"), 2, 1024, wrong); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("changed identity error=%v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if err := CollectArtifacts(context.Background(), backend.New(runner), runner.containerIDValue(), runner.name, runner.labels["io.kenogram.job-owner"], "/workspace/artifacts", filepath.Join(t.TempDir(), "escape"), 2, 1024, base); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink escape error=%v", err)
	}
}

func TestArtifactCollectionAcceptsOnlyExactWorkspaceProjection(t *testing.T) {
	scratch := t.TempDir()
	layout := worldfs.For(scratch, "ephemeral")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	workspace, err := layout.EnsurePortableWritableWorkspace("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "artifacts", "report"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := filesystemIdentityAt(workspace)
	if err != nil {
		t.Fatal(err)
	}
	runner := stoppedArtifactRunner([]map[string]any{{"Source": workspace, "Destination": "/workspace", "RW": true, "Mode": "rw,nodev,nosuid", "Options": []string{"rw", "nodev", "nosuid"}}})
	binding := ArtifactMountBinding{Role: "workspace", Target: "/workspace", Scratch: scratch, Device: identity.Device, Inode: identity.Inode}
	destination := filepath.Join(t.TempDir(), "exact")
	if err := CollectArtifacts(context.Background(), backend.New(runner), runner.containerIDValue(), runner.name, runner.labels["io.kenogram.job-owner"], "/workspace/artifacts", destination, 1, 1024, binding); err != nil {
		t.Fatal(err)
	}
	wrong := binding
	wrong.Scratch = t.TempDir()
	if err := CollectArtifacts(context.Background(), backend.New(runner), runner.containerIDValue(), runner.name, runner.labels["io.kenogram.job-owner"], "/workspace/artifacts", filepath.Join(t.TempDir(), "wrong"), 1, 1024, wrong); err == nil || !strings.Contains(err.Error(), "not Kenogram-owned") {
		t.Fatalf("wrong workspace projection error=%v", err)
	}
}

func TestWorkspaceNamespaceCleanupIsExactAndIdempotent(t *testing.T) {
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(scratch, "ephemeral")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	workspace, err := layout.EnsurePortableWritableWorkspace("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "target"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(workspace, "nested", "link")); err != nil {
		t.Fatal(err)
	}
	declared := t.TempDir()
	if err := os.WriteFile(filepath.Join(declared, "operator"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := filesystemIdentityAt(workspace)
	if err != nil {
		t.Fatal(err)
	}
	scratchID, err := filesystemIdentityAt(scratch)
	if err != nil {
		t.Fatal(err)
	}
	facts := []jobcontract.RuntimeMountObservation{{Role: "workspace", PermissionPolicy: jobcontract.RuntimeWorkspacePermissionPolicy, Target: "/workspace", Mode: "rw", Device: workspaceID.Device, Inode: workspaceID.Inode, FileType: "directory"}}
	mounts := []backend.Mount{{Source: workspace, Target: "/workspace", Mode: "rw"}, {Source: declared, Target: "/operator", Mode: "rw"}}
	bindings, digest, err := workspaceCleanupAuthority(scratch, facts, mounts)
	if err != nil {
		t.Fatal(err)
	}
	runner := stoppedArtifactRunner([]map[string]any{
		{"Source": workspace, "Destination": "/workspace", "RW": true, "Mode": "rw,nodev,nosuid", "Options": []string{"rw", "nodev", "nosuid"}},
		{"Source": declared, "Destination": "/operator", "RW": true, "Mode": "rw,nodev,nosuid", "Options": []string{"rw", "nodev", "nosuid"}},
	})
	authorityFileDigest, err := persistWorkspaceCleanupAuthority(
		scratch, runner.containerIDValue(), runner.labels["io.kenogram.job-owner"],
		runner.labels["io.kenogram.plan-digest"], runner.labels["io.kenogram.declaration-digest"],
		scratchID, bindings, digest,
	)
	if err != nil {
		t.Fatal(err)
	}
	runner.exists = false
	for attempt := 0; attempt < 2; attempt++ {
		err = CleanupWorkspaceContentsAfterContainer(
			context.Background(), runner.containerIDValue(),
			runner.labels["io.kenogram.job-owner"], runner.labels["io.kenogram.plan-digest"],
			runner.labels["io.kenogram.declaration-digest"], scratch, scratchID,
			authorityFileDigest,
		)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	entries, err := os.ReadDir(workspace)
	preserved, preserveErr := os.ReadFile(filepath.Join(declared, "operator"))
	if err != nil || len(entries) != 0 || preserveErr != nil || string(preserved) != "preserve" {
		t.Fatalf("workspace=%v read=%v declared=%q error=%v", entries, err, preserved, preserveErr)
	}
}

func TestObservedWorkspaceCleanupBindingsAcceptPodman49ReadWriteRepresentation(t *testing.T) {
	scratch := t.TempDir()
	layout := worldfs.For(scratch, "ephemeral")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	workspace, err := layout.EnsurePortableWritableWorkspace("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := filesystemIdentityAt(workspace)
	if err != nil {
		t.Fatal(err)
	}

	// Podman 4.9 reports read/write authority in RW and may reserve Mode and
	// Options for the remaining mount attributes instead of duplicating "rw".
	bindings, err := observedWorkspaceCleanupBindings(scratch, []backend.EvidenceMount{{
		Source: workspace, Destination: "/workspace", RW: true,
		Mode: "", Options: []string{"rbind", "rprivate", "nodev", "nosuid"},
	}})
	if err != nil || len(bindings) != 1 || bindings[0].Target != "/workspace" || bindings[0].Source != workspace ||
		bindings[0].Device != identity.Device || bindings[0].Inode != identity.Inode {
		t.Fatalf("bindings=%#v error=%v", bindings, err)
	}
}

func TestObservedWorkspaceCleanupBindingsRejectsMissingAuthorityOrHardening(t *testing.T) {
	scratch := t.TempDir()
	layout := worldfs.For(scratch, "ephemeral")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	workspace, err := layout.EnsurePortableWritableWorkspace("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		rw      bool
		options []string
	}{
		{name: "read-only despite textual rw", rw: false, options: []string{"rw", "nodev", "nosuid"}},
		{name: "missing nodev", rw: true, options: []string{"nosuid"}},
		{name: "missing nosuid", rw: true, options: []string{"nodev"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bindings, err := observedWorkspaceCleanupBindings(scratch, []backend.EvidenceMount{{
				Source: workspace, Destination: "/workspace", RW: test.rw, Options: test.options,
			}})
			if err == nil || bindings != nil {
				t.Fatalf("bindings=%#v error=%v", bindings, err)
			}
		})
	}
}

func TestWorkspaceNamespaceCleanupFailureIsRetained(t *testing.T) {
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
	runner.failWorkspaceCleanup = true
	cleanup := runtime.Cleanup(context.Background(), invocation)
	if cleanup.Status != "incomplete" || !cleanup.ContainerAbsent || !hasReason(cleanup.Reasons, "WORKSPACE_NAMESPACE_CLEANUP_FAILED") {
		t.Fatalf("cleanup=%#v", cleanup)
	}
}

func stoppedArtifactRunner(mounts []map[string]any) *fakePodmanRunner {
	return &fakePodmanRunner{
		exists: true, name: "governed-job", mounts: mounts,
		labels: map[string]string{
			"io.kenogram.job-owner":          strings.Repeat("d", 32),
			"io.kenogram.plan-digest":        strings.Repeat("a", 64),
			"io.kenogram.declaration-digest": strings.Repeat("b", 64),
		},
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

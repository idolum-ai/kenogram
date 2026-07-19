package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/proxy"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

type crashContainer struct {
	Result     plan.Result     `json:"result"`
	Generation int64           `json:"generation"`
	Running    bool            `json:"running"`
	Services   map[string]bool `json:"services,omitempty"`
}

type crashRunner struct {
	results    map[string]plan.Result
	containers map[string]*crashContainer
	mount      string
	statePath  string
	calls      []string
	failPrefix string
	failed     bool
}

func newCrashRunner(mount, statePath string, results ...plan.Result) *crashRunner {
	runner := &crashRunner{results: map[string]plan.Result{}, containers: map[string]*crashContainer{}, mount: mount, statePath: statePath}
	for _, result := range results {
		runner.results[result.PlanDigest] = result
	}
	if raw, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(raw, &runner.containers); err != nil {
			panic(err)
		}
		for _, container := range runner.containers {
			if container.Services == nil {
				container.Services = map[string]bool{}
			}
		}
	}
	return runner
}

func (r *crashRunner) save() error {
	raw, err := json.Marshal(r.containers)
	if err != nil {
		return err
	}
	temporary := r.statePath + ".tmp"
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, r.statePath)
}

func (r *crashRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	call := strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if !r.failed && r.failPrefix != "" && strings.HasPrefix(call, r.failPrefix) {
		r.failed = true
		return nil, fmt.Errorf("injected %s failure", r.failPrefix)
	}
	switch args[0] {
	case "info":
		return []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}}}`), nil
	case "create":
		name := argumentAfter(args, "--name")
		planDigest := labelValue(args, "io.kenogram.plan-digest")
		generation, _ := strconv.ParseInt(labelValue(args, "io.kenogram.generation"), 10, 64)
		result, ok := r.results[planDigest]
		if name == "" || generation <= 0 || !ok {
			return nil, fmt.Errorf("unknown crash fixture create: %v", args)
		}
		r.containers[name] = &crashContainer{Result: result, Generation: generation, Services: map[string]bool{}}
		if err := r.save(); err != nil {
			return nil, err
		}
		return []byte(name), nil
	case "start":
		if container := r.containers[args[len(args)-1]]; container != nil {
			container.Running = true
		}
		return nil, r.save()
	case "stop":
		if container := r.containers[args[len(args)-1]]; container != nil {
			container.Running = false
			container.Services = map[string]bool{}
		}
		return nil, r.save()
	case "rm":
		delete(r.containers, args[len(args)-1])
		return nil, r.save()
	case "ps":
		names := make([]string, 0, len(r.containers))
		for name := range r.containers {
			names = append(names, name)
		}
		sort.Strings(names)
		return []byte(strings.Join(names, "\n") + "\n"), nil
	case "inspect":
		name := args[len(args)-1]
		container := r.containers[name]
		if container == nil {
			return nil, fmt.Errorf("container %s absent", name)
		}
		return r.inspect(name, container)
	case "exec":
		name := argumentAfter(args, "--detach")
		if name != "" {
			container := r.containers[name]
			service := serviceNameFromArgs(args)
			if container != nil && service != "" {
				if container.Services == nil {
					container.Services = map[string]bool{}
				}
				container.Services[service] = true
				return nil, r.save()
			}
		}
		name = args[1]
		container := r.containers[name]
		service := serviceNameFromArgs(args)
		if container != nil && service != "" && container.Services[service] {
			return []byte("supervised\n"), nil
		}
		return nil, fmt.Errorf("service observation unavailable")
	default:
		return []byte("ok"), nil
	}
}

func serviceNameFromArgs(args []string) string {
	joined := strings.Join(args, " ")
	for _, marker := range []string{"/etc/kenogram/services/", "/run/kenogram/services/"} {
		index := strings.Index(joined, marker)
		if index < 0 {
			continue
		}
		name := joined[index+len(marker):]
		name = strings.Trim(name, "'\"")
		name = strings.TrimSuffix(name, ".sh")
		if end := strings.IndexAny(name, " ;'\""); end >= 0 {
			name = name[:end]
		}
		return name
	}
	return ""
}

func (r *crashRunner) inspect(name string, container *crashContainer) ([]byte, error) {
	result := container.Result
	pid := 0
	if container.Running {
		pid = 321
	}
	document := []map[string]any{{
		"Name": name, "BoundingCaps": []string{},
		"State": map[string]any{"Running": container.Running, "Pid": pid},
		"IDMappings": map[string]any{
			"UidMap": []map[string]any{{"ContainerID": os.Getuid(), "HostID": os.Getuid(), "Size": 1}},
			"GidMap": []map[string]any{{"ContainerID": os.Getgid(), "HostID": os.Getgid(), "Size": 1}},
		},
		"Config": map[string]any{
			"User": result.Plan.World.User, "Hostname": result.Plan.World.Hostname, "WorkingDir": result.Plan.World.Workdir,
			"Labels": map[string]string{
				"io.kenogram.world": result.Plan.Name, "io.kenogram.generation": strconv.FormatInt(container.Generation, 10),
				"io.kenogram.plan-digest": result.PlanDigest, "io.kenogram.declaration-digest": result.DeclarationDigest,
			},
		},
		"HostConfig": map[string]any{
			"NetworkMode": "none", "IpcMode": "private", "PidMode": "private", "UTSMode": "private", "UsernsMode": "",
			"CapDrop": []string{"CAP_ALL"}, "SecurityOpt": []string{"no-new-privileges"},
			"Memory": result.Plan.Resources.MemoryBytes, "NanoCpus": result.Plan.Resources.CPUs * 1_000_000_000, "PidsLimit": result.Plan.Resources.PIDs,
		},
		"Mounts": []map[string]any{{"Source": r.mount, "Destination": "/workspace", "RW": true, "Mode": "rw,nodev,nosuid"}},
	}}
	return json.Marshal(document)
}

func (*crashRunner) Start(context.Context, string, ...string) error       { return nil }
func (*crashRunner) Interactive(context.Context, string, ...string) error { return nil }

func argumentAfter(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key {
			return args[index+1]
		}
	}
	return ""
}

func labelValue(args []string, key string) string {
	prefix := key + "="
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--label" && strings.HasPrefix(args[index+1], prefix) {
			return strings.TrimPrefix(args[index+1], prefix)
		}
	}
	return ""
}

func crashPrepared(marker string, pids int64) Prepared {
	prepared := preparedFixture()
	prepared.Raw = []byte("version = 1\n# " + marker + "\n")
	prepared.Path = marker + ".toml"
	prepared.Result.Plan.Resources.PIDs = pids
	prepared.Result.Plan.NetworkAllow = []plan.NetworkAllow{{Host: "example.test", Port: 443}}
	prepared.Result.Plan.Services = []plan.Service{{Name: "worker", Command: []string{"/bin/true"}, Autostart: true, Restart: "never"}}
	canonical, err := plan.Canonical(prepared.Result.Plan)
	if err != nil {
		panic(err)
	}
	prepared.Result.PlanDigest = DeclarationDigest(canonical)
	prepared.Result.DeclarationDigest = DeclarationDigest(prepared.Raw)
	return prepared
}

func crashBackend(runner *crashRunner) *backend.Podman {
	podman := backend.New(runner)
	podman.ReadProcStatus = func(int) ([]byte, error) { return []byte("Seccomp:\t2\n"), nil }
	podman.ReadProcessStart = func(int) string { return "test-process-start" }
	podman.MountIdentity = func(int, string, string) (bool, error) { return true, nil }
	podman.IPCIsolatedFromHost = func(int) (bool, error) { return true, nil }
	return podman
}

func TestLifecycleSIGKILLHelper(t *testing.T) {
	checkpoint := os.Getenv("KENOGRAM_TEST_CRASH_CHECKPOINT")
	if checkpoint == "" {
		return
	}
	base := os.Getenv("KENOGRAM_TEST_CRASH_STATE")
	layout := worldfs.For(base, "w")
	prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
	a := crashTestApp(crashBackend(runner), base, crashProxyExecutable(t, base), func() time.Time { return time.Unix(1, 0) })
	if err := a.Up(context.Background(), prior); err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return time.Unix(2, 0) }
	a.lifecycleCheckpoint = func(name string) {
		if name == checkpoint {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
	}
	if err := a.Up(context.Background(), successor); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("checkpoint %q was not reached", checkpoint)
}

func TestLifecycleRecoversAfterSIGKILLAtCommitBoundaries(t *testing.T) {
	checkpoints := lifecycleCrashCheckpoints
	if len(checkpoints) != 15 {
		t.Fatalf("lifecycle checkpoint count = %d, want 15", len(checkpoints))
	}
	for _, checkpoint := range checkpoints {
		t.Run(checkpoint, func(t *testing.T) {
			base := t.TempDir()
			crashAt(t, base, checkpoint)

			layout := worldfs.For(base, "w")
			prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
			runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
			a := crashTestApp(crashBackend(runner), base, crashProxyExecutable(t, base), func() time.Time { return time.Unix(3, 0) })
			defer func() { _ = a.stopProxy(layout) }()
			transition, transitionErr := layout.ReadTransition()
			expectedGeneration := int64(2)
			if transitionErr == nil && transition.Phase == "rollback" {
				expectedGeneration = 1
			}
			if err := a.recoverTransition(context.Background(), layout); err != nil {
				t.Fatal(err)
			}
			state, err := layout.ReadState()
			if err != nil || state.Generation != expectedGeneration || state.Status != "running" {
				t.Fatalf("recovery-only state = %#v, %v", state, err)
			}
			if _, err := os.Stat(layout.Transition); !os.IsNotExist(err) {
				t.Fatalf("transition remains after recovery-only pass: %v", err)
			}
			if countCalls(runner.calls, "create ") != 0 {
				t.Fatalf("recovery created runtime: %v", runner.calls)
			}
			if len(runner.containers) != 1 || runner.containers[state.Container] == nil || !runner.containers[state.Container].Running || !runner.containers[state.Container].Services["worker"] {
				t.Fatalf("recovery-only runtime authority = %#v", runner.containers)
			}
			if !a.proxyAlive(layout) {
				t.Fatal("network door is not alive after recovery-only pass")
			}

			runner.calls = nil
			if err := a.Up(context.Background(), successor); err != nil {
				t.Fatal(err)
			}
			state, err = layout.ReadState()
			if err != nil || state.Generation != 2 || state.PlanDigest != successor.Result.PlanDigest || state.Status != "running" {
				t.Fatalf("final state = %#v, %v", state, err)
			}
			if len(runner.containers) != 1 || runner.containers[state.Container] == nil || !runner.containers[state.Container].Running || !runner.containers[state.Container].Services["worker"] {
				t.Fatalf("final runtime authority = %#v", runner.containers)
			}
			records, err := history.Verify(layout.History)
			if err != nil {
				t.Fatal(err)
			}
			applied := map[string]int{}
			for _, record := range records {
				if record.Action == "up" && record.Outcome == "applied" {
					applied[record.PlanDigest]++
				}
			}
			if applied[prior.Result.PlanDigest] != 1 || applied[successor.Result.PlanDigest] != 1 {
				t.Fatalf("applied history = %#v; all records = %#v", applied, records)
			}
		})
	}
}

func TestCommitRecoveryFailureNeverReversesAuthority(t *testing.T) {
	for _, failure := range []string{"exists", "inspect", "verification"} {
		t.Run(failure, func(t *testing.T) {
			base := t.TempDir()
			crashAt(t, base, "commit-recorded")
			layout := worldfs.For(base, "w")
			prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
			runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
			podman := crashBackend(runner)
			switch failure {
			case "exists":
				runner.failPrefix = "ps "
			case "inspect":
				runner.failPrefix = "inspect "
			case "verification":
				failed := false
				podman.MountIdentity = func(int, string, string) (bool, error) {
					if !failed {
						failed = true
						return false, nil
					}
					return true, nil
				}
			}
			a := crashTestApp(podman, base, crashProxyExecutable(t, base), time.Now)
			defer func() { _ = a.stopProxy(layout) }()
			if err := a.recoverTransition(context.Background(), layout); err == nil || !strings.Contains(err.Error(), "preserving commit transition") {
				t.Fatalf("first recovery error = %v", err)
			}
			transition, err := layout.ReadTransition()
			if err != nil || transition.Phase != "commit" {
				t.Fatalf("transition = %#v, %v", transition, err)
			}
			if err := a.recoverTransition(context.Background(), layout); err != nil {
				t.Fatal(err)
			}
			state, err := layout.ReadState()
			if err != nil || state.Generation != 2 {
				t.Fatalf("state = %#v, %v", state, err)
			}
		})
	}
}

func TestCommitRecoveryRestartsStoppedSuccessorAndServices(t *testing.T) {
	base := t.TempDir()
	crashAt(t, base, "commit-recorded")
	layout := worldfs.For(base, "w")
	prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
	runtime := runner.containers["kenogram-w-g2"]
	if runtime == nil {
		t.Fatal("committed successor runtime is absent")
	}
	runtime.Running = false
	runtime.Services = map[string]bool{}
	if err := runner.save(); err != nil {
		t.Fatal(err)
	}
	proxyBefore, err := os.ReadFile(layout.ProxyPID)
	if err != nil {
		t.Fatal(err)
	}
	a := crashTestApp(crashBackend(runner), base, crashProxyExecutable(t, base), time.Now)
	defer func() { _ = a.stopProxy(layout) }()
	if err := a.recoverTransition(context.Background(), layout); err != nil {
		t.Fatal(err)
	}
	if !runtime.Running || !runtime.Services["worker"] {
		t.Fatalf("recovered successor = %#v", runtime)
	}
	if !containsCall(runner.calls, "start ", "kenogram-w-g2") {
		t.Fatalf("recovery did not restart successor: %v", runner.calls)
	}
	proxyAfter, err := os.ReadFile(layout.ProxyPID)
	if err != nil || bytes.Equal(proxyBefore, proxyAfter) {
		t.Fatalf("network door was not rebound after restart: before=%q after=%q err=%v", proxyBefore, proxyAfter, err)
	}
}

func TestCommitRecoveryReplacesUnresponsiveProxy(t *testing.T) {
	base := t.TempDir()
	crashAt(t, base, "commit-recorded")
	layout := worldfs.For(base, "w")
	prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
	proxyBefore, err := os.ReadFile(layout.ProxyPID)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{
		Backend: crashBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now,
		Executable: crashProxyExecutable(t, base),
		proxyReady: func(layout worldfs.Layout) bool {
			identity, err := os.ReadFile(layout.ProxyPID)
			return err == nil && !bytes.Equal(identity, proxyBefore) && crashProxyReady(layout)
		},
	}
	defer func() { _ = a.stopProxy(layout) }()
	if err := a.recoverTransition(context.Background(), layout); err != nil {
		t.Fatal(err)
	}
	proxyAfter, err := os.ReadFile(layout.ProxyPID)
	if err != nil || bytes.Equal(proxyBefore, proxyAfter) {
		t.Fatalf("unresponsive proxy was not replaced: before=%q after=%q err=%v", proxyBefore, proxyAfter, err)
	}
}

func TestDestroyRemovesEveryGenerationWithoutRecoveringMissingSuccessor(t *testing.T) {
	for _, test := range []struct {
		name            string
		checkpoint      string
		phase           string
		removeSuccessor bool
	}{
		{name: "rollback/both-generations-present", checkpoint: "predecessor-stopped", phase: "rollback"},
		{name: "rollback/successor-missing", checkpoint: "predecessor-stopped", phase: "rollback", removeSuccessor: true},
		{name: "commit/both-generations-present", checkpoint: "commit-recorded", phase: "commit"},
		{name: "commit/successor-missing", checkpoint: "commit-recorded", phase: "commit", removeSuccessor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			crashAt(t, base, test.checkpoint)
			layout := worldfs.For(base, "w")
			prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
			runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
			if test.removeSuccessor {
				delete(runner.containers, "kenogram-w-g2")
				if err := runner.save(); err != nil {
					t.Fatal(err)
				}
			}
			a := crashTestApp(crashBackend(runner), base, crashProxyExecutable(t, base), func() time.Time { return time.Unix(4, 0) })
			if err := a.Destroy(context.Background(), "w"); err != nil {
				t.Fatal(err)
			}
			if len(runner.containers) != 0 {
				t.Fatalf("containers survived terminal destroy: %#v", runner.containers)
			}
			if _, err := os.Stat(layout.Root); !os.IsNotExist(err) {
				t.Fatalf("world root survived destroy: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(base, ".destroyed", "w-*", "history.jsonl"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("destroyed history = %v, %v", matches, err)
			}
			records, err := history.Verify(matches[0])
			if err != nil || len(records) == 0 {
				t.Fatalf("destroyed history records = %#v, %v", records, err)
			}
			last := records[len(records)-1]
			if last.Action != "destroy" || last.Outcome != "destroyed" || !strings.Contains(last.Detail, "unresolved "+test.phase+" transition") {
				t.Fatalf("destroy record = %#v", last)
			}
		})
	}
}

func TestRollbackRecoveryReplaysServicesAfterStartFailure(t *testing.T) {
	base := t.TempDir()
	crashAt(t, base, "predecessor-stopped")
	layout := worldfs.For(base, "w")
	prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
	runner.failPrefix = "exec --detach kenogram-w-g1"
	a := crashTestApp(crashBackend(runner), base, crashProxyExecutable(t, base), time.Now)
	defer func() { _ = a.stopProxy(layout) }()
	if err := a.recoverTransition(context.Background(), layout); err == nil {
		t.Fatal("first recovery unexpectedly succeeded")
	}
	priorRuntime := runner.containers["kenogram-w-g1"]
	if priorRuntime == nil || !priorRuntime.Running || priorRuntime.Services["worker"] {
		t.Fatalf("runtime after injected failure = %#v", runner.containers)
	}
	if err := a.recoverTransition(context.Background(), layout); err != nil {
		t.Fatal(err)
	}
	if !runner.containers["kenogram-w-g1"].Services["worker"] {
		t.Fatalf("service was not restored: %#v", runner.containers)
	}
}

func crashAt(t *testing.T, base, checkpoint string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleSIGKILLHelper$")
	command.Env = append(os.Environ(), "KENOGRAM_TEST_CRASH_CHECKPOINT="+checkpoint, "KENOGRAM_TEST_CRASH_STATE="+base)
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("crash helper timed out: %v", ctx.Err())
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("crash helper error = %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("crash helper status = %v", exit.Sys())
	}
}

func crashProxyExecutable(t *testing.T, base string) string {
	t.Helper()
	path := filepath.Join(base, "fake-proxy.sh")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	script := `#!/bin/sh
control=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--control" ]; then control=$2; shift 2; continue; fi
  shift
done
: >"$control"
trap 'exit 0' TERM INT
while :; do
  sleep 0.1
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func crashProxyReady(layout worldfs.Layout) bool {
	_, err := os.Stat(layout.ProxySocket)
	return err == nil
}

func crashProxyDiagnosticsReady(context.Context, worldfs.Layout, int64) (bool, error) {
	return true, nil
}

func crashTestApp(podman *backend.Podman, base, executable string, now func() time.Time) *App {
	return &App{
		Backend: podman, BaseDir: base, Out: &bytes.Buffer{}, Now: now,
		Executable: executable, proxyReady: crashProxyReady,
		proxyDiagnosticsReady: crashProxyDiagnosticsReady,
		sendProxyControl:      func(context.Context, string, proxy.ControlRequest) error { return nil },
	}
}

var lifecycleCrashCheckpoints = []string{
	"rollback-recorded", "predecessor-stopped", "cutover-workspace-recorded", "successor-started", "boundary-verified", "services-started", "successor-verified",
	"commit-recorded", "digest-written", "declaration-written", "recovery-plan-written", "state-written", "history-written",
	"predecessor-destroyed", "transition-cleared",
}

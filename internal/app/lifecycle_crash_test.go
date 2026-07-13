package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

type crashContainer struct {
	result     plan.Result
	generation int64
	running    bool
}

type crashRunner struct {
	results    map[string]plan.Result
	containers map[string]*crashContainer
	mount      string
}

func newCrashRunner(mount string, results ...plan.Result) *crashRunner {
	runner := &crashRunner{results: map[string]plan.Result{}, containers: map[string]*crashContainer{}, mount: mount}
	for _, result := range results {
		runner.results[result.PlanDigest] = result
	}
	return runner
}

func (r *crashRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
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
		r.containers[name] = &crashContainer{result: result, generation: generation}
		return []byte(name), nil
	case "start":
		if container := r.containers[args[len(args)-1]]; container != nil {
			container.running = true
		}
		return nil, nil
	case "stop":
		if container := r.containers[args[len(args)-1]]; container != nil {
			container.running = false
		}
		return nil, nil
	case "rm":
		delete(r.containers, args[len(args)-1])
		return nil, nil
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
	default:
		return []byte("ok"), nil
	}
}

func (r *crashRunner) inspect(name string, container *crashContainer) ([]byte, error) {
	result := container.result
	pid := 0
	if container.running {
		pid = 321
	}
	document := []map[string]any{{
		"Name": name, "BoundingCaps": []string{},
		"State": map[string]any{"Running": container.running, "Pid": pid},
		"IDMappings": map[string]any{
			"UidMap": []map[string]any{{"ContainerID": os.Getuid(), "HostID": os.Getuid(), "Size": 1}},
			"GidMap": []map[string]any{{"ContainerID": os.Getgid(), "HostID": os.Getgid(), "Size": 1}},
		},
		"Config": map[string]any{
			"User": result.Plan.World.User, "Hostname": result.Plan.World.Hostname, "WorkingDir": result.Plan.World.Workdir,
			"Labels": map[string]string{
				"io.kenogram.world": result.Plan.Name, "io.kenogram.generation": strconv.FormatInt(container.generation, 10),
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
	podman.MountIdentity = func(int, string, string) (bool, error) { return true, nil }
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
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), prior.Result, successor.Result)
	a := &App{Backend: crashBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: func() time.Time { return time.Unix(1, 0) }, Executable: "kenogram"}
	if err := a.Up(context.Background(), prior); err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return time.Unix(2, 0) }
	a.LifecycleCheckpoint = func(name string) {
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
	checkpoints := []string{
		"rollback-recorded", "predecessor-stopped", "successor-started", "boundary-verified", "services-started", "successor-verified",
		"commit-recorded", "digest-written", "declaration-written", "recovery-plan-written", "state-written", "history-written",
		"predecessor-destroyed", "transition-cleared",
	}
	for _, checkpoint := range checkpoints {
		t.Run(checkpoint, func(t *testing.T) {
			base := t.TempDir()
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

			layout := worldfs.For(base, "w")
			prior, successor := crashPrepared("prior", 3), crashPrepared("successor", 4)
			runner := newCrashRunner(layout.WorkspacePath("/workspace"), prior.Result, successor.Result)
			priorRunning := checkpoint == "rollback-recorded"
			if checkpoint != "predecessor-destroyed" && checkpoint != "transition-cleared" {
				runner.containers["kenogram-w-g1"] = &crashContainer{result: prior.Result, generation: 1, running: priorRunning}
			}
			runner.containers["kenogram-w-g2"] = &crashContainer{result: successor.Result, generation: 2, running: checkpoint != "predecessor-stopped"}
			a := &App{Backend: crashBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: func() time.Time { return time.Unix(3, 0) }, Executable: "kenogram"}
			if err := a.Up(context.Background(), successor); err != nil {
				t.Fatal(err)
			}
			state, err := layout.ReadState()
			if err != nil || state.Generation != 2 || state.PlanDigest != successor.Result.PlanDigest || state.Status != "running" {
				t.Fatalf("recovered state = %#v, %v", state, err)
			}
			if _, err := os.Stat(layout.Transition); !os.IsNotExist(err) {
				t.Fatalf("transition remains: %v", err)
			}
			if len(runner.containers) != 1 || runner.containers[state.Container] == nil || !runner.containers[state.Container].running {
				t.Fatalf("runtime authority = %#v", runner.containers)
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

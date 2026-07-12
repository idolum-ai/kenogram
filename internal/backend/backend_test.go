package backend

import (
	"context"
	"github.com/idolum-ai/kenogram/internal/plan"
	"os"
	"reflect"
	"testing"
)

type call struct {
	name string
	args []string
}
type fake struct {
	calls []call
	out   []byte
}

func (f *fake) Run(_ context.Context, n string, a ...string) ([]byte, error) {
	f.calls = append(f.calls, call{n, append([]string{}, a...)})
	return f.out, nil
}
func (f *fake) Start(_ context.Context, n string, a ...string) error {
	f.calls = append(f.calls, call{n, append([]string{}, a...)})
	return nil
}
func (f *fake) Interactive(_ context.Context, n string, a ...string) error {
	f.calls = append(f.calls, call{n, append([]string{}, a...)})
	return nil
}
func TestCreateExactArgv(t *testing.T) {
	f := &fake{}
	p := New(f)
	r := plan.Result{PlanDigest: "pd", DeclarationDigest: "dd", Plan: plan.Plan{Name: "w", World: plan.World{Hostname: "h", Base: "base@sha256:x", Workdir: "/workspace", User: "agent"}, Resources: plan.Resources{CPUs: 2, MemoryBytes: 3, PIDs: 4}, NetworkAllow: []plan.NetworkAllow{{Host: "x", Port: 443}}}}
	_, err := p.Create(context.Background(), r, 7, []Mount{{Source: "/host", Target: "/workspace", Mode: "rw", NoExec: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "--name", "kenogram-w-g7", "--network", "none", "--ipc", "private", "--pid", "private", "--uts", "private", "--userns", "keep-id", "--hostname", "h", "--user", "agent", "--workdir", "/workspace", "--cpus", "2", "--memory", "3", "--pids-limit", "4", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--label", "io.kenogram.world=w", "--label", "io.kenogram.generation=7", "--label", "io.kenogram.plan-digest=pd", "--label", "io.kenogram.declaration-digest=dd", "--env", "NO_PROXY=localhost,127.0.0.1", "--env", "HTTP_PROXY=http://127.0.0.1:3128", "--env", "HTTPS_PROXY=http://127.0.0.1:3128", "--mount", "type=bind,src=/host,dst=/workspace,rw,nodev,nosuid,noexec", "base@sha256:x", "/usr/bin/tail", "-f", "/dev/null"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, want) {
		t.Fatalf("got %#v", f.calls)
	}
}
func TestVerifyEvidence(t *testing.T) {
	r := plan.Result{PlanDigest: "p", DeclarationDigest: "d", Plan: plan.Plan{Name: "w", World: plan.World{User: "agent"}, Resources: plan.Resources{CPUs: 1, MemoryBytes: 2, PIDs: 3}}}
	e := Evidence{Name: "kenogram-w-g1", Running: true, NetworkMode: "none", IPCMode: "private", PIDMode: "private", UTSMode: "private", UserNSMode: "", UIDMap: []IDMap{{ContainerID: int64(os.Getuid()), HostID: int64(os.Getuid()), Size: 1}}, GIDMap: []IDMap{{ContainerID: int64(os.Getgid()), HostID: int64(os.Getgid()), Size: 1}}, User: "agent", Hostname: "", WorkingDir: "", CapDrop: []string{"CAP_ALL"}, BoundingCaps: []string{}, SecurityOpt: []string{"no-new-privileges"}, Memory: 2, NanoCPUs: 1_000_000_000, PIDs: 3, Labels: map[string]string{"io.kenogram.world": "w", "io.kenogram.generation": "1", "io.kenogram.plan-digest": "p", "io.kenogram.declaration-digest": "d"}}
	if err := Verify(e, r, 1); err != nil {
		t.Fatal(err)
	}
	e.NetworkMode = "bridge"
	if err := Verify(e, r, 1); err == nil {
		t.Fatal("bridge accepted")
	}
	e.NetworkMode = "none"
	e.SecurityOpt = nil
	if err := Verify(e, r, 1); err == nil {
		t.Fatal("missing no-new-privileges accepted")
	}
	e.SecurityOpt = []string{"no-new-privileges"}
	e.Mounts = []EvidenceMount{{Source: "/run/podman/podman.sock", Destination: "/runtime.sock", RW: true, Mode: "nodev,nosuid"}}
	if err := Verify(e, r, 1); err == nil {
		t.Fatal("runtime socket accepted")
	}
}

func TestParseIDMap(t *testing.T) {
	got, err := parseIDMap([]byte("         0     100000       1001\n      1001       1001          1\n      1002     101002      64534\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []IDMap{{ContainerID: 0, HostID: 100000, Size: 1001}, {ContainerID: 1001, HostID: 1001, Size: 1}, {ContainerID: 1002, HostID: 101002, Size: 64534}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ID mappings = %#v, want %#v", got, want)
	}
	for _, raw := range [][]byte{nil, []byte("0 1"), []byte("0 x 1"), []byte("0 0 0")} {
		if _, err := parseIDMap(raw); err == nil {
			t.Fatalf("parseIDMap(%q) succeeded", raw)
		}
	}
}

func TestPreflightRequiresRootlessCgroupV2AndSubIDs(t *testing.T) {
	f := &fake{out: []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}}}`)}
	if err := New(f).Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.out = []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":1}],"gidmap":[{"size":1}]}}}`)
	if err := New(f).Preflight(context.Background()); err == nil {
		t.Fatal("single mapping accepted")
	}
}

func TestExistsMatchesExactContainerName(t *testing.T) {
	f := &fake{out: []byte("kenogram-a-g1\nkenogram-ab-g1\n")}
	p := New(f)
	exists, err := p.Exists(context.Background(), "kenogram-a-g1")
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	exists, err = p.Exists(context.Background(), "kenogram-missing-g1")
	if err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
}

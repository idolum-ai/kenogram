package backend

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/idolum-ai/kenogram/internal/plan"
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
	want := []string{"create", "--name", "kenogram-w-g7", "--network", "none", "--ipc", "private", "--pid", "private", "--uts", "private", "--userns", "keep-id", "--image-volume", "ignore", "--hostname", "h", "--user", "agent", "--workdir", "/workspace", "--cpus", "2", "--memory", "3", "--pids-limit", "4", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--label", "io.kenogram.world=w", "--label", "io.kenogram.generation=7", "--label", "io.kenogram.plan-digest=pd", "--label", "io.kenogram.declaration-digest=dd", "--env", "NO_PROXY=localhost,127.0.0.1", "--env", "HTTP_PROXY=http://127.0.0.1:3128", "--env", "HTTPS_PROXY=http://127.0.0.1:3128", "--mount", "type=bind,src=/host,dst=/workspace,rw,nodev,nosuid,noexec", "--entrypoint", "/usr/bin/tail", "base@sha256:x", "-f", "/dev/null"}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0].args, want) {
		t.Fatalf("got %#v", f.calls)
	}
}
func TestVerifyEvidence(t *testing.T) {
	r := plan.Result{PlanDigest: "p", DeclarationDigest: "d", Plan: plan.Plan{Name: "w", World: plan.World{User: "agent"}, Resources: plan.Resources{CPUs: 1, MemoryBytes: 2, PIDs: 3}}}
	e := Evidence{Name: "kenogram-w-g1", Running: true, NetworkMode: "none", IPCMode: "private", PIDMode: "private", UTSMode: "private", UserNSMode: "", UIDMap: []IDMap{{ContainerID: int64(os.Getuid()), HostID: int64(os.Getuid()), Size: 1}}, GIDMap: []IDMap{{ContainerID: int64(os.Getgid()), HostID: int64(os.Getgid()), Size: 1}}, User: "agent", Hostname: "", WorkingDir: "", CapDrop: []string{"CAP_ALL"}, BoundingCaps: []string{}, SecurityOpt: []string{"no-new-privileges"}, SeccompMode: 2, Memory: 2, NanoCPUs: 1_000_000_000, PIDs: 3, Labels: map[string]string{"io.kenogram.world": "w", "io.kenogram.generation": "1", "io.kenogram.plan-digest": "p", "io.kenogram.declaration-digest": "d"}}
	if err := Verify(e, r, 1, nil); err != nil {
		t.Fatal(err)
	}
	e.NetworkMode = "bridge"
	if err := Verify(e, r, 1, nil); err == nil {
		t.Fatal("bridge accepted")
	}
	e.NetworkMode = "none"
	e.SecurityOpt = nil
	if err := Verify(e, r, 1, nil); err == nil {
		t.Fatal("missing no-new-privileges accepted")
	}
	e.SecurityOpt = []string{"no-new-privileges"}
	e.Mounts = []EvidenceMount{{Source: "/run/podman/podman.sock", Destination: "/runtime.sock", RW: true, Mode: "nodev,nosuid"}}
	if err := Verify(e, r, 1, nil); err == nil {
		t.Fatal("runtime socket accepted")
	}
}

func TestInspectStoppedContainerDoesNotRequireLiveProcessEvidence(t *testing.T) {
	f := &fake{out: []byte(`[{"Name":"/kenogram-w-g1","State":{"Running":false,"Pid":0},"Mounts":[{"Source":"/state/workspace","Destination":"/workspace","RW":true}]}]`)}
	p := New(f)
	p.ReadProcStatus = func(int) ([]byte, error) {
		t.Fatal("read process status for stopped container")
		return nil, nil
	}
	p.MountIdentity = func(int, string, string) (bool, error) {
		t.Fatal("verified mount identity for stopped container")
		return false, nil
	}
	evidence, err := p.Inspect(context.Background(), "kenogram-w-g1")
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Running || evidence.PID != 0 {
		t.Fatalf("stopped evidence = running %t, PID %d", evidence.Running, evidence.PID)
	}
	if len(evidence.Mounts) != 1 || evidence.Mounts[0].IdentityVerified {
		t.Fatalf("stopped mount evidence = %#v", evidence.Mounts)
	}
}

func TestVerifyExactMountEvidence(t *testing.T) {
	r := plan.Result{PlanDigest: "p", DeclarationDigest: "d", Plan: plan.Plan{Name: "w", World: plan.World{User: "agent"}, Resources: plan.Resources{CPUs: 1, MemoryBytes: 2, PIDs: 3}}}
	expected := []Mount{{Source: "/state/workspace", Target: "/workspace", Mode: "rw", NoExec: true}}
	base := Evidence{Name: "kenogram-w-g1", Running: true, NetworkMode: "none", IPCMode: "private", PIDMode: "private", UTSMode: "private", UIDMap: []IDMap{{ContainerID: int64(os.Getuid()), HostID: int64(os.Getuid()), Size: 1}}, GIDMap: []IDMap{{ContainerID: int64(os.Getgid()), HostID: int64(os.Getgid()), Size: 1}}, User: "agent", BoundingCaps: []string{}, SecurityOpt: []string{"no-new-privileges"}, SeccompMode: 2, Memory: 2, NanoCPUs: 1_000_000_000, PIDs: 3, Labels: map[string]string{"io.kenogram.world": "w", "io.kenogram.generation": "1", "io.kenogram.plan-digest": "p", "io.kenogram.declaration-digest": "d"}, Mounts: []EvidenceMount{{Source: "/state/workspace", Destination: "/workspace", RW: true, Mode: "rw,nodev,nosuid,noexec", IdentityVerified: true}}}
	if err := Verify(base, r, 1, expected); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Evidence){
		"wrong source": func(e *Evidence) { e.Mounts[0].Source = "/host/home" },
		"wrong mode":   func(e *Evidence) { e.Mounts[0].RW = false },
		"missing option": func(e *Evidence) {
			e.Mounts[0].Mode = "rw,nodev,nosuid"
		},
		"unexpected mount": func(e *Evidence) {
			e.Mounts = append(e.Mounts, EvidenceMount{Source: "/host", Destination: "/extra", RW: true, Mode: "rw,nodev,nosuid", IdentityVerified: true})
		},
		"swapped identity": func(e *Evidence) { e.Mounts[0].IdentityVerified = false },
	} {
		t.Run(name, func(t *testing.T) {
			evidence := base
			evidence.Mounts = append([]EvidenceMount(nil), base.Mounts...)
			mutate(&evidence)
			if err := Verify(evidence, r, 1, expected); err == nil {
				t.Fatal("forged mount evidence accepted")
			}
		})
	}
}

func TestSameFileIdentityDetectsSourceSwap(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	bound := filepath.Join(dir, "bound")
	if err := os.WriteFile(source, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, bound); err != nil {
		t.Fatal(err)
	}
	if same, err := sameFileIdentity(source, bound); err != nil || !same {
		t.Fatalf("original identity = %t, %v", same, err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if same, err := sameFileIdentity(source, bound); err != nil || same {
		t.Fatalf("swapped identity = %t, %v", same, err)
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

func TestParseProcBoundingCaps(t *testing.T) {
	zero, err := parseProcBoundingCaps([]byte("Name:\ttail\nCapBnd:\t0000000000000000\n"))
	if err != nil || zero == nil || len(zero) != 0 {
		t.Fatalf("zero bounding set = %#v, %v", zero, err)
	}
	nonzero, err := parseProcBoundingCaps([]byte("CapBnd:\t0000000000000400\n"))
	if err != nil || !reflect.DeepEqual(nonzero, []string{"0000000000000400"}) {
		t.Fatalf("nonzero bounding set = %#v, %v", nonzero, err)
	}
	for _, raw := range [][]byte{nil, []byte("CapBnd:\n"), []byte("CapBnd:\tnot-hex\n")} {
		if _, err := parseProcBoundingCaps(raw); err == nil {
			t.Fatalf("parseProcBoundingCaps(%q) succeeded", raw)
		}
	}
}

func TestParseProcSeccomp(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
		ok   bool
	}{
		{raw: "Name:\ttail\nSeccomp:\t2\n", want: 2, ok: true},
		{raw: "Seccomp:\t0\n", want: 0, ok: true},
		{raw: "Seccomp:\t1\n", want: 1, ok: true},
		{raw: "Name:\ttail\n"},
		{raw: "Seccomp:\tx\n"},
		{raw: "Seccomp:\t3\n"},
	} {
		got, err := parseProcSeccomp([]byte(test.raw))
		if test.ok && (err != nil || got != test.want) {
			t.Fatalf("parseProcSeccomp(%q) = %d, %v", test.raw, got, err)
		}
		if !test.ok && err == nil {
			t.Fatalf("parseProcSeccomp(%q) succeeded", test.raw)
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

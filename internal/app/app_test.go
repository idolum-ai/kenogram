package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/decl"
	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

type runtimeRunner struct {
	calls   []string
	network string
}

type destroyFailRunner struct{ runtimeRunner }

type failOnceRunner struct {
	runtimeRunner
	prefix string
	failed bool
}

func (r *failOnceRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(args, " ")
	if !r.failed && strings.HasPrefix(call, r.prefix) {
		r.failed = true
		r.calls = append(r.calls, call)
		return nil, fmt.Errorf("injected %s failure", r.prefix)
	}
	return r.runtimeRunner.Run(ctx, name, args...)
}

func (r *destroyFailRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "rm" {
		return nil, fmt.Errorf("injected destroy failure")
	}
	return r.runtimeRunner.Run(ctx, name, args...)
}

func (r *runtimeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(args) > 0 && args[0] == "info" {
		return []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}}}`), nil
	}
	if len(args) > 0 && args[0] == "inspect" {
		network := r.network
		if network == "" {
			network = "none"
		}
		return []byte(fmt.Sprintf(`[{"Name":"kenogram-w-g1","BoundingCaps":[],"State":{"Running":true,"Pid":123},"IDMappings":{"UidMap":[{"ContainerID":%d,"HostID":%d,"Size":1}],"GidMap":[{"ContainerID":%d,"HostID":%d,"Size":1}]},"Config":{"User":"agent","Hostname":"w","WorkingDir":"/workspace","Labels":{"io.kenogram.world":"w","io.kenogram.generation":"1","io.kenogram.plan-digest":"pd","io.kenogram.declaration-digest":"dd"}},"HostConfig":{"NetworkMode":%q,"IpcMode":"private","PidMode":"private","UTSMode":"private","UsernsMode":"","CapDrop":["CAP_ALL"],"SecurityOpt":["no-new-privileges"],"Memory":2,"NanoCpus":1000000000,"PidsLimit":3},"Mounts":[{"Destination":"/workspace","RW":true,"Mode":"rw,nodev,nosuid"}]}]`, os.Getuid(), os.Getuid(), os.Getgid(), os.Getgid(), network)), nil
	}
	if len(args) > 0 && args[0] == "ps" {
		return []byte("kenogram-w-g1\n"), nil
	}
	return []byte("ok"), nil
}
func (r *runtimeRunner) Start(context.Context, string, ...string) error       { return nil }
func (r *runtimeRunner) Interactive(context.Context, string, ...string) error { return nil }

func preparedFixture() Prepared {
	return Prepared{Raw: []byte("version = 1\n"), Result: plan.Result{PlanDigest: "pd", DeclarationDigest: "dd", Plan: plan.Plan{Version: 1, Name: "w", World: plan.World{Hostname: "w", Base: "base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Workdir: "/workspace", User: "agent"}, Resources: plan.Resources{CPUs: 1, MemoryBytes: 2, PIDs: 3}, Workspace: []string{"/workspace"}}}, Declaration: decl.Declaration{Name: "w"}}
}
func TestUpRecordsAppliedOnlyAfterEvidence(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	var out bytes.Buffer
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &out, Now: func() time.Time { return time.Unix(1, 0) }, Executable: "kenogram"}
	if err := a.Up(context.Background(), preparedFixture()); err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	state, err := layout.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "running" || state.Generation != 1 {
		t.Fatalf("state=%#v", state)
	}
	records, err := history.Verify(layout.History)
	if err != nil || len(records) != 1 || records[0].Outcome != "applied" {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	if _, err := os.Stat(layout.Applied); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.Transition); !os.IsNotExist(err) {
		t.Fatalf("transition remains after commit: %v", err)
	}
	if !containsCall(runner.calls, "cp ", "/generated/. kenogram-w-g1:/") {
		t.Fatalf("generated root contents were not copied to /: %v", runner.calls)
	}
}

func TestMaterializeCreatesWorldUserServiceStatusDirectory(t *testing.T) {
	layout := worldfs.For(t.TempDir(), "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: backend.New(runner)}
	if err := a.materialize(context.Background(), layout, "kenogram-w-g1", 1, preparedFixture()); err != nil {
		t.Fatal(err)
	}
	statusDir := filepath.Join(layout.Staging, "g1", "generated", "run", "kenogram", "services")
	if info, err := os.Stat(statusDir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("service status directory = %#v, %v", info, err)
	}
}

func TestUpRecoversRollbackTransitionBeforeCreating(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteTransition(worldfs.Transition{Version: 1, Phase: "rollback", Successor: worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1"}, SuccessorDeclaration: []byte("interrupted")}); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), preparedFixture()); err != nil {
		t.Fatal(err)
	}
	rm, create := -1, -1
	for index, call := range runner.calls {
		if strings.HasPrefix(call, "rm ") && rm < 0 {
			rm = index
		}
		if strings.HasPrefix(call, "create ") && create < 0 {
			create = index
		}
	}
	if rm < 0 || create < 0 || rm >= create {
		t.Fatalf("interrupted successor was not removed before create: %v", runner.calls)
	}
}

func TestServiceScriptReportsStateAndBacksOff(t *testing.T) {
	script := serviceScript(plan.Service{Name: "worker", Command: []string{"/bin/false"}, Restart: "always"})
	for _, want := range []string{"/run/kenogram/services", "running %s", "exited %s", "sleep 1"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestRecoveryPlanDoesNotRereadCopySource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(source, []byte("materialized"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := preparedFixture().Result
	result.Plan.Copies = []plan.Copy{{Source: source, SourceDigest: "content-digest", Target: "/secret", Secret: true}}
	canonical, err := plan.Canonical(result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	result.PlanDigest = DeclarationDigest(canonical)
	rawDeclaration := []byte("retained declaration")
	result.DeclarationDigest = DeclarationDigest(rawDeclaration)
	rawPlan, err := encodeRecoveryPlan(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	state := worldfs.State{PlanDigest: result.PlanDigest, DeclarationDigest: result.DeclarationDigest}
	prepared, err := preparedFromRecovery(rawDeclaration, rawPlan, "applied.toml", state)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Result.Plan.Copies[0].SourceDigest != "content-digest" {
		t.Fatal("recovery intent changed")
	}
}
func TestUpRejectsBadRuntimeEvidence(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{network: "bridge"}
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	err := a.Up(context.Background(), preparedFixture())
	if err == nil || !strings.Contains(err.Error(), "network mode") {
		t.Fatalf("err=%v", err)
	}
	layout := worldfs.For(base, "w")
	if _, err := os.Stat(layout.Applied); !os.IsNotExist(err) {
		t.Fatalf("applied exists: %v", err)
	}
	records, verifyErr := history.Verify(layout.History)
	if verifyErr != nil || records[len(records)-1].Outcome != "failed" {
		t.Fatalf("records=%#v err=%v", records, verifyErr)
	}
	if _, err := os.Stat(filepath.Join(base, "w", "state.json")); !os.IsNotExist(err) {
		t.Fatal("state recorded")
	}
}

func TestFirstUpFaultsDoNotCreateAuthority(t *testing.T) {
	for _, prefix := range []string{"create ", "cp ", "start ", "inspect "} {
		t.Run(strings.TrimSpace(prefix), func(t *testing.T) {
			base := t.TempDir()
			runner := &failOnceRunner{prefix: prefix}
			a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
			if err := a.Up(context.Background(), preparedFixture()); err == nil {
				t.Fatal("fault was accepted")
			}
			layout := worldfs.For(base, "w")
			for _, path := range []string{layout.State, layout.Applied, layout.Transition} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("authority file remains after %s fault: %s (%v)", prefix, path, err)
				}
			}
		})
	}
}

func TestUpAdoptsVerifiedMatchingGeneration(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	prepared := preparedFixture()
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	before := countCalls(runner.calls, "create ")
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	after := countCalls(runner.calls, "create ")
	if before != 1 || after != 1 {
		t.Fatalf("create calls before=%d after=%d: %v", before, after, runner.calls)
	}
}

func TestUpRejectsCopyDriftBeforeCutover(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(source, []byte("planned"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := plan.DigestSource(source)
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedFixture()
	prepared.Result.Plan.Copies = []plan.Copy{{Source: source, SourceDigest: digest, Target: "/etc/config", Mode: "0600"}}
	if err := os.WriteFile(source, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	err = a.Up(context.Background(), prepared)
	if err == nil || !strings.Contains(err.Error(), "changed after planning") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(worldfs.For(base, "w").Applied); !os.IsNotExist(statErr) {
		t.Fatalf("applied declaration exists after drift: %v", statErr)
	}
}

func TestDestroyFailurePreservesWorldState(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteState(worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	runner := &destroyFailRunner{}
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Destroy(context.Background(), "w"); err == nil || !strings.Contains(err.Error(), "injected destroy failure") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(layout.State); err != nil {
		t.Fatalf("world state removed after failed destroy: %v", err)
	}
}
func TestDestroyRenamesWorldAndPreservesOnlyHistory(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteState(worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(layout.History, history.Record{Action: "up", Outcome: "applied"}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: backend.New(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: func() time.Time { return time.Unix(2, 0) }, Executable: "kenogram"}
	if err := a.Destroy(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.Root); !os.IsNotExist(err) {
		t.Fatalf("world root remains: %v", err)
	}
	tombstone := filepath.Join(base, ".destroyed", "w-2000000000")
	entries, err := os.ReadDir(tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "history.jsonl" {
		t.Fatalf("tombstone entries=%v", entries)
	}
}
func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func containsCall(calls []string, prefix, fragment string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) && strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	calls               []string
	network             string
	mountSource         string
	containers          string
	planDigest          string
	declDigest          string
	successorPlanDigest string
	successorDeclDigest string
	successorNanoCPUs   int64
}

type destroyFailRunner struct{ runtimeRunner }

type failOnceRunner struct {
	runtimeRunner
	prefix string
	failed bool
}

type stoppedRunner struct{ runtimeRunner }

type workspaceMutatingRunner struct {
	runtimeRunner
	workspace string
	mutated   bool
}

type workspaceStartingRunner struct {
	runtimeRunner
	workspace string
	mutated   bool
}

type cancellationRunner struct{ runtimeRunner }

func (r *cancellationRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "ps" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.runtimeRunner.Run(ctx, name, args...)
}

func (r *workspaceStartingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "ps" && !r.mutated {
		r.mutated = true
		if err := os.WriteFile(filepath.Join(r.workspace, "started-after-review"), []byte("changed"), 0o600); err != nil {
			return nil, err
		}
	}
	return r.runtimeRunner.Run(ctx, name, args...)
}

func (r *workspaceMutatingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "stop" && !r.mutated {
		r.mutated = true
		if err := os.WriteFile(filepath.Join(r.workspace, "after-review"), []byte("changed"), 0o600); err != nil {
			return nil, err
		}
	}
	return r.runtimeRunner.Run(ctx, name, args...)
}

func (r *stoppedRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	raw, err := r.runtimeRunner.Run(ctx, name, args...)
	if len(args) > 0 && args[0] == "inspect" {
		raw = bytes.Replace(raw, []byte(`"Running":true`), []byte(`"Running":false`), 1)
		raw = bytes.Replace(raw, []byte(`"Pid":123`), []byte(`"Pid":0`), 1)
	}
	return raw, err
}

type supervisedServiceRunner struct{ calls []string }

func (r *supervisedServiceRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	return []byte("supervised\n"), nil
}
func (r *supervisedServiceRunner) Start(context.Context, string, ...string) error       { return nil }
func (r *supervisedServiceRunner) Interactive(context.Context, string, ...string) error { return nil }

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
	if len(args) > 0 && args[0] == "create" {
		for _, argument := range args {
			if strings.HasPrefix(argument, "type=bind,src=") {
				for _, field := range strings.Split(argument, ",") {
					if strings.HasPrefix(field, "src=") {
						r.mountSource = strings.TrimPrefix(field, "src=")
					}
				}
			}
		}
	}
	if len(args) > 0 && args[0] == "info" {
		return []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}}}`), nil
	}
	if len(args) > 0 && args[0] == "inspect" {
		container := args[len(args)-1]
		generation := 1
		if strings.HasSuffix(container, "-g2") {
			generation = 2
		}
		network := r.network
		if network == "" {
			network = "none"
		}
		planDigest, declDigest := r.planDigest, r.declDigest
		if generation == 2 && r.successorPlanDigest != "" {
			planDigest, declDigest = r.successorPlanDigest, r.successorDeclDigest
		}
		if planDigest == "" {
			planDigest = preparedFixture().Result.PlanDigest
		}
		if declDigest == "" {
			declDigest = preparedFixture().Result.DeclarationDigest
		}
		nanoCPUs := int64(1_000_000_000)
		if generation == 2 && r.successorNanoCPUs != 0 {
			nanoCPUs = r.successorNanoCPUs
		}
		return []byte(fmt.Sprintf(`[{"Name":%q,"BoundingCaps":[],"State":{"Running":true,"Pid":123},"IDMappings":{"UidMap":[{"ContainerID":%d,"HostID":%d,"Size":1}],"GidMap":[{"ContainerID":%d,"HostID":%d,"Size":1}]},"Config":{"User":"agent","Hostname":"w","WorkingDir":"/workspace","Labels":{"io.kenogram.world":"w","io.kenogram.generation":%q,"io.kenogram.plan-digest":%q,"io.kenogram.declaration-digest":%q}},"HostConfig":{"NetworkMode":%q,"IpcMode":"private","PidMode":"private","UTSMode":"private","UsernsMode":"","CapDrop":["CAP_ALL"],"SecurityOpt":["no-new-privileges"],"Memory":2,"NanoCpus":%d,"PidsLimit":3},"Mounts":[{"Source":%q,"Destination":"/workspace","RW":true,"Mode":"rw,nodev,nosuid"}]}]`, container, os.Getuid(), os.Getuid(), os.Getgid(), os.Getgid(), strconv.Itoa(generation), planDigest, declDigest, network, nanoCPUs, r.mountSource)), nil
	}
	if len(args) > 0 && args[0] == "ps" {
		if r.containers != "" {
			return []byte(r.containers), nil
		}
		return []byte("kenogram-w-g1\n"), nil
	}
	return []byte("ok"), nil
}
func (r *runtimeRunner) Start(context.Context, string, ...string) error { return nil }
func (r *runtimeRunner) Interactive(_ context.Context, _ string, args ...string) error {
	r.calls = append(r.calls, "interactive "+strings.Join(args, " "))
	return nil
}

func testBackend(r backend.Runner) *backend.Podman {
	podman := backend.New(r)
	podman.ReadProcStatus = func(int) ([]byte, error) { return []byte("Seccomp:\t2\n"), nil }
	podman.ReadProcessStart = func(int) string { return "test-process-start" }
	podman.MountIdentity = func(int, string, string) (bool, error) { return true, nil }
	podman.IPCIsolatedFromHost = func(int) (bool, error) { return true, nil }
	return podman
}

func preparedFixture() Prepared {
	prepared := Prepared{Raw: []byte("version = 1\n"), Result: plan.Result{Plan: plan.Plan{Version: 1, Name: "w", World: plan.World{Hostname: "w", Base: "base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Workdir: "/workspace", User: "agent"}, Resources: plan.Resources{CPUs: 1, MemoryBytes: 2, PIDs: 3}, Workspace: []string{"/workspace"}}}, Declaration: decl.Declaration{Name: "w"}}
	canonical, err := plan.Canonical(prepared.Result.Plan)
	if err != nil {
		panic(err)
	}
	planSum := sha256.Sum256(canonical)
	prepared.Result.PlanDigest = hex.EncodeToString(planSum[:])
	prepared.Result.DeclarationDigest = DeclarationDigest(prepared.Raw)
	return prepared
}

func refreshPreparedPlanDigest(prepared *Prepared) {
	canonical, err := plan.Canonical(prepared.Result.Plan)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	prepared.Result.PlanDigest = hex.EncodeToString(sum[:])
}
func TestUpRecordsAppliedOnlyAfterEvidence(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	var out bytes.Buffer
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &out, Now: func() time.Time { return time.Unix(1, 0) }, Executable: "kenogram"}
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

func TestUpReviewedRejectsEvidenceChangedAfterReview(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	prepared := preparedFixture()
	comparison, err := a.CompareUp(prepared)
	if err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.Workspace, "appeared-after-review"), []byte("unreviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = a.UpReviewed(context.Background(), prepared, comparison)
	if err == nil || !strings.Contains(err.Error(), "revalidate reviewed comparison") {
		t.Fatalf("error = %v", err)
	}
	if containsCall(runner.calls, "create ", "") {
		t.Fatalf("runtime mutated after stale review: %v", runner.calls)
	}
}

func TestUpReviewedAcceptsItsOwnEmptyWorldDirectoryCreation(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	prepared := preparedFixture()
	comparison, err := a.CompareUp(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.UpReviewed(context.Background(), prepared, comparison); err != nil {
		t.Fatal(err)
	}
}

func TestUpReviewedRejectsDifferentSuccessorThanReviewed(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	reviewed := preparedFixture()
	comparison, err := a.CompareUp(reviewed)
	if err != nil {
		t.Fatal(err)
	}
	different := reviewed
	different.Result.Plan.Resources.CPUs++
	canonical, err := plan.Canonical(different.Result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	different.Result.PlanDigest = hex.EncodeToString(sum[:])
	err = a.UpReviewed(context.Background(), different, comparison)
	if err == nil || !strings.Contains(err.Error(), "reviewed predecessor evidence changed") {
		t.Fatalf("error = %v", err)
	}
	if containsCall(runner.calls, "create ", "") {
		t.Fatalf("runtime mutated after successor changed: %v", runner.calls)
	}
}

func TestUpReviewedRejectsCandidateContentChangedWithoutDigest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Prepared)
		want   string
	}{
		{
			name: "plan",
			mutate: func(prepared *Prepared) {
				prepared.Result.Plan.Resources.CPUs++
			},
			want: "plan digest does not match prepared plan",
		},
		{
			name: "declaration",
			mutate: func(prepared *Prepared) {
				prepared.Raw = append(append([]byte(nil), prepared.Raw...), []byte("# changed after review\n")...)
			},
			want: "declaration digest does not match prepared bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			runner := &runtimeRunner{}
			a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
			prepared := preparedFixture()
			comparison, err := a.CompareUp(prepared)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&prepared)
			err = a.UpReviewed(context.Background(), prepared, comparison)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if containsCall(runner.calls, "create ", "") {
				t.Fatalf("runtime mutated after candidate changed: %v", runner.calls)
			}
		})
	}
}

func TestUpReviewedRejectsTamperedComparisonPresentation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*UpComparison)
	}{
		{
			name: "changes",
			mutate: func(comparison *UpComparison) {
				comparison.Changes = append(comparison.Changes, plan.Change{Path: "resources.cpus", Before: "1", After: "2"})
			},
		},
		{
			name: "workspace",
			mutate: func(comparison *UpComparison) {
				comparison.Workspace = "workspace: caller replacement"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			runner := &runtimeRunner{}
			a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
			prepared := preparedFixture()
			comparison, err := a.CompareUp(prepared)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&comparison)
			err = a.UpReviewed(context.Background(), prepared, comparison)
			if err == nil || !strings.Contains(err.Error(), "reviewed predecessor evidence changed") {
				t.Fatalf("error = %v", err)
			}
			if containsCall(runner.calls, "create ", "") {
				t.Fatalf("runtime mutated after comparison changed: %v", runner.calls)
			}
		})
	}
}

func TestCompareUpAllowsFailureOnlyHistoryForRetry(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(layout.History, history.Record{Action: "up", Outcome: "failed", Detail: "create: injected"}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	a := &App{BaseDir: base}
	comparison, err := a.CompareUp(preparedFixture())
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Workspace != "workspace: new (no carried state)" {
		t.Fatalf("workspace = %q", comparison.Workspace)
	}
}

func TestCompareUpRejectsCorruptAuthoritativeEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, worldfs.Layout)
		want   string
	}{
		{
			name: "missing history",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				if err := os.Remove(layout.History); err != nil {
					t.Fatal(err)
				}
			},
			want: "verify predecessor history",
		},
		{
			name: "truncated history",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				if err := os.WriteFile(layout.History, []byte("{\"truncated\":"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "truncated final record",
		},
		{
			name: "empty history",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				if err := os.WriteFile(layout.History, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "authoritative history is empty",
		},
		{
			name: "hash-corrupt history",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				records, err := history.Verify(layout.History)
				if err != nil || len(records) == 0 {
					t.Fatalf("history before corruption = %#v, %v", records, err)
				}
				records[0].Hash = strings.Repeat("0", sha256.Size*2)
				raw, err := json.Marshal(records[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(layout.History, append(raw, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "hash mismatch",
		},
		{
			name: "invalid workspace digest",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				if err := os.WriteFile(filepath.Join(layout.Digests, "g1.json"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "validate digest tree",
		},
		{
			name: "wrong state name",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				mutateState(t, layout, func(state *worldfs.State) { state.Name = "different-world" })
			},
			want: "does not match world",
		},
		{
			name: "non-positive state generation",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				mutateState(t, layout, func(state *worldfs.State) { state.Generation = 0 })
			},
			want: "is not positive",
		},
		{
			name: "non-canonical state container",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				mutateState(t, layout, func(state *worldfs.State) { state.Container = "" })
			},
			want: "does not match \"kenogram-w-g1\"",
		},
		{
			name: "invalid state status",
			mutate: func(t *testing.T, layout worldfs.Layout) {
				mutateState(t, layout, func(state *worldfs.State) { state.Status = "nonsense" })
			},
			want: "is not valid here",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			prepared := preparedFixture()
			runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
			a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
			if err := a.Up(context.Background(), prepared); err != nil {
				t.Fatal(err)
			}
			layout := worldfs.For(base, "w")
			test.mutate(t, layout)
			if _, err := a.CompareUp(prepared); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompareUpRejectsLiveAuthorityWithMissingWorkspaceMount(t *testing.T) {
	base := t.TempDir()
	prepared := preparedFixture()
	runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	if err := os.RemoveAll(layout.Workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompareUp(prepared); err == nil || !strings.Contains(err.Error(), "running container does not match recorded authority") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompareUpContextCancelsRuntimeObservation(t *testing.T) {
	base := t.TempDir()
	prepared := preparedFixture()
	runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	a.Backend = testBackend(&cancellationRunner{runtimeRunner: *runner})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.CompareUpContext(ctx, prepared); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func mutateState(t *testing.T, layout worldfs.Layout, mutate func(*worldfs.State)) {
	t.Helper()
	state, err := layout.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	mutate(&state)
	if err := layout.WriteState(state); err != nil {
		t.Fatal(err)
	}
}

func TestCompareUpRejectsOrphanedStaging(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(layout.Staging, "g1")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "partial"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{BaseDir: base}
	if _, err := a.CompareUp(preparedFixture()); err == nil || !strings.Contains(err.Error(), "staged generation artifacts exist") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompareUpVerifiesOptionalFirstGenerationTransitionHistory(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteTransition(worldfs.Transition{
		Version:   1,
		Phase:     "rollback",
		Successor: worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", Status: "staging"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.History, []byte("{\"truncated\":"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{BaseDir: base}
	if _, err := a.CompareUp(preparedFixture()); err == nil || !strings.Contains(err.Error(), "truncated final record") {
		t.Fatalf("corrupt optional history error = %v", err)
	}
	if err := os.Remove(layout.History); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompareUp(preparedFixture()); err != nil {
		t.Fatalf("missing first-generation transition history = %v", err)
	}
}

func TestValidateComparisonTransitionRejectsInconsistentStates(t *testing.T) {
	valid := worldfs.Transition{
		Version:         1,
		Phase:           "rollback",
		Prior:           &worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", Status: "running"},
		PriorWasRunning: true,
		Successor:       worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2", Status: "staging"},
	}
	tests := []struct {
		name   string
		mutate func(*worldfs.Transition)
		want   string
	}{
		{name: "wrong successor name", mutate: func(transition *worldfs.Transition) { transition.Successor.Name = "other" }, want: "does not match world"},
		{name: "wrong successor container", mutate: func(transition *worldfs.Transition) { transition.Successor.Container = "other" }, want: "does not match"},
		{name: "wrong successor status", mutate: func(transition *worldfs.Transition) { transition.Successor.Status = "running" }, want: "is not valid here"},
		{name: "nonsequential generation", mutate: func(transition *worldfs.Transition) {
			transition.Successor.Generation = 3
			transition.Successor.Container = "kenogram-w-g3"
		}, want: "does not follow"},
		{name: "running down prior", mutate: func(transition *worldfs.Transition) { transition.Prior.Status = "down" }, want: "recorded running"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transition := valid
			prior := *valid.Prior
			transition.Prior = &prior
			test.mutate(&transition)
			if err := validateComparisonTransition(transition, "w"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestUpReviewedRecomparesAuthorityAfterRollbackRecovery(t *testing.T) {
	base := t.TempDir()
	raw := []byte(`version = 1
name = "w"
[world]
hostname = "w"
base = "base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "agent"
[resources]
cpus = 1
memory_bytes = 2
pids = 3
[workspace]
paths = ["/workspace"]
`)
	prepared, err := PrepareBytes(raw, filepath.Join(base, "world.toml"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	state, err := layout.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	recoveryPlan, err := encodeRecoveryPlan(prepared.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteTransition(worldfs.Transition{
		Version:          1,
		Phase:            "rollback",
		Prior:            &state,
		PriorDeclaration: prepared.Raw,
		PriorPlan:        recoveryPlan,
		Successor:        worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2", Status: "staging"},
	}); err != nil {
		t.Fatal(err)
	}
	comparison, err := a.CompareUp(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !comparison.recoveryPending {
		t.Fatal("comparison did not record pending recovery")
	}
	tampered := comparison
	tampered.recoveryPending = false
	if err := a.UpReviewed(context.Background(), prepared, tampered); err == nil || !strings.Contains(err.Error(), "reviewed predecessor evidence changed") {
		t.Fatalf("tampered comparison error = %v", err)
	}
	if _, err := os.Stat(layout.Transition); err != nil {
		t.Fatalf("tampered comparison recovered transition: %v", err)
	}
	if err := a.UpReviewed(context.Background(), prepared, comparison); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.Transition); !os.IsNotExist(err) {
		t.Fatalf("transition remains after reviewed recovery: %v", err)
	}
}

func TestUpReviewedCarriesWorkspaceAdvancedByActivePredecessor(t *testing.T) {
	base := t.TempDir()
	prepared := preparedFixture()
	runner := &workspaceMutatingRunner{}
	runner.planDigest = prepared.Result.PlanDigest
	runner.declDigest = prepared.Result.DeclarationDigest
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	runner.workspace = layout.Workspace
	successor := prepared
	successor.Result.Plan.Resources.CPUs++
	refreshPreparedPlanDigest(&successor)
	runner.successorPlanDigest = successor.Result.PlanDigest
	runner.successorDeclDigest = successor.Result.DeclarationDigest
	runner.successorNanoCPUs = successor.Result.Plan.Resources.CPUs * 1_000_000_000
	comparison, err := a.CompareUp(successor)
	if err != nil {
		t.Fatal(err)
	}
	cutoverRecorded := false
	a.lifecycleCheckpoint = func(name string) {
		if name != "cutover-workspace-recorded" {
			return
		}
		transition, err := layout.ReadTransition()
		if err != nil {
			t.Fatal(err)
		}
		cutover, err := worldfs.Digest(layout.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		if transition.Workspace.Root != cutover.Root {
			t.Fatalf("recorded cutover root = %q, want %q", transition.Workspace.Root, cutover.Root)
		}
		cutoverRecorded = true
	}
	if err := a.UpReviewed(context.Background(), successor, comparison); err != nil {
		t.Fatal(err)
	}
	if !cutoverRecorded {
		t.Fatal("cutover workspace was not durably recorded")
	}
	if !containsCall(runner.calls, "start ", "kenogram-w-g2") {
		t.Fatalf("successor did not start with authoritative workspace: %v", runner.calls)
	}
	if got, err := os.ReadFile(filepath.Join(layout.Workspace, "after-review")); err != nil || string(got) != "changed" {
		t.Fatalf("carried workspace = %q, %v", got, err)
	}
}

func TestUpReviewedRejectsInactiveWorkspaceChangedAtCutover(t *testing.T) {
	base := t.TempDir()
	prepared := preparedFixture()
	runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := a.Down(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	stopped := &stoppedRunner{runtimeRunner: *runner}
	a.Backend = testBackend(stopped)
	layout := worldfs.For(base, "w")
	successor := prepared
	successor.Result.Plan.Resources.CPUs++
	refreshPreparedPlanDigest(&successor)
	runner.successorPlanDigest = successor.Result.PlanDigest
	runner.successorDeclDigest = successor.Result.DeclarationDigest
	runner.successorNanoCPUs = successor.Result.Plan.Resources.CPUs * 1_000_000_000
	comparison, err := a.CompareUp(successor)
	if err != nil {
		t.Fatal(err)
	}
	a.lifecycleCheckpoint = func(name string) {
		if name == "rollback-recorded" {
			if err := os.WriteFile(filepath.Join(layout.Workspace, "after-review"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	err = a.UpReviewed(context.Background(), successor, comparison)
	if err == nil || !strings.Contains(err.Error(), "reviewed workspace changed before successor start") {
		t.Fatalf("error = %v", err)
	}
	if containsCall(stopped.calls, "start ", "kenogram-w-g2") {
		t.Fatalf("successor started with unreviewed inactive workspace: %v", stopped.calls)
	}
}

func TestUpReviewedRejectsQuiescentPredecessorBecomingActive(t *testing.T) {
	base := t.TempDir()
	prepared := preparedFixture()
	runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := a.Down(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	stopped := &stoppedRunner{runtimeRunner: *runner}
	a.Backend = testBackend(stopped)
	successor := prepared
	successor.Result.Plan.Resources.CPUs++
	refreshPreparedPlanDigest(&successor)
	comparison, err := a.CompareUp(successor)
	if err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	started := &workspaceStartingRunner{runtimeRunner: *runner, workspace: layout.Workspace}
	started.successorPlanDigest = successor.Result.PlanDigest
	started.successorDeclDigest = successor.Result.DeclarationDigest
	started.successorNanoCPUs = successor.Result.Plan.Resources.CPUs * 1_000_000_000
	a.Backend = testBackend(started)
	err = a.UpReviewed(context.Background(), successor, comparison)
	if err == nil || !strings.Contains(err.Error(), "reviewed quiescent predecessor became active") {
		t.Fatalf("error = %v", err)
	}
	if containsCall(started.calls, "create ", "kenogram-w-g2") || containsCall(started.calls, "start ", "kenogram-w-g2") {
		t.Fatalf("successor mutated after classification changed: %v", started.calls)
	}
}

func TestUpReviewedRejectsNewWorldWorkspaceCreatedAtCutover(t *testing.T) {
	base := t.TempDir()
	prepared := preparedFixture()
	runner := &runtimeRunner{planDigest: prepared.Result.PlanDigest, declDigest: prepared.Result.DeclarationDigest}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	comparison, err := a.CompareUp(prepared)
	if err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	a.lifecycleCheckpoint = func(name string) {
		if name != "rollback-recorded" {
			return
		}
		if err := os.WriteFile(filepath.Join(layout.Workspace, "after-review"), []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = a.UpReviewed(context.Background(), prepared, comparison)
	if err == nil || !strings.Contains(err.Error(), "reviewed workspace changed before successor start") {
		t.Fatalf("error = %v", err)
	}
	if containsCall(runner.calls, "start ", "kenogram-w-g1") {
		t.Fatalf("successor started with unreviewed workspace: %v", runner.calls)
	}
	if _, err := os.Stat(layout.Transition); !os.IsNotExist(err) {
		t.Fatalf("rollback transition remains: %v", err)
	}
}

func TestMaterializeCreatesWorldUserServiceStatusDirectory(t *testing.T) {
	layout := worldfs.For(t.TempDir(), "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner)}
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
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
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

func TestStatusAndRepairEntryFollowTransitionAuthority(t *testing.T) {
	for _, test := range []struct {
		phase               string
		authoritative       int64
		candidate           int64
		authoritativeTarget string
	}{
		{phase: "rollback", authoritative: 1, candidate: 2, authoritativeTarget: "kenogram-w-g1"},
		{phase: "commit", authoritative: 2, candidate: 1, authoritativeTarget: "kenogram-w-g2"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			base := t.TempDir()
			layout := worldfs.For(base, "w")
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			prior := worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", Status: "running"}
			successor := worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2", Status: "staging"}
			if err := layout.WriteState(prior); err != nil {
				t.Fatal(err)
			}
			if err := layout.WriteTransition(worldfs.Transition{Version: 1, Phase: test.phase, Prior: &prior, Successor: successor}); err != nil {
				t.Fatal(err)
			}
			runner := &runtimeRunner{containers: "kenogram-w-g1\nkenogram-w-g2\n"}
			a := &App{Backend: testBackend(runner), BaseDir: base}
			status, err := a.Status(context.Background(), "w")
			if err != nil {
				t.Fatal(err)
			}
			if status.RecoveryPhase != test.phase || status.Authoritative == nil || status.Authoritative.State.Generation != test.authoritative || status.Candidate == nil || status.Candidate.State.Generation != test.candidate {
				t.Fatalf("status = %#v", status)
			}
			if status.Authoritative.Evidence == nil || status.Authoritative.Evidence.Name != test.authoritativeTarget {
				t.Fatalf("authoritative evidence = %#v", status.Authoritative.Evidence)
			}
			if err := a.Enter(context.Background(), "w", true); err != nil {
				t.Fatal(err)
			}
			if !containsCall(runner.calls, "interactive ", test.authoritativeTarget+" /bin/sh") {
				t.Fatalf("repair entry did not use authority %s: %v", test.authoritativeTarget, runner.calls)
			}
		})
	}
}

func TestRollbackWithoutPredecessorReportsCandidateButCannotEnter(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteTransition(worldfs.Transition{Version: 1, Phase: "rollback", Successor: worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1"}}); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner), BaseDir: base}
	status, err := a.Status(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	if status.Authoritative != nil || status.Candidate == nil || status.Candidate.State.Generation != 1 {
		t.Fatalf("status = %#v", status)
	}
	if err := a.Enter(context.Background(), "w", true); err == nil || !strings.Contains(err.Error(), "no authoritative generation") {
		t.Fatalf("enter error = %v", err)
	}
}

func TestServiceScriptReportsStateAndBacksOff(t *testing.T) {
	script := serviceScript(plan.Service{Name: "worker", Command: []string{"/bin/false"}, Restart: "always"})
	for _, want := range []string{"/run/kenogram/services", ".supervisor", "trap", "running %s", "exited %s", "sleep 1"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestStartServicesAdoptsExistingSupervisor(t *testing.T) {
	runner := &supervisedServiceRunner{}
	a := &App{Backend: backend.New(runner)}
	services := []plan.Service{{Name: "worker", Autostart: true, Command: []string{"worker"}}}
	if err := a.startServices(context.Background(), "kenogram-w-g1", services); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || strings.Contains(runner.calls[0], "--detach") {
		t.Fatalf("calls = %v", runner.calls)
	}
}

func TestStartProxyDoesNotAcceptStaleSocketAsReady(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.ProxySocket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(base, "no-proxy.sh")
	script := "#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 0.1; done\n"
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &App{BaseDir: base, Executable: executable}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := a.startProxy(ctx, layout, 123, nil); err == nil {
		t.Fatal("proxy without a control server was accepted as ready")
	}
	if _, err := os.Stat(layout.ProxySocket); !os.IsNotExist(err) {
		t.Fatalf("stale proxy socket remains: %v", err)
	}
}

func TestStoppedStatusStillObservesRetainedContainer(t *testing.T) {
	base := t.TempDir()
	l := worldfs.For(base, "w")
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	state := worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", Status: "stopped"}
	if err := l.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.History, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &stoppedRunner{runtimeRunner: runtimeRunner{containers: "kenogram-w-g1\n"}}
	status, err := (&App{Backend: testBackend(runner), BaseDir: base}).Status(context.Background(), "w")
	if err != nil {
		t.Fatal(err)
	}
	observation := status.Authoritative
	if observation == nil || !observation.Exists || observation.Evidence == nil || observation.Evidence.Running {
		t.Fatalf("status = %#v", status)
	}
}

func TestMutationsFailClosedDuringTransition(t *testing.T) {
	base := t.TempDir()
	l := worldfs.For(base, "w")
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteTransition(worldfs.Transition{Version: 1, Phase: "commit", Successor: worldfs.State{Name: "w", Generation: 2}}); err != nil {
		t.Fatal(err)
	}
	a := &App{BaseDir: base, Now: time.Now}
	for name, operation := range map[string]func() error{
		"allow":          func() error { return a.Allow("w", "example.com:443", "1m") },
		"revoke":         func() error { return a.Revoke("w", "example.com:443") },
		"repair-history": func() error { return a.RepairHistory("w") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil || !strings.Contains(err.Error(), "unresolved commit transition") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWorldsUsesTransitionAuthority(t *testing.T) {
	for _, test := range []struct {
		phase      string
		generation int64
	}{
		{phase: "rollback", generation: 1},
		{phase: "commit", generation: 2},
	} {
		t.Run(test.phase, func(t *testing.T) {
			base := t.TempDir()
			layout := worldfs.For(base, "w")
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			prior := worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1"}
			successor := worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2"}
			if err := layout.WriteTransition(worldfs.Transition{Version: 1, Phase: test.phase, Prior: &prior, Successor: successor}); err != nil {
				t.Fatal(err)
			}
			worlds, err := (&App{BaseDir: base}).Worlds()
			if err != nil || len(worlds) != 1 || worlds[0].Generation != test.generation || worlds[0].Status != "recovery-required:"+test.phase {
				t.Fatalf("worlds = %#v, %v", worlds, err)
			}
		})
	}
}

func TestInsideDocumentKeepsOptionalTransportsOutOfWorldOntology(t *testing.T) {
	document := insideDocument()
	if strings.Contains(strings.ToLower(document), "engram") {
		t.Fatalf("generated world guidance names an optional transport:\n%s", document)
	}
	for _, required := range []string{"request as prose", "operator", "host-side declaration"} {
		if !strings.Contains(document, required) {
			t.Fatalf("generated world guidance is missing %q:\n%s", required, document)
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

func TestRecoveryAcceptsLegacyAppliedPlanWithoutInterfacesField(t *testing.T) {
	result := preparedFixture().Result
	canonical, err := plan.Canonical(result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(canonical, []byte(`"interfaces"`)) {
		t.Fatalf("empty interfaces changed legacy canonical plan: %s", canonical)
	}
	result.PlanDigest = DeclarationDigest(canonical)
	declaration := []byte("legacy declaration")
	result.DeclarationDigest = DeclarationDigest(declaration)
	rawPlan, err := encodeRecoveryPlan(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rawPlan, []byte(`"interfaces"`)) {
		t.Fatalf("legacy recovery fixture unexpectedly contains interfaces: %s", rawPlan)
	}
	state := worldfs.State{PlanDigest: result.PlanDigest, DeclarationDigest: result.DeclarationDigest}
	if _, err := preparedFromRecovery(declaration, rawPlan, "applied.toml", state); err != nil {
		t.Fatalf("legacy applied plan rejected after upgrade: %v", err)
	}
}

func TestConnectUsesOnlyDeclaredInterfaceOnVerifiedAuthoritativeGeneration(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.EnsureWorkspace("/workspace"); err != nil {
		t.Fatal(err)
	}
	prepared := preparedFixture()
	prepared.Result.Plan.Interfaces = []plan.Interface{{Name: "ssh", Address: "127.0.0.1:2222"}}
	canonical, err := plan.Canonical(prepared.Result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Raw = []byte("retained declaration")
	prepared.Result.PlanDigest = DeclarationDigest(canonical)
	prepared.Result.DeclarationDigest = DeclarationDigest(prepared.Raw)
	rawPlan, err := encodeRecoveryPlan(prepared.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteApplied(prepared.Raw); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteAppliedPlan(rawPlan); err != nil {
		t.Fatal(err)
	}
	state := worldfs.State{Name: "w", Generation: 1, Container: "kenogram-w-g1", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, Status: "running"}
	if err := layout.WriteState(state); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{mountSource: layout.WorkspacePath("/workspace"), planDigest: state.PlanDigest, declDigest: state.DeclarationDigest}
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	called := false
	podman := testBackend(runner)
	a := &App{Backend: podman, BaseDir: base, acquireConnection: func(_ context.Context, pid int, processStart, address string, revalidate func() error) (net.Conn, error) {
		called = true
		if pid != 123 || processStart != "test-process-start" || address != "127.0.0.1:2222" {
			t.Fatalf("pid=%d processStart=%q address=%q", pid, processStart, address)
		}
		if err := revalidate(); err != nil {
			return nil, err
		}
		return client, nil
	}}
	connection, err := a.Connect(context.Background(), "w", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if !called {
		t.Fatal("namespace connection was not acquired")
	}
	successor := prepared.Result
	successor.Plan.Interfaces[0].Address = "127.0.0.1:3333"
	successorCanonical, err := plan.Canonical(successor.Plan)
	if err != nil {
		t.Fatal(err)
	}
	successor.PlanDigest = DeclarationDigest(successorCanonical)
	successorRaw, err := encodeRecoveryPlan(successor)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteTransition(worldfs.Transition{Version: 1, Phase: "rollback", Prior: &state, PriorDeclaration: prepared.Raw, PriorPlan: rawPlan, Successor: worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2", PlanDigest: successor.PlanDigest, DeclarationDigest: successor.DeclarationDigest}, SuccessorDeclaration: prepared.Raw, SuccessorPlan: successorRaw}); err != nil {
		t.Fatal(err)
	}
	connection, err = a.Connect(context.Background(), "w", "ssh")
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	if _, err := a.Connect(context.Background(), "w", "undeclared"); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared interface: %v", err)
	}
	identityReads := 0
	podman.ReadProcessStart = func(int) string {
		identityReads++
		if identityReads <= 2 {
			return "test-process-start"
		}
		return "replacement-process-start"
	}
	if _, err := a.Connect(context.Background(), "w", "ssh"); err == nil || !strings.Contains(err.Error(), "identity changed after namespace pin") {
		t.Fatalf("changed runtime identity: %v", err)
	}
}

func TestConnectMissingWorldHasAnOperatorFacingError(t *testing.T) {
	a := &App{BaseDir: t.TempDir()}
	if _, err := a.Connect(context.Background(), "absent", "ssh"); err == nil || err.Error() != `world "absent" does not exist` {
		t.Fatalf("missing world: %v", err)
	}
}
func TestUpRejectsBadRuntimeEvidence(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{network: "bridge"}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	prepared := preparedFixture()
	prepared.Result.Plan.Services = []plan.Service{{Name: "canary", Command: []string{"/bin/true"}, Autostart: true, Restart: "never"}}
	refreshPreparedPlanDigest(&prepared)
	runner.planDigest = prepared.Result.PlanDigest
	runner.declDigest = prepared.Result.DeclarationDigest
	err := a.Up(context.Background(), prepared)
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
	if countCalls(runner.calls, "exec ") != 0 {
		t.Fatalf("service executed before bad runtime evidence was rejected: %v", runner.calls)
	}
}

func TestUpClearsStagedSecretAfterCopyFailure(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	canary := "KENOGRAM_FAILED_STAGE_SECRET_7f04681c"
	if err := os.WriteFile(secret, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := plan.DigestSource(secret)
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedFixture()
	prepared.Result.Plan.Copies = []plan.Copy{{Source: secret, SourceDigest: digest, Target: "/run/secret", Mode: "0600", Secret: true}}
	runner := &failOnceRunner{prefix: "cp "}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
	if err := a.Up(context.Background(), prepared); err == nil {
		t.Fatal("copy failure was accepted")
	}
	if err := filepath.Walk(base, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(raw), canary) {
				t.Fatalf("failed staging retained secret at %s", path)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpRejectsMountsOverlappingControlAndState(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	runtimeRoot := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "podman"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	for name, source := range map[string]string{
		"runtime socket parent": runtimeRoot,
		"state root parent":     root,
	} {
		t.Run(name, func(t *testing.T) {
			prepared := preparedFixture()
			prepared.Result.Plan.Mounts = []plan.Mount{{Source: source, Target: "/host", Mode: "ro"}}
			refreshPreparedPlanDigest(&prepared)
			runner := &runtimeRunner{}
			a := &App{Backend: testBackend(runner), BaseDir: stateRoot, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
			err := a.Up(context.Background(), prepared)
			if err == nil || !strings.Contains(err.Error(), "protected host path") {
				t.Fatalf("err=%v", err)
			}
			if countCalls(runner.calls, "create ") != 0 || countCalls(runner.calls, "exec ") != 0 {
				t.Fatalf("runtime mutation preceded mount rejection: %v", runner.calls)
			}
		})
	}
}

func TestReplacementRejectsSourceBeneathPredecessorWritableMount(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	prior := preparedFixture().Result
	prior.Plan.Mounts = []plan.Mount{{Source: root, Target: "/host", Mode: "rw"}}
	next := preparedFixture().Result
	next.Plan.Mounts = []plan.Mount{{Source: child, Target: "/project", Mode: "ro"}}
	if err := validateReplacementMountSources(prior, next); err == nil || !strings.Contains(err.Error(), "predecessor-writable") {
		t.Fatalf("mutable descendant source = %v", err)
	}
	next.Plan.Mounts[0].Source = root
	if err := validateReplacementMountSources(prior, next); err != nil {
		t.Fatalf("stable mount root rejected: %v", err)
	}
}

func TestFirstUpFaultsDoNotCreateAuthority(t *testing.T) {
	for _, prefix := range []string{"create ", "cp ", "start ", "inspect "} {
		t.Run(strings.TrimSpace(prefix), func(t *testing.T) {
			base := t.TempDir()
			runner := &failOnceRunner{prefix: prefix}
			a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
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
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
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

func TestAdoptionRecordsUnavailableDigestForChangingWorkspace(t *testing.T) {
	digest, detail, err := adoptionWorkspaceEvidence("workspace", func(string) (worldfs.DigestTree, error) {
		return worldfs.DigestTree{}, fmt.Errorf("digest retries exhausted: %w", worldfs.ErrWorkspaceChanging)
	})
	if err != nil || digest != "" || !strings.Contains(detail, "live workspace was changing") {
		t.Fatalf("digest=%q detail=%q err=%v", digest, detail, err)
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
	refreshPreparedPlanDigest(&prepared)
	if err := os.WriteFile(source, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
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
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: time.Now, Executable: "kenogram"}
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
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: func() time.Time { return time.Unix(2, 0) }, Executable: "kenogram"}
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

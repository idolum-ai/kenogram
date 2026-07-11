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
		return []byte(fmt.Sprintf(`[{"Name":"kenogram-w-g1","State":{"Running":true,"Pid":123},"Config":{"Labels":{"io.kenogram.world":"w","io.kenogram.generation":"1","io.kenogram.plan-digest":"pd","io.kenogram.declaration-digest":"dd"}},"HostConfig":{"NetworkMode":%q,"Memory":2,"NanoCpus":1000000000,"PidsLimit":3},"Mounts":[{"Destination":"/workspace"}]}]`, network)), nil
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
func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/lockfile"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

func TestStableInspectionDigestRequiresConsecutiveCompleteObservations(t *testing.T) {
	workspace := t.TempDir()
	first := filepath.Join(workspace, "first")
	second := filepath.Join(workspace, "second")
	writeInspectionFile(t, first, "alpha", 0o600)
	writeInspectionFile(t, second, "bravo", 0o600)
	calls := 0
	a := &App{digestWorkspaceContext: func(ctx context.Context, root string) (worldfs.DigestTree, error) {
		calls++
		switch calls {
		case 2:
			writeInspectionFile(t, first, "bravo", 0o600)
			writeInspectionFile(t, second, "alpha", 0o600)
		}
		return worldfs.DigestContext(ctx, root)
	}}
	got, err := a.stableInspectionDigest(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	want, err := worldfs.Digest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || got.Root != want.Root {
		t.Fatalf("stable digest calls=%d root=%s want=%s", calls, got.Root, want.Root)
	}
}

func TestStableInspectionDigestEnforcesLiveObservationLimits(t *testing.T) {
	workspace := t.TempDir()
	writeInspectionFile(t, filepath.Join(workspace, "first"), "first", 0o600)
	writeInspectionFile(t, filepath.Join(workspace, "second"), "second", 0o600)
	prior := inspectionDigestLimits
	inspectionDigestLimits = worldfs.DigestLimits{MaxEntries: 2, MaxMetadataBytes: 1024, MaxFileBytes: 1024}
	t.Cleanup(func() { inspectionDigestLimits = prior })
	a := &App{}
	if _, err := a.stableInspectionDigest(context.Background(), workspace); err == nil || !strings.Contains(err.Error(), "2 entries") {
		t.Fatalf("observation limit error = %v", err)
	}
}

func TestInspectWorkspaceCancellationReleasesSharedLock(t *testing.T) {
	a, layout, prepared := inspectionFixture(t, []string{"/workspace"})
	recordInspectionGeneration(t, layout, prepared, 1)
	started := make(chan struct{})
	a.digestWorkspaceContext = func(ctx context.Context, _ string) (worldfs.DigestTree, error) {
		close(started)
		<-ctx.Done()
		return worldfs.DigestTree{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := a.InspectWorkspaceContext(ctx, "w", 1)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("cancellation error = %v", err)
	}
	lock, err := lockfile.Acquire(layout.Lock)
	if err != nil {
		t.Fatalf("shared observation lock remained held: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectWorkspaceAcceptsDeduplicatedAppliedGeneration(t *testing.T) {
	base := t.TempDir()
	runner := &runtimeRunner{}
	a := &App{Backend: testBackend(runner), BaseDir: base, Out: &bytes.Buffer{}, Now: func() time.Time { return time.Unix(1, 0) }, Executable: "kenogram"}
	prepared := preparedFixture()
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	layout := worldfs.For(base, "w")
	baseline, err := layout.ReadDigest(1)
	if err != nil {
		t.Fatal(err)
	}

	// The runtime disappeared outside Kenogram. Reapplying the unchanged
	// declaration creates g2, but AppendOnce legitimately keeps the identical
	// adjacent applied record as recovery-safe history deduplication.
	runner.containers = "\n"
	a.Now = func() time.Time { return time.Unix(2, 0) }
	if err := a.Up(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	records, err := history.Verify(layout.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].WorkspaceDigest != baseline.Root {
		t.Fatalf("deduplicated history = %#v", records)
	}
	state, err := layout.ReadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 2 {
		t.Fatalf("generation = %d, want 2", state.Generation)
	}
	got, err := a.InspectWorkspace("w", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaselineRoot != baseline.Root || got.CurrentRoot != baseline.Root || len(got.Loci) != 1 || len(got.Loci[0].Changes) != 0 {
		t.Fatalf("inspection = %#v", got)
	}
}

func TestInspectWorkspaceReportsDeterministicMetadataByDeclaredLocus(t *testing.T) {
	a, layout, prepared := inspectionFixture(t, []string{"/workspace", "/data"})
	data := layout.WorkspacePath("/data")
	workspace := layout.WorkspacePath("/workspace")
	writeInspectionFile(t, filepath.Join(data, "removed"), "remove-canary", 0o600)
	writeInspectionFile(t, filepath.Join(data, "modified"), "before-canary", 0o600)
	writeInspectionFile(t, filepath.Join(data, "mode"), "same", 0o600)
	writeInspectionFile(t, filepath.Join(data, "type"), "file", 0o600)
	if err := os.Symlink("baseline-link-canary", filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	baseline := recordInspectionGeneration(t, layout, prepared, 1)

	if err := os.Remove(filepath.Join(data, "removed")); err != nil {
		t.Fatal(err)
	}
	writeInspectionFile(t, filepath.Join(data, "modified"), "after-canary", 0o600)
	if err := os.Chmod(filepath.Join(data, "mode"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, "type")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(data, "type"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("current-link-canary", filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	writeInspectionFile(t, filepath.Join(workspace, "added"), "add-canary", 0o600)

	got, err := a.InspectWorkspace("w", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaselineRoot != baseline.Root || got.CurrentRoot == baseline.Root || len(got.Loci) != 2 {
		t.Fatalf("inspection roots/loci = %#v", got)
	}
	if got.Loci[0].Locus != "/data" || got.Loci[1].Locus != "/workspace" {
		t.Fatalf("locus order = %#v", got.Loci)
	}
	wantKinds := []string{"modified", "type-or-mode-changed", "modified", "removed", "type-or-mode-changed"}
	if len(got.Loci[0].Changes) != len(wantKinds) {
		t.Fatalf("data changes = %#v", got.Loci[0].Changes)
	}
	for i, want := range wantKinds {
		if got.Loci[0].Changes[i].Change != want {
			t.Fatalf("change %d = %q, want %q", i, got.Loci[0].Changes[i].Change, want)
		}
	}
	if change := got.Loci[1].Changes; len(change) != 1 || change[0].Path != "added" || change[0].Change != "added" {
		t.Fatalf("workspace changes = %#v", change)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"remove-canary", "before-canary", "after-canary", "add-canary", "baseline-link-canary", "current-link-canary"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("file content %q leaked in %s", canary, raw)
		}
	}
}

func TestInspectWorkspaceFailsClosedOnEvidence(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, a *App, layout worldfs.Layout, prepared Prepared)
		want string
	}{
		{name: "missing baseline", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			if err := os.Remove(filepath.Join(layout.Digests, "g1.json")); err != nil {
				t.Fatal(err)
			}
		}, want: "read baseline g1"},
		{name: "corrupt baseline", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(layout.Digests, "g1.json"), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "validate digest tree"},
		{name: "changing workspace", edit: func(t *testing.T, a *App, _ worldfs.Layout, _ Prepared) {
			t.Helper()
			a.digestWorkspace = func(string) (worldfs.DigestTree, error) {
				return worldfs.DigestTree{}, fmt.Errorf("unstable: %w", worldfs.ErrWorkspaceChanging)
			}
		}, want: "workspace is changing"},
		{name: "noncanonical current workspace", edit: func(t *testing.T, a *App, _ worldfs.Layout, _ Prepared) {
			t.Helper()
			a.digestWorkspace = func(string) (worldfs.DigestTree, error) {
				return worldfs.DigestTree{Root: strings.Repeat("0", 64)}, nil
			}
		}, want: "validate current workspace"},
		{name: "corrupt history", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			if err := os.WriteFile(layout.History, []byte("{\"hash\":\"invalid\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "verify history"},
		{name: "baseline history mismatch", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			if err := os.Remove(layout.History); err != nil {
				t.Fatal(err)
			}
			state, err := layout.ReadState()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := history.Append(layout.History, history.Record{Action: "up", Outcome: "applied", PlanDigest: state.PlanDigest, DeclarationDigest: state.DeclarationDigest, WorkspaceDigest: strings.Repeat("f", 64)}, time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
		}, want: "unexplained generation gap"},
		{name: "missing committed generation", edit: func(t *testing.T, _ *App, layout worldfs.Layout, prepared Prepared) {
			t.Helper()
			baseline, err := layout.ReadDigest(1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := layout.WriteDigest(3, baseline); err != nil {
				t.Fatal(err)
			}
			state := worldfs.State{Name: "w", Generation: 3, Container: "kenogram-w-g3", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, Status: "down"}
			if err := layout.WriteState(state); err != nil {
				t.Fatal(err)
			}
		}, want: "read committed g2 digest"},
		{name: "absurd authoritative generation", edit: func(t *testing.T, _ *App, layout worldfs.Layout, prepared Prepared) {
			t.Helper()
			const generation = int64(9223372036854775807)
			state := worldfs.State{Name: "w", Generation: generation, Container: "kenogram-w-g9223372036854775807", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, Status: "down"}
			if err := layout.WriteState(state); err != nil {
				t.Fatal(err)
			}
		}, want: "read committed g2 digest"},
		{name: "unresolved transition", edit: func(t *testing.T, _ *App, layout worldfs.Layout, prepared Prepared) {
			t.Helper()
			rawPlan, err := encodeRecoveryPlan(prepared.Result)
			if err != nil {
				t.Fatal(err)
			}
			state, err := layout.ReadState()
			if err != nil {
				t.Fatal(err)
			}
			if err := layout.WriteTransition(worldfs.Transition{Version: 1, Phase: "commit", Successor: state, SuccessorDeclaration: prepared.Raw, SuccessorPlan: rawPlan}); err != nil {
				t.Fatal(err)
			}
		}, want: "unresolved transition"},
		{name: "corrupt transition", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			if err := os.WriteFile(layout.Transition, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "read transition evidence"},
		{name: "dangling transition symlink", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			if err := os.Symlink("missing-transition-target", layout.Transition); err != nil {
				t.Fatal(err)
			}
		}, want: "transition artifact exists"},
		{name: "unattributed entry", edit: func(t *testing.T, _ *App, layout worldfs.Layout, _ Prepared) {
			t.Helper()
			writeInspectionFile(t, filepath.Join(layout.Workspace, "outside"), "secret", 0o600)
		}, want: "outside every declared locus"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, layout, prepared := inspectionFixture(t, []string{"/workspace"})
			recordInspectionGeneration(t, layout, prepared, 1)
			test.edit(t, a, layout, prepared)
			if _, err := a.InspectWorkspace("w", 1); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInspectWorkspaceRequiresSamePlanForHistoricalLocusAttribution(t *testing.T) {
	a, layout, prepared := inspectionFixture(t, []string{"/workspace"})
	baseline := recordInspectionGeneration(t, layout, prepared, 1)
	second := prepared
	second.Result.Plan.Workspace = []string{"/data"}
	refreshPreparedPlanDigest(&second)
	rawPlan, err := encodeRecoveryPlan(second.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteAppliedPlan(rawPlan); err != nil {
		t.Fatal(err)
	}
	state := worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2", PlanDigest: second.Result.PlanDigest, DeclarationDigest: second.Result.DeclarationDigest, Status: "down"}
	if err := layout.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := layout.WriteDigest(2, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(layout.History, history.Record{Action: "up", Outcome: "applied", PlanDigest: state.PlanDigest, DeclarationDigest: state.DeclarationDigest, WorkspaceDigest: baseline.Root}, time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InspectWorkspace("w", 1); err == nil || !strings.Contains(err.Error(), "different plan") {
		t.Fatalf("err = %v", err)
	}
}

func TestInspectWorkspaceRejectsAmbiguousDeduplicatedBaselinePlan(t *testing.T) {
	a, layout, prior := inspectionFixture(t, []string{"/workspace"})
	tree := recordInspectionGeneration(t, layout, prior, 1)
	successor := prior
	successor.Result.Plan.Resources.PIDs++
	refreshPreparedPlanDigest(&successor)
	rawPlan, err := encodeRecoveryPlan(successor.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteAppliedPlan(rawPlan); err != nil {
		t.Fatal(err)
	}
	for generation := int64(2); generation <= 3; generation++ {
		if _, err := layout.WriteDigest(generation, tree); err != nil {
			t.Fatal(err)
		}
	}
	state := worldfs.State{Name: "w", Generation: 3, Container: "kenogram-w-g3", PlanDigest: successor.Result.PlanDigest, DeclarationDigest: successor.Result.DeclarationDigest, Status: "down"}
	if err := layout.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(layout.History, history.Record{Action: "up", Outcome: "applied", PlanDigest: state.PlanDigest, DeclarationDigest: state.DeclarationDigest, WorkspaceDigest: tree.Root}, time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InspectWorkspace("w", 2); err == nil || !strings.Contains(err.Error(), "may use a different plan") {
		t.Fatalf("err = %v", err)
	}
}

func TestBindInspectionGenerationsMatrix(t *testing.T) {
	records := func(roots ...string) []history.Record {
		result := make([]history.Record, len(roots))
		for index, root := range roots {
			result[index] = history.Record{WorkspaceDigest: root}
		}
		return result
	}
	tests := []struct {
		name     string
		roots    []string
		records  []history.Record
		baseline int
		want     []int
		wantErr  string
	}{
		{name: "one record covers repeated run", roots: []string{"a", "a", "a"}, records: records("a"), baseline: 1, want: []int{0}},
		{name: "left edge is unambiguous", roots: []string{"a", "a", "a"}, records: records("a", "a"), baseline: 0, want: []int{0}},
		{name: "middle exposes every candidate", roots: []string{"a", "a", "a"}, records: records("a", "a"), baseline: 1, want: []int{0, 1}},
		{name: "right edge is unambiguous", roots: []string{"a", "a", "a"}, records: records("a", "a"), baseline: 2, want: []int{1}},
		{name: "nonconsecutive roots stay ordered", roots: []string{"a", "b", "a"}, records: records("a", "b", "a"), baseline: 2, want: []int{2}},
		{name: "unexplained gap", roots: []string{"a", "b", "c"}, records: records("a", "c"), baseline: 0, wantErr: "unexplained generation gap"},
		{name: "excess records", roots: []string{"a"}, records: records("a", "a"), baseline: 0, wantErr: "applied records"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := bindInspectionGenerations(test.roots, test.records, test.baseline)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("candidates = %v, want %v", got, test.want)
			}
		})
	}
}

func inspectionFixture(t *testing.T, loci []string) (*App, worldfs.Layout, Prepared) {
	t.Helper()
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.Lock, []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, locus := range loci {
		if _, err := layout.EnsureWorkspace(locus); err != nil {
			t.Fatal(err)
		}
	}
	prepared := preparedFixture()
	prepared.Result.Plan.Workspace = append([]string{}, loci...)
	refreshPreparedPlanDigest(&prepared)
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
	return &App{BaseDir: base}, layout, prepared
}

func recordInspectionGeneration(t *testing.T, layout worldfs.Layout, prepared Prepared, generation int64) worldfs.DigestTree {
	t.Helper()
	tree, err := worldfs.Digest(layout.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.WriteDigest(generation, tree); err != nil {
		t.Fatal(err)
	}
	state := worldfs.State{Name: "w", Generation: generation, Container: fmt.Sprintf("kenogram-w-g%d", generation), PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, Status: "down"}
	if err := layout.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if _, err := history.Append(layout.History, history.Record{Action: "up", Outcome: "applied", PlanDigest: state.PlanDigest, DeclarationDigest: state.DeclarationDigest, WorkspaceDigest: tree.Root}, time.Unix(generation, 0)); err != nil {
		t.Fatal(err)
	}
	return tree
}

func writeInspectionFile(t *testing.T, name, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

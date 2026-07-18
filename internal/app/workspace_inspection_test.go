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
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

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

package app

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/lockfile"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

// WorkspaceEntry is metadata-only evidence about one carried path. Regular
// file bytes are represented only by their digest and are never returned.
type WorkspaceEntry struct {
	Type   string `json:"type"`
	Mode   string `json:"mode"`
	Size   *int64 `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type WorkspaceChange struct {
	Path   string          `json:"path"`
	Change string          `json:"change"`
	Before *WorkspaceEntry `json:"before,omitempty"`
	After  *WorkspaceEntry `json:"after,omitempty"`
}

type WorkspaceLocusInspection struct {
	Locus   string            `json:"locus"`
	Changes []WorkspaceChange `json:"changes"`
}

type WorkspaceInspection struct {
	SchemaVersion      int                        `json:"schema_version"`
	World              string                     `json:"world"`
	BaselineGeneration int64                      `json:"baseline_generation"`
	BaselineRoot       string                     `json:"baseline_root"`
	CurrentRoot        string                     `json:"current_root"`
	Loci               []WorkspaceLocusInspection `json:"loci"`
}

// InspectWorkspace compares one explicitly selected committed generation with
// the current carried tree. It is read-only, but shares the world mutation lock
// so its authority files cannot change during inspection. World processes are
// outside that lock; Digest therefore fails if it cannot obtain a stable view.
func (a *App) InspectWorkspace(world string, baselineGeneration int64) (WorkspaceInspection, error) {
	if err := worldfs.ValidateName(world); err != nil {
		return WorkspaceInspection{}, err
	}
	if baselineGeneration < 1 {
		return WorkspaceInspection{}, fmt.Errorf("baseline generation must be positive")
	}
	l := worldfs.For(a.BaseDir, world)
	lock, err := lockfile.AcquireShared(l.Lock)
	if err != nil {
		return WorkspaceInspection{}, err
	}
	defer lock.Release()

	if _, err := l.ReadTransition(); err == nil {
		return WorkspaceInspection{}, fmt.Errorf("world has an unresolved transition; recover it before inspection")
	} else if !os.IsNotExist(err) {
		return WorkspaceInspection{}, fmt.Errorf("read transition evidence: %w", err)
	} else if _, artifactErr := os.Lstat(l.Transition); artifactErr == nil {
		return WorkspaceInspection{}, fmt.Errorf("transition artifact exists but cannot be read")
	} else if !os.IsNotExist(artifactErr) {
		return WorkspaceInspection{}, fmt.Errorf("inspect transition artifact: %w", artifactErr)
	}
	state, err := l.ReadState()
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("read authoritative state: %w", err)
	}
	if err := validateComparisonState(state, world, "running", "down"); err != nil {
		return WorkspaceInspection{}, fmt.Errorf("validate authoritative state: %w", err)
	}
	if baselineGeneration > state.Generation {
		return WorkspaceInspection{}, fmt.Errorf("baseline g%d is newer than authoritative g%d", baselineGeneration, state.Generation)
	}
	prepared, err := a.loadPredecessor(l, state)
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("load authoritative declaration: %w", err)
	}
	baseline, err := l.ReadDigest(baselineGeneration)
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("read baseline g%d: %w", baselineGeneration, err)
	}
	if err := verifyInspectionHistory(l, state, baselineGeneration, baseline.Root); err != nil {
		return WorkspaceInspection{}, err
	}
	current, err := a.digest(l.Workspace)
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("digest current workspace: %w", err)
	}
	loci, err := compareWorkspaceLoci(l, prepared.Result.Plan.Workspace, baseline, current)
	if err != nil {
		return WorkspaceInspection{}, err
	}
	return WorkspaceInspection{
		SchemaVersion: 1, World: world, BaselineGeneration: baselineGeneration,
		BaselineRoot: baseline.Root, CurrentRoot: current.Root, Loci: loci,
	}, nil
}

func verifyInspectionHistory(l worldfs.Layout, state worldfs.State, baselineGeneration int64, baselineRoot string) error {
	records, err := history.Verify(l.History)
	if err != nil {
		return fmt.Errorf("verify history: %w", err)
	}
	applied := make([]history.Record, 0)
	for _, record := range records {
		if record.Action == "up" && record.Outcome == "applied" {
			applied = append(applied, record)
		}
	}
	if int64(len(applied)) != state.Generation {
		return fmt.Errorf("history has %d applied generations but state names g%d", len(applied), state.Generation)
	}
	currentRecord := applied[state.Generation-1]
	if currentRecord.PlanDigest != state.PlanDigest || currentRecord.DeclarationDigest != state.DeclarationDigest {
		return fmt.Errorf("authoritative state does not match applied history for g%d", state.Generation)
	}
	baselineRecord := applied[baselineGeneration-1]
	if baselineRecord.WorkspaceDigest != baselineRoot {
		return fmt.Errorf("baseline g%d digest is not bound to its applied history record", baselineGeneration)
	}
	if baselineRecord.PlanDigest != state.PlanDigest {
		return fmt.Errorf("baseline g%d used a different plan; declared-locus attribution is unavailable", baselineGeneration)
	}
	return nil
}

func compareWorkspaceLoci(l worldfs.Layout, declared []string, before, after worldfs.DigestTree) ([]WorkspaceLocusInspection, error) {
	if err := worldfs.ValidateDigestTree(before); err != nil {
		return nil, fmt.Errorf("validate baseline workspace: %w", err)
	}
	if err := worldfs.ValidateDigestTree(after); err != nil {
		return nil, fmt.Errorf("validate current workspace: %w", err)
	}
	if before.Entries[0] != after.Entries[0] {
		return nil, fmt.Errorf("workspace root metadata changed outside a declared locus")
	}
	type locus struct{ target, storage string }
	known := make(map[string]string, len(declared))
	locusList := make([]locus, 0, len(declared))
	for _, target := range declared {
		storage := filepath.Base(l.WorkspacePath(target))
		if prior, exists := known[storage]; exists {
			return nil, fmt.Errorf("declared loci %q and %q have the same storage identity", prior, target)
		}
		known[storage] = target
		locusList = append(locusList, locus{target: target, storage: storage})
	}
	sort.Slice(locusList, func(i, j int) bool { return locusList[i].target < locusList[j].target })

	byLocus := make(map[string]map[string][2]*worldfs.DigestEntry, len(declared))
	for _, item := range locusList {
		byLocus[item.target] = make(map[string][2]*worldfs.DigestEntry)
	}
	add := func(side int, entry worldfs.DigestEntry) error {
		if entry.Path == "" {
			return nil
		}
		top, rest, _ := strings.Cut(entry.Path, "/")
		target, ok := known[top]
		if !ok {
			return fmt.Errorf("workspace entry %q is outside every declared locus", entry.Path)
		}
		rel := "."
		if rest != "" {
			rel = rest
		}
		pair := byLocus[target][rel]
		copy := entry
		pair[side] = &copy
		byLocus[target][rel] = pair
		return nil
	}
	for _, entry := range before.Entries {
		if err := add(0, entry); err != nil {
			return nil, fmt.Errorf("attribute baseline: %w", err)
		}
	}
	for _, entry := range after.Entries {
		if err := add(1, entry); err != nil {
			return nil, fmt.Errorf("attribute current workspace: %w", err)
		}
	}

	result := make([]WorkspaceLocusInspection, 0, len(locusList))
	for _, item := range locusList {
		entries := byLocus[item.target]
		paths := make([]string, 0, len(entries))
		for rel := range entries {
			paths = append(paths, rel)
		}
		sort.Strings(paths)
		changes := make([]WorkspaceChange, 0)
		for _, rel := range paths {
			pair := entries[rel]
			if pair[0] != nil && pair[1] != nil && *pair[0] == *pair[1] {
				continue
			}
			change := WorkspaceChange{Path: path.Clean(rel)}
			switch {
			case pair[0] == nil:
				change.Change = "added"
			case pair[1] == nil:
				change.Change = "removed"
			case pair[0].Type != pair[1].Type || pair[0].Mode != pair[1].Mode:
				change.Change = "type-or-mode-changed"
			default:
				change.Change = "modified"
			}
			change.Before = inspectionEntry(pair[0])
			change.After = inspectionEntry(pair[1])
			changes = append(changes, change)
		}
		result = append(result, WorkspaceLocusInspection{Locus: item.target, Changes: changes})
	}
	return result, nil
}

func inspectionEntry(entry *worldfs.DigestEntry) *WorkspaceEntry {
	if entry == nil {
		return nil
	}
	result := &WorkspaceEntry{Type: entry.Type, Mode: fmt.Sprintf("%04o", entry.Mode), SHA256: entry.SHA256}
	if entry.Type == "file" {
		size := entry.Size
		result.Size = &size
	}
	return result
}

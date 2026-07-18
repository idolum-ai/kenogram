package app

import (
	"context"
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
	return a.InspectWorkspaceContext(context.Background(), world, baselineGeneration)
}

func (a *App) InspectWorkspaceContext(ctx context.Context, world string, baselineGeneration int64) (WorkspaceInspection, error) {
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
	if err := ctx.Err(); err != nil {
		return WorkspaceInspection{}, fmt.Errorf("inspect workspace: %w", err)
	}

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
	baseline, err := l.ReadDigestContext(ctx, baselineGeneration)
	if err != nil {
		return WorkspaceInspection{}, fmt.Errorf("read baseline g%d: %w", baselineGeneration, err)
	}
	if err := verifyInspectionHistory(ctx, l, state, baselineGeneration, baseline.Root); err != nil {
		return WorkspaceInspection{}, err
	}
	current, err := a.stableInspectionDigest(ctx, l.Workspace)
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

const stableInspectionAttempts = 8

func (a *App) stableInspectionDigest(ctx context.Context, workspace string) (worldfs.DigestTree, error) {
	previous, err := a.digestContext(ctx, workspace)
	if err != nil {
		return worldfs.DigestTree{}, err
	}
	for attempt := 1; attempt < stableInspectionAttempts; attempt++ {
		current, digestErr := a.digestContext(ctx, workspace)
		if digestErr != nil {
			return worldfs.DigestTree{}, digestErr
		}
		if current.Root == previous.Root {
			return current, nil
		}
		previous = current
	}
	return worldfs.DigestTree{}, fmt.Errorf("workspace did not produce two consecutive complete observations after %d attempts: %w", stableInspectionAttempts, worldfs.ErrWorkspaceChanging)
}

func verifyInspectionHistory(ctx context.Context, l worldfs.Layout, state worldfs.State, baselineGeneration int64, baselineRoot string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("verify history: %w", err)
	}
	records, err := history.VerifyContext(ctx, l.History)
	if err != nil {
		return fmt.Errorf("verify history: %w", err)
	}
	applied := make([]history.Record, 0)
	for _, record := range records {
		if record.Action == "up" && record.Outcome == "applied" {
			applied = append(applied, record)
		}
	}
	if len(applied) == 0 {
		return fmt.Errorf("history contains no applied generation evidence")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("verify history: %w", err)
	}
	roots := []string{}
	for generation := int64(1); ; {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("bind committed generations: %w", err)
		}
		tree, readErr := l.ReadDigestContext(ctx, generation)
		if readErr != nil {
			return fmt.Errorf("read committed g%d digest: %w", generation, readErr)
		}
		roots = append(roots, tree.Root)
		if generation == state.Generation {
			break
		}
		generation++
	}
	if baselineGeneration > int64(len(roots)) || roots[int(baselineGeneration-1)] != baselineRoot {
		return fmt.Errorf("baseline g%d changed while binding history", baselineGeneration)
	}
	baselineRecords, err := bindInspectionGenerations(roots, applied, int(baselineGeneration-1))
	if err != nil {
		return fmt.Errorf("bind committed generations to applied history: %w", err)
	}
	currentRecord := applied[len(applied)-1]
	if currentRecord.PlanDigest != state.PlanDigest || currentRecord.DeclarationDigest != state.DeclarationDigest {
		return fmt.Errorf("authoritative state does not match applied history for g%d", state.Generation)
	}
	for _, recordIndex := range baselineRecords {
		if applied[recordIndex].PlanDigest != state.PlanDigest {
			return fmt.Errorf("baseline g%d may use a different plan; declared-locus attribution is unavailable", baselineGeneration)
		}
	}
	return nil
}

// bindInspectionGenerations treats applied history as a run-length encoding of
// committed generations. AppendOnce may omit an immediately repeated semantic
// record, but every physical applied record still represents at least one
// generation and records remain ordered. Consecutive records with the same
// workspace root can make a baseline's exact record ambiguous; callers must
// therefore validate every returned candidate before attributing a locus.
func bindInspectionGenerations(roots []string, applied []history.Record, baseline int) ([]int, error) {
	if len(roots) == 0 || baseline < 0 || baseline >= len(roots) {
		return nil, fmt.Errorf("invalid generation range")
	}
	if len(applied) > len(roots) {
		return nil, fmt.Errorf("history contains %d applied records for %d committed generations", len(applied), len(roots))
	}
	baselineRecords := []int(nil)
	for generation, record := 0, 0; generation < len(roots) || record < len(applied); {
		if generation == len(roots) || record == len(applied) || roots[generation] != applied[record].WorkspaceDigest {
			return nil, fmt.Errorf("unexplained generation gap at g%d or applied record %d", generation+1, record+1)
		}
		generationEnd := generation + 1
		for generationEnd < len(roots) && roots[generationEnd] == roots[generation] {
			generationEnd++
		}
		recordEnd := record + 1
		for recordEnd < len(applied) && applied[recordEnd].WorkspaceDigest == applied[record].WorkspaceDigest {
			recordEnd++
		}
		generationCount := generationEnd - generation
		recordCount := recordEnd - record
		if recordCount > generationCount {
			return nil, fmt.Errorf("%d applied records compete for %d generations beginning at g%d", recordCount, generationCount, generation+1)
		}
		if baseline >= generation && baseline < generationEnd {
			position := baseline - generation
			minimum := max(0, recordCount-(generationCount-position))
			maximum := min(recordCount-1, position)
			baselineRecords = make([]int, 0, maximum-minimum+1)
			for candidate := minimum; candidate <= maximum; candidate++ {
				baselineRecords = append(baselineRecords, record+candidate)
			}
		}
		generation, record = generationEnd, recordEnd
	}
	if len(baselineRecords) == 0 {
		return nil, fmt.Errorf("baseline generation has no applied history binding")
	}
	return baselineRecords, nil
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

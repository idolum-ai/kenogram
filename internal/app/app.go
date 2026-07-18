// Package app orchestrates world lifecycle without interpreting inhabitant input.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/decl"
	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/lockfile"
	"github.com/idolum-ai/kenogram/internal/naming"
	"github.com/idolum-ai/kenogram/internal/netns"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/proxy"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

type App struct {
	Backend    *backend.Podman
	BaseDir    string
	Out        io.Writer
	Now        func() time.Time
	Executable string
	// acquireConnection is replaceable only by package tests; production uses
	// the namespace descriptor-transfer boundary.
	acquireConnection func(context.Context, int, string, string, func() error) (net.Conn, error)
	// lifecycleCheckpoint is nil in production. Tests use it to terminate a
	// process at named durable boundaries without changing runtime behavior.
	lifecycleCheckpoint func(string)
	// proxyReady is nil in production. Lifecycle crash tests replace the real
	// control round trip because their proxy process is a persistence fixture.
	proxyReady func(worldfs.Layout) bool
	// digestWorkspace is nil in production. Tests use it to prove that a live
	// predecessor is not required to produce a stable tree before cutover.
	digestWorkspace func(string) (worldfs.DigestTree, error)
	// digestWorkspaceContext is replaceable only by package tests that need to
	// coordinate cancellation or complete-observation stability.
	digestWorkspaceContext func(context.Context, string) (worldfs.DigestTree, error)
}

func New() (*App, error) {
	base, err := worldfs.BaseDir()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &App{Backend: backend.New(nil), BaseDir: base, Out: os.Stdout, Now: func() time.Time { return time.Now().UTC() }, Executable: executable, acquireConnection: netns.AcquireConnection}, nil
}

func (a *App) checkpoint(name string) {
	if a.lifecycleCheckpoint != nil {
		a.lifecycleCheckpoint(name)
	}
}

func (a *App) digest(path string) (worldfs.DigestTree, error) {
	if a.digestWorkspace != nil {
		return a.digestWorkspace(path)
	}
	return worldfs.Digest(path)
}

func (a *App) digestContext(ctx context.Context, path string) (worldfs.DigestTree, error) {
	if err := ctx.Err(); err != nil {
		return worldfs.DigestTree{}, err
	}
	if a.digestWorkspaceContext != nil {
		return a.digestWorkspaceContext(ctx, path)
	}
	if a.digestWorkspace != nil {
		tree, err := a.digestWorkspace(path)
		if err != nil {
			return worldfs.DigestTree{}, err
		}
		if err := ctx.Err(); err != nil {
			return worldfs.DigestTree{}, err
		}
		return tree, nil
	}
	return worldfs.DigestContext(ctx, path)
}

func (a *App) proxyIsReady(ctx context.Context, l worldfs.Layout) bool {
	if a.proxyReady != nil {
		return a.proxyReady(l)
	}
	if !a.proxyAlive(l) {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	return proxy.SendControlContext(probeCtx, l.ProxySocket, proxy.ControlRequest{Operation: "ping"}) == nil
}

type Prepared struct {
	Raw         []byte
	Declaration decl.Declaration
	Result      plan.Result
	Path        string
}

// UpComparison is the complete predecessor evidence rendered before an up.
// The snapshot is opaque to callers; UpReviewed revalidates it under the world
// mutation lock so the authority reviewed by an operator cannot change between
// review and application.
type UpComparison struct {
	Changes         []plan.Change
	Workspace       string
	snapshot        string
	recoveryPending bool
	workspaceRoot   string
	workspaceMode   string
}

const (
	workspaceModeEmpty  = "empty"
	workspaceModeExact  = "exact"
	workspaceModeActive = "active"
)

// GenerationObservation keeps recorded authority distinct from runtime
// observation. A missing runtime is evidence too, so Exists is explicit.
type GenerationObservation struct {
	State    worldfs.State     `json:"state"`
	Exists   bool              `json:"runtime_exists"`
	Evidence *backend.Evidence `json:"runtime_evidence,omitempty"`
}

// StatusResult reports transition authority without hiding the generation that
// is being committed or rolled back.
type StatusResult struct {
	Authoritative *GenerationObservation `json:"authoritative,omitempty"`
	Candidate     *GenerationObservation `json:"candidate,omitempty"`
	RecoveryPhase string                 `json:"recovery_phase,omitempty"`
}

func Prepare(path string) (Prepared, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Prepared{}, fmt.Errorf("read declaration: %w", err)
	}
	return PrepareBytes(raw, path)
}
func PrepareBytes(raw []byte, path string) (Prepared, error) {
	d, err := decl.Parse(raw)
	if err != nil {
		return Prepared{}, fmt.Errorf("parse declaration: %w", err)
	}
	result, err := plan.Build(d, path, raw)
	if err != nil {
		return Prepared{}, fmt.Errorf("validate declaration: %w", err)
	}
	return Prepared{raw, d, result, path}, nil
}

func (a *App) Up(ctx context.Context, prepared Prepared) (retErr error) {
	return a.up(ctx, prepared, nil)
}

// UpReviewed applies a prepared declaration only while the predecessor
// evidence still matches a comparison acquired with CompareUp.
func (a *App) UpReviewed(ctx context.Context, prepared Prepared, comparison UpComparison) error {
	if comparison.snapshot == "" {
		return fmt.Errorf("reviewed comparison snapshot is empty")
	}
	return a.up(ctx, prepared, &comparison)
}

func (a *App) up(ctx context.Context, prepared Prepared, reviewed *UpComparison) (retErr error) {
	if err := validatePreparedIntegrity(prepared); err != nil {
		return fmt.Errorf("validate prepared candidate: %w", err)
	}
	if err := naming.World(prepared.Result.Plan.Name); err != nil {
		return err
	}
	if err := a.Backend.Preflight(ctx); err != nil {
		return fmt.Errorf("runtime preflight: %w", err)
	}
	l := worldfs.For(a.BaseDir, prepared.Result.Plan.Name)
	if err := l.Ensure(); err != nil {
		return err
	}
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	defer lock.Release()
	if reviewed != nil {
		current, compareErr := a.CompareUpContext(ctx, prepared)
		if compareErr != nil {
			return fmt.Errorf("revalidate reviewed comparison: %w", compareErr)
		}
		if current.snapshot != reviewed.snapshot || current.recoveryPending != reviewed.recoveryPending ||
			!slices.Equal(current.Changes, reviewed.Changes) || current.Workspace != reviewed.Workspace {
			return fmt.Errorf("reviewed predecessor evidence changed; review the plan again")
		}
		*reviewed = current
	}
	if err := a.recoverTransition(ctx, l); err != nil {
		return fmt.Errorf("recover interrupted transition: %w", err)
	}
	if reviewed != nil && reviewed.recoveryPending {
		recovered, compareErr := a.CompareUpContext(ctx, prepared)
		if compareErr != nil {
			return fmt.Errorf("compare recovered predecessor: %w", compareErr)
		}
		if !slices.Equal(recovered.Changes, reviewed.Changes) || recovered.Workspace != reviewed.Workspace {
			return fmt.Errorf("transition recovery changed predecessor evidence; review the plan again")
		}
		*reviewed = recovered
	}
	prior, priorErr := l.ReadState()
	if priorErr != nil && !os.IsNotExist(priorErr) {
		return fmt.Errorf("read predecessor state: %w", priorErr)
	}
	priorExists := false
	priorActive := false
	if priorErr == nil && prior.Container != "" {
		priorExists, err = a.Backend.Exists(ctx, prior.Container)
		if err != nil {
			return fmt.Errorf("observe predecessor existence: %w", err)
		}
		if priorExists {
			evidence, inspectErr := a.Backend.Inspect(ctx, prior.Container)
			if inspectErr != nil {
				return fmt.Errorf("observe predecessor runtime: %w", inspectErr)
			}
			priorActive = evidence.Running
		}
	}
	if reviewed != nil {
		if reviewed.workspaceMode == workspaceModeActive && !priorActive {
			return fmt.Errorf("reviewed live predecessor is no longer active; review the workspace again")
		}
		if reviewed.workspaceMode == workspaceModeExact && priorActive {
			return fmt.Errorf("reviewed quiescent predecessor became active; review the workspace again")
		}
	}
	if priorErr == nil && prior.PlanDigest == prepared.Result.PlanDigest && prior.Container != "" && priorExists {
		if adopted, adoptErr := a.adopt(ctx, l, prior, prepared, priorActive); adoptErr != nil {
			return adoptErr
		} else if adopted {
			return nil
		}
	}
	var priorPrepared *Prepared
	if priorErr == nil && prior.Container != "" {
		loaded, loadErr := a.loadPredecessor(l, prior)
		if loadErr != nil {
			return fmt.Errorf("load predecessor intent: %w", loadErr)
		}
		priorPrepared = &loaded
	}
	if priorActive && priorPrepared != nil {
		if err := validateReplacementMountSources(priorPrepared.Result, prepared.Result); err != nil {
			return err
		}
	}
	generation := l.NextGeneration()
	var workspaceBeforeScaffolding *worldfs.DigestTree
	if reviewed == nil || reviewed.workspaceMode != workspaceModeActive {
		before, digestErr := a.digest(l.Workspace)
		if digestErr != nil {
			return fmt.Errorf("digest workspace before staging: %w", digestErr)
		}
		if reviewed != nil {
			switch reviewed.workspaceMode {
			case workspaceModeEmpty:
				if !workspaceHasOnlyEmptyMounts(before, l, prepared.Result.Plan.Workspace) {
					return fmt.Errorf("reviewed workspace changed before successor start; review the plan again")
				}
			case workspaceModeExact:
				if before.Root != reviewed.workspaceRoot {
					return fmt.Errorf("reviewed workspace changed before successor start; review the plan again")
				}
			default:
				return fmt.Errorf("reviewed workspace mode is invalid")
			}
			workspaceBeforeScaffolding = &before
		}
		fmt.Fprintf(a.Out, "workspace: %d entries (%s)\n", len(before.Entries), worldfs.ShortDigest(before.Root))
	}
	mounts, err := a.mounts(l, prepared.Result)
	if err != nil {
		return err
	}
	container, err := a.Backend.Create(ctx, prepared.Result, generation, mounts)
	if err != nil {
		return a.recordFailure(l, prepared, "create", err)
	}
	success := false
	cutover := false
	successorStarted := false
	successorProxy := false
	rolledBack := false
	transitionWritten := false
	rollback := func() error {
		if rolledBack || !cutover || !priorActive {
			return nil
		}
		rolledBack = true
		if priorPrepared == nil {
			return fmt.Errorf("predecessor intent unavailable")
		}
		return a.restorePredecessor(context.Background(), l, prior, *priorPrepared)
	}
	defer func() {
		if !success {
			var cleanup []error
			if successorProxy {
				cleanup = append(cleanup, a.stopProxy(l))
			}
			if successorStarted {
				cleanup = append(cleanup, a.Backend.Stop(context.Background(), container))
			}
			if cutover {
				cleanup = append(cleanup, rollback())
			}
			cleanup = append(cleanup, a.Backend.Destroy(context.Background(), container))
			if transitionWritten && errors.Join(cleanup...) == nil {
				cleanup = append(cleanup, l.ClearTransition())
			}
			retErr = errors.Join(retErr, errors.Join(cleanup...))
		}
	}()
	stagingCleared := false
	defer func() {
		if !stagingCleared {
			retErr = errors.Join(retErr, l.ClearStaging(generation))
		}
	}()
	if err := a.materialize(ctx, l, container, generation, prepared); err != nil {
		return a.recordFailure(l, prepared, "materialize", err)
	}
	if err := l.ClearStaging(generation); err != nil {
		return a.recordFailure(l, prepared, "clear staging", err)
	}
	stagingCleared = true
	absoluteDeclarationPath, _ := filepath.Abs(prepared.Path)
	state := worldfs.State{Name: prepared.Result.Plan.Name, Generation: generation, Container: container, PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, DeclarationPath: absoluteDeclarationPath, Status: "staging"}
	transition := worldfs.Transition{Version: 1, Phase: "rollback", Successor: state, SuccessorDeclaration: prepared.Raw}
	transition.SuccessorPlan, err = encodeRecoveryPlan(prepared.Result)
	if err != nil {
		return err
	}
	if priorErr == nil && prior.Container != "" {
		priorCopy := prior
		transition.Prior = &priorCopy
		transition.PriorWasRunning = priorActive
		if priorPrepared != nil {
			transition.PriorDeclaration = priorPrepared.Raw
			transition.PriorPlan, err = encodeRecoveryPlan(priorPrepared.Result)
			if err != nil {
				return err
			}
		}
	}
	if err := l.WriteTransition(transition); err != nil {
		return err
	}
	transitionWritten = true
	a.checkpoint("rollback-recorded")
	if priorActive {
		cutover = true
		if err := a.stopProxy(l); err != nil {
			return err
		}
		if err := a.Backend.Stop(ctx, prior.Container); err != nil {
			return a.recordFailure(l, prepared, "stop predecessor", err)
		}
		a.checkpoint("predecessor-stopped")
	}
	cutoverWorkspace, digestErr := a.digest(l.Workspace)
	if digestErr != nil {
		return fmt.Errorf("capture workspace at cutover: %w", digestErr)
	}
	if reviewed != nil {
		switch reviewed.workspaceMode {
		case workspaceModeEmpty, workspaceModeExact:
			if workspaceBeforeScaffolding == nil || !workspaceMatchesScaffolding(*workspaceBeforeScaffolding, cutoverWorkspace, l, prepared.Result.Plan.Workspace) {
				return fmt.Errorf("reviewed workspace changed before successor start; review the plan again")
			}
		case workspaceModeActive:
			if !priorActive {
				return fmt.Errorf("reviewed live predecessor is no longer active; review a stable workspace before replacement")
			}
		default:
			return fmt.Errorf("reviewed workspace mode is invalid")
		}
	}
	transition.Workspace = cutoverWorkspace
	if err := l.WriteTransition(transition); err != nil {
		return fmt.Errorf("record cutover workspace: %w", err)
	}
	a.checkpoint("cutover-workspace-recorded")
	if reviewed != nil && reviewed.workspaceMode == workspaceModeActive {
		fmt.Fprintf(a.Out, "workspace: captured authoritative cutover (%s)\n", worldfs.ShortDigest(cutoverWorkspace.Root))
	}
	if err := a.Backend.Start(ctx, container); err != nil {
		return a.recordFailure(l, prepared, "start successor", err)
	}
	successorStarted = true
	a.checkpoint("successor-started")
	evidence, err := a.Backend.Inspect(ctx, container)
	if err != nil {
		return a.recordFailure(l, prepared, "inspect successor", err)
	}
	if err := a.verifyRuntimeEvidence(evidence, prepared.Result, generation, mounts); err != nil {
		return a.recordFailure(l, prepared, "verify successor before services", err)
	}
	a.checkpoint("boundary-verified")
	proxyPID := 0
	if len(prepared.Result.Plan.NetworkAllow) > 0 {
		proxyPID, err = a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return a.recordFailure(l, prepared, "start proxy", err)
		}
		successorProxy = true
	}
	if err := a.startServices(ctx, container, prepared.Result.Plan.Services); err != nil {
		return a.recordFailure(l, prepared, "start services", err)
	}
	a.checkpoint("services-started")
	evidence, err = a.Backend.Inspect(ctx, container)
	if err != nil || a.verifyRuntimeEvidence(evidence, prepared.Result, generation, mounts) != nil {
		if err == nil {
			err = a.verifyRuntimeEvidence(evidence, prepared.Result, generation, mounts)
		}
		return a.recordFailure(l, prepared, "verify successor", err)
	}
	a.checkpoint("successor-verified")
	after, err := worldfs.Digest(l.Workspace)
	if err != nil {
		return err
	}
	state.Status = "running"
	state.ProxyPID = proxyPID
	transition.Phase = "commit"
	transition.Successor = state
	transition.Workspace = after
	transition.ImageDigests = []string{prepared.Result.Plan.World.Base}
	if err := l.WriteTransition(transition); err != nil {
		return err
	}
	a.checkpoint("commit-recorded")
	if _, err := l.WriteDigest(generation, after); err != nil {
		return err
	}
	a.checkpoint("digest-written")
	if err := l.WriteApplied(prepared.Raw); err != nil {
		return err
	}
	a.checkpoint("declaration-written")
	if err := l.WriteAppliedPlan(transition.SuccessorPlan); err != nil {
		return err
	}
	a.checkpoint("recovery-plan-written")
	if err := l.WriteState(state); err != nil {
		return err
	}
	a.checkpoint("state-written")
	if _, err := history.AppendOnce(l.History, history.Record{Action: "up", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, ImageDigests: []string{prepared.Result.Plan.World.Base}, WorkspaceDigest: after.Root, Outcome: "applied"}, a.Now()); err != nil {
		return err
	}
	a.checkpoint("history-written")
	success = true
	if priorErr == nil && priorExists && prior.Container != "" && prior.Container != container {
		if err := a.Backend.Destroy(ctx, prior.Container); err != nil {
			return fmt.Errorf("applied successor but remove predecessor: %w", err)
		}
		a.checkpoint("predecessor-destroyed")
	}
	if err := l.ClearTransition(); err != nil {
		return fmt.Errorf("applied successor but clear transition: %w", err)
	}
	a.checkpoint("transition-cleared")
	fmt.Fprintf(a.Out, "applied %s generation g%d (%s)\n", prepared.Result.Plan.Name, generation, prepared.Result.PlanDigest)
	return nil
}

// CompareUp acquires the settled or transition-authoritative predecessor plan
// and the carried-workspace evidence that runUp must render before confirmation.
// Only a world with no authority, no orphaned durable evidence, and no carried
// workspace entries is classified as new; every other read or validation
// failure is returned to the caller.
func (a *App) CompareUp(prepared Prepared) (UpComparison, error) {
	return a.CompareUpContext(context.Background(), prepared)
}

// CompareUpContext is CompareUp with cancellation for runtime observations.
func (a *App) CompareUpContext(ctx context.Context, prepared Prepared) (UpComparison, error) {
	if err := validatePreparedIntegrity(prepared); err != nil {
		return UpComparison{}, fmt.Errorf("validate prepared candidate: %w", err)
	}
	if err := naming.World(prepared.Result.Plan.Name); err != nil {
		return UpComparison{}, err
	}
	l := worldfs.For(a.BaseDir, prepared.Result.Plan.Name)

	transition, transitionErr := l.ReadTransition()
	if transitionErr != nil && !os.IsNotExist(transitionErr) {
		return UpComparison{}, fmt.Errorf("read predecessor transition: %w", transitionErr)
	}
	var transitionEvidence *worldfs.Transition
	if transitionErr == nil {
		transitionEvidence = &transition
	}

	var state worldfs.State
	var prior Prepared
	priorExists := false
	if transitionErr == nil {
		if err := validateComparisonTransition(transition, prepared.Result.Plan.Name); err != nil {
			return UpComparison{}, fmt.Errorf("validate predecessor transition: %w", err)
		}
		authoritative, _ := transitionAuthority(transition)
		if authoritative != nil {
			state = *authoritative
			loaded, err := a.authoritativePrepared(l, state)
			if err != nil {
				return UpComparison{}, fmt.Errorf("prepare authoritative predecessor: %w", err)
			}
			prior = loaded
			priorExists = true
		}
	} else {
		var err error
		state, err = l.ReadState()
		if err == nil {
			if err := validateComparisonState(state, prepared.Result.Plan.Name, "running", "down"); err != nil {
				return UpComparison{}, fmt.Errorf("validate predecessor state: %w", err)
			}
			loaded, loadErr := a.loadPredecessor(l, state)
			if loadErr != nil {
				return UpComparison{}, fmt.Errorf("prepare predecessor: %w", loadErr)
			}
			prior = loaded
			priorExists = true
		} else if !os.IsNotExist(err) {
			return UpComparison{}, fmt.Errorf("read predecessor state: %w", err)
		} else if _, stateEntryErr := os.Lstat(l.State); stateEntryErr == nil {
			return UpComparison{}, fmt.Errorf("read predecessor state: %w", err)
		} else if !os.IsNotExist(stateEntryErr) {
			return UpComparison{}, fmt.Errorf("inspect predecessor state: %w", stateEntryErr)
		} else if _, appliedErr := os.Lstat(l.Applied); appliedErr == nil {
			return UpComparison{}, fmt.Errorf("predecessor state is missing while applied declaration exists")
		} else if !os.IsNotExist(appliedErr) {
			return UpComparison{}, fmt.Errorf("inspect predecessor declaration: %w", appliedErr)
		} else if _, planErr := os.Lstat(l.AppliedPlan); planErr == nil {
			return UpComparison{}, fmt.Errorf("predecessor state is missing while applied plan exists")
		} else if !os.IsNotExist(planErr) {
			return UpComparison{}, fmt.Errorf("inspect predecessor plan: %w", planErr)
		} else if recorded, digestErr := directoryHasEntries(l.Digests); digestErr != nil {
			return UpComparison{}, fmt.Errorf("inspect predecessor workspace digests: %w", digestErr)
		} else if recorded {
			return UpComparison{}, fmt.Errorf("predecessor state is missing while recorded workspace digests exist")
		} else if err := validateNewWorldArtifacts(l); err != nil {
			return UpComparison{}, err
		}
	}

	changes := []plan.Change{}
	if priorExists {
		var err error
		changes, err = plan.Diff(prior.Result.Plan, prepared.Result.Plan)
		if err != nil {
			return UpComparison{}, fmt.Errorf("compare predecessor plan: %w", err)
		}
	}
	var priorDigest *worldfs.DigestTree
	if priorExists {
		var digest worldfs.DigestTree
		var err error
		if transitionErr == nil && transition.Phase == "commit" && transition.Workspace.Root != "" {
			digest = transition.Workspace
			err = worldfs.ValidateDigestTree(digest)
		} else {
			digest, err = l.ReadDigest(state.Generation)
		}
		if err != nil {
			return UpComparison{}, fmt.Errorf("read predecessor workspace digest: %w", err)
		}
		priorDigest = &digest
	}
	if priorExists && state.Status == "running" && a.Backend != nil {
		active, err := a.comparisonPredecessorActive(ctx, l, state, prior.Result)
		if err != nil {
			return UpComparison{}, fmt.Errorf("observe predecessor runtime for workspace comparison: %w", err)
		}
		if active {
			if err := verifyComparisonHistory(l, true, transitionEvidence); err != nil {
				return UpComparison{}, err
			}
			workspace := fmt.Sprintf("workspace: live authoritative g%d (may advance during apply; applied %s)", state.Generation, worldfs.ShortDigest(priorDigest.Root))
			return newUpComparison(prepared, changes, workspace, workspaceModeActive, transitionEvidence, &state, &prior, nil, priorDigest)
		}
	}

	current, currentErr := worldfs.Digest(l.Workspace)
	if currentErr != nil {
		if !priorExists && errors.Is(currentErr, os.ErrNotExist) {
			if err := verifyComparisonHistory(l, false, transitionEvidence); err != nil {
				return UpComparison{}, err
			}
			workspace := "workspace: new (no carried state)"
			return newUpComparison(prepared, changes, workspace, workspaceModeEmpty, transitionEvidence, nil, nil, nil, nil)
		}
		return UpComparison{}, fmt.Errorf("digest current workspace: %w", currentErr)
	}
	if !priorExists && workspaceHasOnlyEmptyMounts(current, l, prepared.Result.Plan.Workspace) {
		if err := verifyComparisonHistory(l, false, transitionEvidence); err != nil {
			return UpComparison{}, err
		}
		return newUpComparison(prepared, changes, "workspace: new (no carried state)", workspaceModeEmpty, transitionEvidence, nil, nil, nil, nil)
	}
	if !priorExists && transitionErr != nil {
		return UpComparison{}, fmt.Errorf("predecessor state is missing while carried workspace entries exist")
	}

	workspace := fmt.Sprintf("workspace: new (%d entries, %s)", len(current.Entries), worldfs.ShortDigest(current.Root))
	if priorExists {
		workspace = fmt.Sprintf("workspace: %d files changed since g%d (%s -> %s)", worldfs.ChangedFiles(*priorDigest, current), state.Generation, worldfs.ShortDigest(priorDigest.Root), worldfs.ShortDigest(current.Root))
	}
	if err := verifyComparisonHistory(l, priorExists, transitionEvidence); err != nil {
		return UpComparison{}, err
	}
	var stateEvidence *worldfs.State
	var priorEvidence *Prepared
	if priorExists {
		stateEvidence = &state
		priorEvidence = &prior
	}
	return newUpComparison(prepared, changes, workspace, workspaceModeExact, transitionEvidence, stateEvidence, priorEvidence, &current, priorDigest)
}

func (a *App) comparisonPredecessorActive(ctx context.Context, l worldfs.Layout, state worldfs.State, result plan.Result) (bool, error) {
	exists, err := a.Backend.Exists(ctx, state.Container)
	if err != nil || !exists {
		return false, err
	}
	evidence, err := a.Backend.Inspect(ctx, state.Container)
	if err != nil {
		return false, err
	}
	if !evidence.Running {
		return false, nil
	}
	expected := make([]backend.Mount, 0, len(result.Plan.Workspace)+len(result.Plan.Mounts))
	for _, target := range result.Plan.Workspace {
		expected = append(expected, backend.Mount{Source: l.WorkspacePath(target), Target: target, Mode: "rw"})
	}
	for _, mount := range result.Plan.Mounts {
		expected = append(expected, backend.Mount{Source: mount.Source, Target: mount.Target, Mode: mount.Mode})
	}
	if err := a.verifyRuntimeEvidence(evidence, result, state.Generation, expected); err != nil {
		return false, fmt.Errorf("running container does not match recorded authority: %w", err)
	}
	return true, nil
}

func validateComparisonTransition(transition worldfs.Transition, world string) error {
	successorStatus := "staging"
	if transition.Phase == "commit" {
		successorStatus = "running"
	}
	if err := validateComparisonState(transition.Successor, world, successorStatus); err != nil {
		return fmt.Errorf("successor: %w", err)
	}
	if transition.Prior == nil {
		if transition.PriorWasRunning {
			return fmt.Errorf("prior is absent but recorded running")
		}
		if transition.Successor.Generation != 1 {
			return fmt.Errorf("first successor generation is %d, not 1", transition.Successor.Generation)
		}
		return nil
	}
	if err := validateComparisonState(*transition.Prior, world, "running", "down"); err != nil {
		return fmt.Errorf("prior: %w", err)
	}
	if transition.Successor.Generation != transition.Prior.Generation+1 {
		return fmt.Errorf("successor generation %d does not follow prior generation %d", transition.Successor.Generation, transition.Prior.Generation)
	}
	if transition.PriorWasRunning && transition.Prior.Status != "running" {
		return fmt.Errorf("prior is recorded running with status %q", transition.Prior.Status)
	}
	return nil
}

func validateComparisonState(state worldfs.State, world string, allowedStatuses ...string) error {
	if state.Name != world {
		return fmt.Errorf("name %q does not match world %q", state.Name, world)
	}
	if state.Generation < 1 {
		return fmt.Errorf("generation %d is not positive", state.Generation)
	}
	wantContainer := backend.ContainerName(world, state.Generation)
	if state.Container != wantContainer {
		return fmt.Errorf("container %q does not match %q", state.Container, wantContainer)
	}
	if !slices.Contains(allowedStatuses, state.Status) {
		return fmt.Errorf("status %q is not valid here", state.Status)
	}
	if state.ProxyPID < 0 {
		return fmt.Errorf("proxy PID %d is negative", state.ProxyPID)
	}
	if (state.Status == "down" || state.Status == "staging") && state.ProxyPID != 0 {
		return fmt.Errorf("status %q has proxy PID %d", state.Status, state.ProxyPID)
	}
	return nil
}

func workspaceHasOnlyEmptyMounts(tree worldfs.DigestTree, l worldfs.Layout, targets []string) bool {
	allowed := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		allowed[filepath.Base(l.WorkspacePath(target))] = struct{}{}
	}
	if len(tree.Entries) == 0 || tree.Entries[0].Path != "" || !isEmptyWorkspaceMount(tree.Entries[0]) {
		return false
	}
	for _, entry := range tree.Entries[1:] {
		if _, ok := allowed[entry.Path]; !ok || !isEmptyWorkspaceMount(entry) {
			return false
		}
	}
	return true
}

// workspaceMatchesScaffolding proves that mounts changed no reviewed entry and
// added only the candidate's missing empty mount roots. This distinguishes
// deterministic Kenogram structure from carried or externally changed data.
func workspaceMatchesScaffolding(before, after worldfs.DigestTree, l worldfs.Layout, targets []string) bool {
	baseline := make(map[string]worldfs.DigestEntry, len(before.Entries))
	for _, entry := range before.Entries {
		baseline[entry.Path] = entry
	}
	allowedAdded := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		path := filepath.Base(l.WorkspacePath(target))
		if entry, ok := baseline[path]; ok {
			if entry.Type != "directory" {
				return false
			}
		} else {
			allowedAdded[path] = struct{}{}
		}
	}
	observed := make(map[string]worldfs.DigestEntry, len(after.Entries))
	for _, entry := range after.Entries {
		observed[entry.Path] = entry
		if prior, ok := baseline[entry.Path]; ok {
			if entry != prior {
				return false
			}
			continue
		}
		if _, ok := allowedAdded[entry.Path]; !ok || !isEmptyWorkspaceMount(entry) {
			return false
		}
	}
	if len(observed) != len(baseline)+len(allowedAdded) {
		return false
	}
	for path := range baseline {
		if _, ok := observed[path]; !ok {
			return false
		}
	}
	for path := range allowedAdded {
		if _, ok := observed[path]; !ok {
			return false
		}
	}
	return true
}

func isEmptyWorkspaceMount(entry worldfs.DigestEntry) bool {
	return entry.Type == "directory" && entry.Mode == 0o700 && entry.Size == 0 && entry.SHA256 == "" && entry.Link == ""
}

func directoryHasEntries(path string) (bool, error) {
	directory, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer directory.Close()
	entries, err := directory.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return len(entries) > 0, err
}

func validateNewWorldArtifacts(l worldfs.Layout) error {
	for _, artifact := range []struct {
		path string
		name string
	}{{l.ProxyPID, "proxy identity"}, {l.ProxySocket, "proxy socket"}} {
		if _, err := os.Lstat(artifact.path); err == nil {
			return fmt.Errorf("predecessor state is missing while %s exists", artifact.name)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect predecessor %s: %w", artifact.name, err)
		}
	}
	if recorded, err := directoryHasEntries(l.Staging); err != nil {
		return fmt.Errorf("inspect predecessor staging: %w", err)
	} else if recorded {
		return fmt.Errorf("predecessor state is missing while staged generation artifacts exist")
	}
	records, err := history.Verify(l.History)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify predecessor history: %w", err)
	}
	if !historyIsAuthorityFree(records) {
		return fmt.Errorf("predecessor state is missing while authoritative history exists")
	}
	return nil
}

func historyIsAuthorityFree(records []history.Record) bool {
	seenFailure := false
	for _, record := range records {
		switch {
		case record.Action == "up" && record.Outcome == "failed":
			seenFailure = true
		case record.Action == "history-repair" && record.Outcome == "truncated-tail-removed":
			if !seenFailure || record.PlanDigest != "" || record.DeclarationDigest != "" || len(record.ImageDigests) != 0 || record.WorkspaceDigest != "" || record.Detail != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func verifyComparisonHistory(l worldfs.Layout, priorExists bool, transition *worldfs.Transition) error {
	required := priorExists && transition == nil
	if transition != nil {
		required = transition.Prior != nil
	}
	records, err := history.Verify(l.History)
	if os.IsNotExist(err) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify predecessor history: %w", err)
	}
	if required && len(records) == 0 {
		return fmt.Errorf("verify predecessor history: authoritative history is empty")
	}
	return nil
}

func validatePreparedIntegrity(prepared Prepared) error {
	canonical, err := plan.Canonical(prepared.Result.Plan)
	if err != nil {
		return fmt.Errorf("canonicalize plan: %w", err)
	}
	planSum := sha256.Sum256(canonical)
	if hex.EncodeToString(planSum[:]) != prepared.Result.PlanDigest {
		return fmt.Errorf("plan digest does not match prepared plan")
	}
	if DeclarationDigest(prepared.Raw) != prepared.Result.DeclarationDigest {
		return fmt.Errorf("declaration digest does not match prepared bytes")
	}
	return nil
}

func newUpComparison(prepared Prepared, changes []plan.Change, workspace, workspaceMode string, transition *worldfs.Transition, state *worldfs.State, prior *Prepared, current, priorDigest *worldfs.DigestTree) (UpComparison, error) {
	payload := struct {
		Changes                    []plan.Change       `json:"changes"`
		Workspace                  string              `json:"workspace"`
		WorkspaceMode              string              `json:"workspace_mode"`
		CandidatePlanDigest        string              `json:"candidate_plan_digest"`
		CandidateDeclarationDigest string              `json:"candidate_declaration_digest"`
		Transition                 *worldfs.Transition `json:"transition,omitempty"`
		State                      *worldfs.State      `json:"state,omitempty"`
		PriorPlanDigest            string              `json:"prior_plan_digest,omitempty"`
		PriorDeclarationDigest     string              `json:"prior_declaration_digest,omitempty"`
		CurrentWorkspaceRoot       string              `json:"current_workspace_root,omitempty"`
		PredecessorWorkspaceRoot   string              `json:"predecessor_workspace_root,omitempty"`
	}{Changes: changes, Workspace: workspace, WorkspaceMode: workspaceMode, CandidatePlanDigest: prepared.Result.PlanDigest, CandidateDeclarationDigest: prepared.Result.DeclarationDigest, Transition: transition, State: state}
	if prior != nil {
		payload.PriorPlanDigest = prior.Result.PlanDigest
		payload.PriorDeclarationDigest = DeclarationDigest(prior.Raw)
	}
	if current != nil {
		payload.CurrentWorkspaceRoot = current.Root
	}
	if priorDigest != nil {
		payload.PredecessorWorkspaceRoot = priorDigest.Root
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return UpComparison{}, fmt.Errorf("encode comparison snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	comparison := UpComparison{Changes: changes, Workspace: workspace, snapshot: hex.EncodeToString(sum[:]), recoveryPending: transition != nil, workspaceMode: workspaceMode}
	if current != nil {
		comparison.workspaceRoot = current.Root
	}
	return comparison, nil
}

func (a *App) adopt(ctx context.Context, l worldfs.Layout, state worldfs.State, prepared Prepared, running bool) (adopted bool, retErr error) {
	restarted := false
	proxyStarted := false
	committed := false
	defer func() {
		if !committed {
			var cleanup []error
			if proxyStarted {
				cleanup = append(cleanup, a.stopProxy(l))
			}
			if restarted {
				cleanup = append(cleanup, a.Backend.Stop(context.Background(), state.Container))
			}
			retErr = errors.Join(retErr, errors.Join(cleanup...))
		}
	}()
	if !running {
		if err := a.Backend.Start(ctx, state.Container); err != nil {
			return false, nil
		}
		restarted = true
	}
	evidence, err := a.Backend.Inspect(ctx, state.Container)
	if err != nil {
		return false, nil
	}
	mounts, err := a.mounts(l, prepared.Result)
	if err != nil {
		return false, err
	}
	if err := a.verifyRuntimeEvidence(evidence, prepared.Result, state.Generation, mounts); err != nil {
		return false, nil
	}
	proxyPID := state.ProxyPID
	if len(prepared.Result.Plan.NetworkAllow) > 0 {
		if a.proxyAlive(l) {
			destinations := make([]proxy.Destination, 0, len(prepared.Result.Plan.NetworkAllow))
			for _, allowed := range prepared.Result.Plan.NetworkAllow {
				destinations = append(destinations, proxy.Destination{Host: allowed.Host, Port: int(allowed.Port)})
			}
			err = proxy.SendControlContext(ctx, l.ProxySocket, proxy.ControlRequest{Operation: "reconcile", Destinations: destinations})
		}
		if !a.proxyAlive(l) || err != nil {
			proxyPID, err = a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
			if err != nil {
				return false, err
			}
			proxyStarted = true
		}
	} else if a.proxyAlive(l) {
		if err := a.stopProxy(l); err != nil {
			return false, err
		}
		proxyPID = 0
	}
	if restarted {
		if err := a.startServices(ctx, state.Container, prepared.Result.Plan.Services); err != nil {
			return false, err
		}
	}
	state.Status = "running"
	state.ProxyPID = proxyPID
	recoveryPlan, err := encodeRecoveryPlan(prepared.Result)
	if err != nil {
		return false, err
	}
	if err := l.WriteAppliedPlan(recoveryPlan); err != nil {
		return false, err
	}
	if err := l.WriteState(state); err != nil {
		return false, err
	}
	workspaceDigest, workspaceDetail, err := adoptionWorkspaceEvidence(l.Workspace, worldfs.Digest)
	if err != nil {
		return false, err
	}
	outcome := "adopted"
	if restarted {
		outcome = "restarted"
	}
	if _, err := history.Append(l.History, history.Record{Action: "up", PlanDigest: state.PlanDigest, DeclarationDigest: state.DeclarationDigest, WorkspaceDigest: workspaceDigest, Outcome: outcome, Detail: workspaceDetail}, a.Now()); err != nil {
		return false, err
	}
	fmt.Fprintf(a.Out, "%s %s generation g%d (%s)\n", state.Name, outcome, state.Generation, state.PlanDigest)
	committed = true
	return true, nil
}

func adoptionWorkspaceEvidence(path string, digest func(string) (worldfs.DigestTree, error)) (string, string, error) {
	tree, err := digest(path)
	if err == nil {
		return tree.Root, "", nil
	}
	if worldfs.IsChanging(err) {
		return "", "stable workspace digest unavailable: live workspace was changing during adoption", nil
	}
	return "", "", err
}
func (a *App) proxyAlive(l worldfs.Layout) bool {
	raw, err := os.ReadFile(l.ProxyPID)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 || lockfile.ProcessStart(pid) != fields[1] {
		return false
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	if !strings.Contains(string(cmdline), "_proxy") || !strings.Contains(string(cmdline), l.ProxySocket) {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	_, err = os.Stat(l.ProxySocket)
	return err == nil
}

func (a *App) loadPredecessor(l worldfs.Layout, prior worldfs.State) (Prepared, error) {
	raw, err := os.ReadFile(l.Applied)
	if err != nil {
		return Prepared{}, err
	}
	sourcePath := prior.DeclarationPath
	if sourcePath == "" {
		sourcePath = l.Applied
	}
	if rawPlan, planErr := os.ReadFile(l.AppliedPlan); planErr == nil {
		return preparedFromRecovery(raw, rawPlan, sourcePath, prior)
	} else if !os.IsNotExist(planErr) {
		return Prepared{}, planErr
	}
	prepared, err := PrepareBytes(raw, sourcePath)
	if err != nil {
		return Prepared{}, err
	}
	if prepared.Result.PlanDigest != prior.PlanDigest {
		return Prepared{}, fmt.Errorf("applied predecessor plan %s does not match state %s", prepared.Result.PlanDigest, prior.PlanDigest)
	}
	if prepared.Result.DeclarationDigest != prior.DeclarationDigest {
		return Prepared{}, fmt.Errorf("applied predecessor declaration %s does not match state %s", prepared.Result.DeclarationDigest, prior.DeclarationDigest)
	}
	return prepared, nil
}

func encodeRecoveryPlan(result plan.Result) ([]byte, error) {
	type wireResult plan.Result
	raw, err := json.MarshalIndent(wireResult(result), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func preparedFromRecovery(rawDeclaration, rawPlan []byte, path string, state worldfs.State) (Prepared, error) {
	type wireResult plan.Result
	var wire wireResult
	if err := json.Unmarshal(rawPlan, &wire); err != nil {
		return Prepared{}, fmt.Errorf("decode applied plan: %w", err)
	}
	result := plan.Result(wire)
	canonical, err := plan.Canonical(result.Plan)
	if err != nil {
		return Prepared{}, err
	}
	planSum := sha256.Sum256(canonical)
	if hex.EncodeToString(planSum[:]) != state.PlanDigest || result.PlanDigest != state.PlanDigest {
		return Prepared{}, fmt.Errorf("applied recovery plan does not match state plan digest")
	}
	if DeclarationDigest(rawDeclaration) != state.DeclarationDigest || result.DeclarationDigest != state.DeclarationDigest {
		return Prepared{}, fmt.Errorf("applied recovery plan does not match state declaration digest")
	}
	return Prepared{Raw: rawDeclaration, Result: result, Path: path}, nil
}

func (a *App) recoverTransition(ctx context.Context, l worldfs.Layout) error {
	transition, err := l.ReadTransition()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if transition.Phase == "rollback" {
		return a.rollbackTransition(ctx, l, transition)
	}

	path := transition.Successor.DeclarationPath
	if path == "" {
		path = l.Applied
	}
	prepared, prepareErr := preparedFromRecovery(transition.SuccessorDeclaration, transition.SuccessorPlan, path, transition.Successor)
	if prepareErr != nil {
		return fmt.Errorf("committed successor intent is invalid; preserving commit transition: %w", prepareErr)
	}
	exists, existsErr := a.Backend.Exists(ctx, transition.Successor.Container)
	if existsErr != nil {
		return fmt.Errorf("observe committed successor; preserving commit transition: %w", existsErr)
	}
	if !exists {
		return fmt.Errorf("committed successor %s is absent; preserving commit transition", transition.Successor.Container)
	}
	evidence, inspectErr := a.Backend.Inspect(ctx, transition.Successor.Container)
	if inspectErr != nil {
		return fmt.Errorf("inspect committed successor; preserving commit transition: %w", inspectErr)
	}
	restarted := false
	if !evidence.Running {
		if err := a.Backend.Start(ctx, transition.Successor.Container); err != nil {
			return fmt.Errorf("restart committed successor; preserving commit transition: %w", err)
		}
		restarted = true
		evidence, inspectErr = a.Backend.Inspect(ctx, transition.Successor.Container)
		if inspectErr != nil {
			return fmt.Errorf("inspect restarted committed successor; preserving commit transition: %w", inspectErr)
		}
	}
	mounts, mountsErr := a.mounts(l, prepared.Result)
	if mountsErr != nil {
		return fmt.Errorf("reconstruct committed successor mounts; preserving commit transition: %w", mountsErr)
	}
	if verifyErr := a.verifyRuntimeEvidence(evidence, prepared.Result, transition.Successor.Generation, mounts); verifyErr != nil {
		return fmt.Errorf("verify committed successor; preserving commit transition: %w", verifyErr)
	}
	if len(prepared.Result.Plan.NetworkAllow) > 0 && (restarted || !a.proxyIsReady(ctx, l)) {
		pid, err := a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return fmt.Errorf("restore committed successor proxy; preserving commit transition: %w", err)
		}
		transition.Successor.ProxyPID = pid
		if err := l.WriteTransition(transition); err != nil {
			return err
		}
	}
	if err := a.startServices(ctx, transition.Successor.Container, prepared.Result.Plan.Services); err != nil {
		return fmt.Errorf("restore committed successor services; preserving commit transition: %w", err)
	}
	if _, err := l.WriteDigest(transition.Successor.Generation, transition.Workspace); err != nil {
		return err
	}
	if err := l.WriteApplied(transition.SuccessorDeclaration); err != nil {
		return err
	}
	if err := l.WriteAppliedPlan(transition.SuccessorPlan); err != nil {
		return err
	}
	if err := l.WriteState(transition.Successor); err != nil {
		return err
	}
	if _, err := history.AppendOnce(l.History, history.Record{Action: "up", PlanDigest: transition.Successor.PlanDigest, DeclarationDigest: transition.Successor.DeclarationDigest, ImageDigests: transition.ImageDigests, WorkspaceDigest: transition.Workspace.Root, Outcome: "applied"}, a.Now()); err != nil {
		return err
	}
	if transition.Prior != nil && transition.Prior.Container != "" && transition.Prior.Container != transition.Successor.Container {
		exists, err := a.Backend.Exists(ctx, transition.Prior.Container)
		if err != nil {
			return err
		}
		if exists {
			if err := a.Backend.Destroy(ctx, transition.Prior.Container); err != nil {
				return err
			}
		}
	}
	return l.ClearTransition()
}

func (a *App) rollbackTransition(ctx context.Context, l worldfs.Layout, transition worldfs.Transition) error {
	if err := a.stopProxy(l); err != nil {
		return err
	}
	if transition.Successor.Container != "" {
		exists, err := a.Backend.Exists(ctx, transition.Successor.Container)
		if err != nil {
			return err
		}
		if exists {
			if err := a.Backend.Destroy(ctx, transition.Successor.Container); err != nil {
				return err
			}
		}
	}
	if transition.Prior == nil {
		for _, path := range []string{l.State, l.Applied, l.AppliedPlan} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return l.ClearTransition()
	}
	if len(transition.PriorDeclaration) == 0 {
		return fmt.Errorf("predecessor declaration is missing")
	}
	path := transition.Prior.DeclarationPath
	if path == "" {
		path = l.Applied
	}
	prepared, err := preparedFromRecovery(transition.PriorDeclaration, transition.PriorPlan, path, *transition.Prior)
	if err != nil {
		return err
	}
	if transition.PriorWasRunning {
		if err := a.restorePredecessor(ctx, l, *transition.Prior, prepared); err != nil {
			return err
		}
	} else {
		if err := l.WriteApplied(transition.PriorDeclaration); err != nil {
			return err
		}
		if err := l.WriteAppliedPlan(transition.PriorPlan); err != nil {
			return err
		}
		if err := l.WriteState(*transition.Prior); err != nil {
			return err
		}
	}
	return l.ClearTransition()
}

func (a *App) restorePredecessor(ctx context.Context, l worldfs.Layout, prior worldfs.State, prepared Prepared) error {
	exists, err := a.Backend.Exists(ctx, prior.Container)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("predecessor container %s is missing", prior.Container)
	}
	evidence, err := a.Backend.Inspect(ctx, prior.Container)
	if err != nil {
		return err
	}
	if !evidence.Running {
		if err := a.Backend.Start(ctx, prior.Container); err != nil {
			return err
		}
		evidence, err = a.Backend.Inspect(ctx, prior.Container)
		if err != nil {
			return err
		}
	}
	mounts, err := a.mounts(l, prepared.Result)
	if err != nil {
		return err
	}
	if err := a.verifyRuntimeEvidence(evidence, prepared.Result, prior.Generation, mounts); err != nil {
		return err
	}
	proxyPID := 0
	if len(prepared.Result.Plan.NetworkAllow) > 0 {
		proxyPID, err = a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return err
		}
	}
	if err := a.startServices(ctx, prior.Container, prepared.Result.Plan.Services); err != nil {
		return err
	}
	prior.ProxyPID = proxyPID
	prior.Status = "running"
	if err := l.WriteApplied(prepared.Raw); err != nil {
		return err
	}
	recoveryPlan, err := encodeRecoveryPlan(prepared.Result)
	if err != nil {
		return err
	}
	if err := l.WriteAppliedPlan(recoveryPlan); err != nil {
		return err
	}
	return l.WriteState(prior)
}

func (a *App) mounts(l worldfs.Layout, result plan.Result) ([]backend.Mount, error) {
	if err := a.ValidateHostMounts(result); err != nil {
		return nil, err
	}
	mounts := []backend.Mount{}
	for _, target := range result.Plan.Workspace {
		source, err := l.EnsureWorkspace(target)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, backend.Mount{Source: source, Target: target, Mode: "rw"})
	}
	for _, m := range result.Plan.Mounts {
		mounts = append(mounts, backend.Mount{Source: m.Source, Target: m.Target, Mode: m.Mode})
	}
	return mounts, nil
}

// ValidateHostMounts performs the host-specific, read-only safety checks that
// complement declaration parsing. It is safe to call during dry-run.
func (a *App) ValidateHostMounts(result plan.Result) error {
	for _, mount := range result.Plan.Mounts {
		if err := a.validateHostMountSource(mount.Source, false); err != nil {
			return fmt.Errorf("mount source %q: %w", mount.Source, err)
		}
	}
	return nil
}

func (a *App) verifyRuntimeEvidence(evidence backend.Evidence, result plan.Result, generation int64, expected []backend.Mount) error {
	workspace := make(map[string]bool, len(result.Plan.Workspace))
	for _, target := range result.Plan.Workspace {
		workspace[target] = true
	}
	for _, mount := range evidence.Mounts {
		if err := a.validateHostMountSource(mount.Source, workspace[mount.Destination]); err != nil {
			return fmt.Errorf("observed mount source %q: %w", mount.Source, err)
		}
	}
	return backend.Verify(evidence, result, generation, expected)
}

func (a *App) validateHostMountSource(source string, allowState bool) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("must be a regular file or directory")
	}
	protected := []string{}
	if !allowState {
		protected = append(protected, a.BaseDir)
	}
	uid := strconv.Itoa(os.Getuid())
	protected = append(protected,
		filepath.Join("/run/user", uid, "podman", "podman.sock"),
		filepath.Join("/run/user", uid, "docker.sock"),
		"/run/podman/podman.sock",
		"/run/docker.sock",
		"/var/run/docker.sock",
	)
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		protected = append(protected, filepath.Join(runtimeDir, "podman", "podman.sock"), filepath.Join(runtimeDir, "docker.sock"))
	}
	for _, variable := range []string{"CONTAINER_HOST", "DOCKER_HOST"} {
		if endpoint := strings.TrimSpace(os.Getenv(variable)); strings.HasPrefix(endpoint, "unix://") {
			protected = append(protected, strings.TrimPrefix(endpoint, "unix://"))
		}
	}
	for _, path := range protected {
		if path != "" && hostPathsOverlap(source, path) {
			return fmt.Errorf("overlaps protected host path %q", path)
		}
	}
	return nil
}

func validateReplacementMountSources(prior, next plan.Result) error {
	for _, oldMount := range prior.Plan.Mounts {
		if oldMount.Mode != "rw" {
			continue
		}
		oldSource := canonicalHostPath(oldMount.Source)
		for _, newMount := range next.Plan.Mounts {
			newSource := canonicalHostPath(newMount.Source)
			if newSource != oldSource && strings.HasPrefix(newSource, oldSource+string(os.PathSeparator)) {
				return fmt.Errorf("mount source %q is beneath predecessor-writable source %q", newMount.Source, oldMount.Source)
			}
		}
	}
	return nil
}

func canonicalHostPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(evaluated)
	}
	return filepath.Clean(absolute)
}

func hostPathsOverlap(first, second string) bool {
	first, firstErr := filepath.Abs(first)
	second, secondErr := filepath.Abs(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	first, second = filepath.Clean(first), filepath.Clean(second)
	if evaluated, err := filepath.EvalSymlinks(first); err == nil {
		first = filepath.Clean(evaluated)
	}
	if evaluated, err := filepath.EvalSymlinks(second); err == nil {
		second = filepath.Clean(evaluated)
	}
	return first == second || strings.HasPrefix(first, second+string(os.PathSeparator)) || strings.HasPrefix(second, first+string(os.PathSeparator))
}
func (a *App) materialize(ctx context.Context, l worldfs.Layout, container string, generation int64, p Prepared) error {
	for i, c := range p.Result.Plan.Copies {
		liveDigest, err := plan.DigestSource(c.Source)
		if err != nil {
			return err
		}
		if liveDigest != c.SourceDigest {
			return fmt.Errorf("copy source %s changed after planning", c.Source)
		}
		stage, err := l.StageSource(generation, i, c.Source, c.Mode)
		if err != nil {
			return err
		}
		stagedDigest, err := plan.DigestSource(stage)
		if err != nil {
			return err
		}
		if stagedDigest != c.SourceDigest {
			return fmt.Errorf("staging did not preserve copy source %s", c.Source)
		}
		if err := l.ApplyStageMode(stage, c.Mode); err != nil {
			return err
		}
		if err := a.Backend.Copy(ctx, container, stage, c.Target); err != nil {
			return err
		}
	}
	root := filepath.Join(l.Staging, fmt.Sprintf("g%d", generation), "generated")
	if err := os.MkdirAll(filepath.Join(root, "etc", "kenogram", "services"), 0o700); err != nil {
		return err
	}
	// Service supervisors run as the declared world user. Materialize their
	// runtime status directory with host-user ownership so they never need
	// permission to create a child directly under root-owned /run.
	if err := os.MkdirAll(filepath.Join(root, "run", "kenogram", "services"), 0o700); err != nil {
		return err
	}
	inside := insideDocument()
	if err := os.WriteFile(filepath.Join(root, "KENOGRAM.md"), []byte(inside), 0o444); err != nil {
		return err
	}
	projection := map[string]any{"name": p.Result.Plan.Name, "generation": generation, "plan_digest": p.Result.PlanDigest, "declaration_digest": p.Result.DeclarationDigest, "mounts": p.Result.Plan.Mounts, "allowed_destinations": p.Result.Plan.NetworkAllow, "interfaces": p.Result.Plan.Interfaces, "resources": p.Result.Plan.Resources, "workspace_paths": p.Result.Plan.Workspace, "door": doorAddress(p.Result.Plan.NetworkAllow)}
	raw, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "kenogram", "world.json"), append(raw, '\n'), 0o444); err != nil {
		return err
	}
	for _, service := range p.Result.Plan.Services {
		script := serviceScript(service)
		if err := naming.Service(service.Name); err != nil {
			return err
		}
		serviceRoot := filepath.Join(root, "etc", "kenogram", "services")
		servicePath, err := naming.JoinUnder(serviceRoot, service.Name+".sh")
		if err != nil {
			return err
		}
		if err := os.WriteFile(servicePath, []byte(script), 0o555); err != nil {
			return err
		}
	}
	// Preserve the trailing /. so Podman copies the generated directory's
	// contents into the container root rather than creating /generated.
	return a.Backend.Copy(ctx, container, root+string(os.PathSeparator)+".", "/")
}

func doorAddress(allows []plan.NetworkAllow) string {
	if len(allows) == 0 {
		return ""
	}
	return "127.0.0.1:3128"
}
func serviceScript(s plan.Service) string {
	command := make([]string, len(s.Command))
	for i, arg := range s.Command {
		command[i] = shellQuote(arg)
	}
	line := strings.Join(command, " ")
	prefix := "#!/bin/sh\nset -u\nstatus_dir=/run/kenogram/services\nmkdir -p \"$status_dir\"\nstatus=\"$status_dir/" + s.Name + "\"\nsupervisor=\"$status.supervisor\"\nprintf '%s\\n' \"$$\" >\"$supervisor\"\ntrap 'rm -f \"$supervisor\"' EXIT HUP INT TERM\nrun_service() {\n  printf 'starting\\n' >\"$status\"\n  " + line + " &\n  pid=$!\n  printf 'running %s\\n' \"$pid\" >\"$status\"\n  wait \"$pid\"\n  code=$?\n  printf 'exited %s\\n' \"$code\" >\"$status\"\n  return \"$code\"\n}\n"
	switch s.Restart {
	case "always":
		return prefix + "while :; do\n  run_service || :\n  sleep 1\ndone\n"
	case "on-failure":
		return prefix + "while :; do\n  run_service\n  code=$?\n  [ \"$code\" -eq 0 ] && exit 0\n  sleep 1\ndone\n"
	default:
		return prefix + "run_service\n"
	}
}

func serviceObservationCommand(name string) []string {
	status := "/run/kenogram/services/" + name
	script := "status=" + shellQuote(status) + "; supervisor=\"$status.supervisor\"; " +
		"if [ -r \"$supervisor\" ]; then read pid <\"$supervisor\"; " +
		"if kill -0 \"$pid\" 2>/dev/null; then printf 'supervised\\n'; exit 0; fi; fi; cat \"$status\""
	return []string{"/bin/sh", "-c", script}
}

func serviceConverged(status string) bool {
	return status == "supervised" || status == "exited 0"
}

func (a *App) startServices(ctx context.Context, container string, services []plan.Service) error {
	for _, service := range services {
		if !service.Autostart {
			continue
		}
		observation := serviceObservationCommand(service.Name)
		raw, observationErr := a.Backend.ExecOutput(ctx, container, observation)
		if observationErr == nil && serviceConverged(strings.TrimSpace(string(raw))) {
			continue
		}
		script := "/etc/kenogram/services/" + service.Name + ".sh"
		if err := a.Backend.Exec(ctx, container, true, []string{"/bin/sh", script}); err != nil {
			return fmt.Errorf("%s: %w", service.Name, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			raw, err := a.Backend.ExecOutput(ctx, container, observation)
			status := strings.TrimSpace(string(raw))
			if err == nil && serviceConverged(status) {
				// A service command may intentionally daemonize (tmux is the
				// canonical example). Its successful acknowledgement is
				// observable even though no foreground child remains.
				break
			}
			if err == nil && strings.HasPrefix(status, "exited ") {
				return fmt.Errorf("%s became %s", service.Name, status)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not report running", service.Name)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func insideDocument() string {
	return `You are running inside a Kenogram world.

Everything visible here belongs to this world and may be modified.
Resources outside this world are absent. This world's network reaches
only the destinations listed in /etc/kenogram/world.json, through the
proxy at 127.0.0.1:3128. There is no name resolution here; names are
resolved on the other side of the door. Software that needs raw DNS or
UDP will not work.

To request a change, state what you need and why through the configured
terminal interaction. Kenogram treats that request as prose, not authority.
An operator decides whether to edit and reapply the host-side declaration.
Processes restart when the world is replaced; after resuming, re-state
anything still pending.

Files under /workspace survive replacement; everything else is rebuilt.
`
}
func (a *App) recordFailure(l worldfs.Layout, p Prepared, detail string, cause error) error {
	_, historyErr := history.Append(l.History, history.Record{Action: "up", PlanDigest: p.Result.PlanDigest, DeclarationDigest: p.Result.DeclarationDigest, Outcome: "failed", Detail: detail + ": " + cause.Error()}, a.Now())
	return errors.Join(fmt.Errorf("%s: %w", detail, cause), historyErr)
}

func (a *App) startProxy(ctx context.Context, l worldfs.Layout, pid int, allows []plan.NetworkAllow) (int, error) {
	if err := a.stopProxy(l); err != nil {
		return 0, err
	}
	args := []string{"_proxy", "--pid", strconv.Itoa(pid), "--control", l.ProxySocket, "--log", filepath.Join(l.Root, "proxy.log")}
	for _, allow := range allows {
		args = append(args, "--allow", netJoin(allow.Host, int(allow.Port)))
	}
	// The proxy outlives the command that applies the world. After readiness,
	// its lifecycle is owned explicitly through ProxyPID and stopProxy rather
	// than the startup context. A new session separates it from the applying
	// terminal and process group.
	command := exec.Command(a.Executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logFile, logErr := os.OpenFile(filepath.Join(l.Root, "proxy.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if logErr != nil {
		return 0, logErr
	}
	defer logFile.Close()
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		return 0, err
	}
	processPID := command.Process.Pid
	start := lockfile.ProcessStart(processPID)
	if start == "" {
		return 0, errors.Join(fmt.Errorf("observe proxy process start"), abortProxyStart(command, l))
	}
	if err := os.WriteFile(l.ProxyPID, []byte(strconv.Itoa(processPID)+" "+start+"\n"), 0o600); err != nil {
		return 0, errors.Join(err, abortProxyStart(command, l))
	}
	if err := command.Process.Release(); err != nil {
		return 0, errors.Join(err, abortProxyStart(command, l))
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if a.proxyIsReady(ctx, l) {
			return processPID, nil
		}
		if time.Now().After(deadline) {
			return 0, errors.Join(fmt.Errorf("proxy did not become ready"), a.stopProxy(l))
		}
		select {
		case <-ctx.Done():
			return 0, errors.Join(ctx.Err(), a.stopProxy(l))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func abortProxyStart(command *exec.Cmd, l worldfs.Layout) error {
	if command.Process != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	return errors.Join(removeIfExists(l.ProxyPID), removeIfExists(l.ProxySocket))
}

func netJoin(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
func (a *App) stopProxy(l worldfs.Layout) error {
	raw, err := os.ReadFile(l.ProxyPID)
	if os.IsNotExist(err) {
		return removeIfExists(l.ProxySocket)
	}
	if err != nil {
		return err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return fmt.Errorf("invalid proxy identity in %s", l.ProxyPID)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid proxy PID in %s", l.ProxyPID)
	}
	start := lockfile.ProcessStart(pid)
	if start != "" {
		if start != fields[1] {
			return fmt.Errorf("proxy PID %d was reused; ownership is uncertain", pid)
		}
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if readErr != nil {
			return fmt.Errorf("observe proxy PID %d: %w", pid, readErr)
		}
		if !strings.Contains(string(cmdline), "_proxy") || !strings.Contains(string(cmdline), l.ProxySocket) {
			return fmt.Errorf("proxy PID %d ownership is uncertain", pid)
		}
		if killErr := syscall.Kill(pid, syscall.SIGTERM); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return killErr
		}
		deadline := time.Now().Add(5 * time.Second)
		for lockfile.ProcessStart(pid) == fields[1] && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
		}
		if lockfile.ProcessStart(pid) == fields[1] {
			return fmt.Errorf("proxy PID %d did not exit", pid)
		}
	}
	return errors.Join(removeIfExists(l.ProxyPID), removeIfExists(l.ProxySocket))
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (a *App) Down(ctx context.Context, name string) error {
	if err := naming.World(name); err != nil {
		return err
	}
	l := worldfs.For(a.BaseDir, name)
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := a.Backend.Preflight(ctx); err != nil {
		return fmt.Errorf("runtime preflight: %w", err)
	}
	if err := a.recoverTransition(ctx, l); err != nil {
		return fmt.Errorf("recover interrupted transition: %w", err)
	}
	s, err := l.ReadState()
	if err != nil {
		return err
	}
	if s.Status == "running" {
		if err := a.Backend.Stop(ctx, s.Container); err != nil {
			return err
		}
	}
	if err := a.stopProxy(l); err != nil {
		return err
	}
	s.Status = "down"
	s.ProxyPID = 0
	if err := l.WriteState(s); err != nil {
		return err
	}
	_, err = history.Append(l.History, history.Record{Action: "down", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "stopped"}, a.Now())
	return err
}
func (a *App) Destroy(ctx context.Context, name string) error {
	if err := naming.World(name); err != nil {
		return err
	}
	l := worldfs.For(a.BaseDir, name)
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	if err := a.Backend.Preflight(ctx); err != nil {
		lock.Release()
		return fmt.Errorf("runtime preflight: %w", err)
	}
	s, detail, err := a.destroyRuntime(ctx, l)
	if err != nil {
		lock.Release()
		return err
	}
	if _, err := history.Append(l.History, history.Record{Action: "destroy", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "destroyed", Detail: detail}, a.Now()); err != nil {
		lock.Release()
		return err
	}
	tombstoneParent := filepath.Join(a.BaseDir, ".destroyed")
	if err := os.MkdirAll(tombstoneParent, 0o700); err != nil {
		lock.Release()
		return err
	}
	tombstone := filepath.Join(tombstoneParent, name+"-"+strconv.FormatInt(a.Now().UnixNano(), 10))
	if err := os.Rename(l.Root, tombstone); err != nil {
		lock.Release()
		return err
	}
	if err := syncDirectory(tombstoneParent); err != nil {
		lock.Release()
		return err
	}
	if err := lock.ReleaseMoved(filepath.Join(tombstone, "mutation.lock")); err != nil {
		return err
	}
	entries, err := os.ReadDir(tombstone)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "history.jsonl" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(tombstone, entry.Name())); err != nil {
			return err
		}
	}
	return syncDirectory(tombstone)
}

func (a *App) destroyRuntime(ctx context.Context, l worldfs.Layout) (worldfs.State, string, error) {
	transition, transitionErr := l.ReadTransition()
	if transitionErr != nil && !os.IsNotExist(transitionErr) {
		return worldfs.State{}, "", fmt.Errorf("read transition before destroy: %w", transitionErr)
	}

	states := []worldfs.State{}
	authoritative := worldfs.State{}
	detail := ""
	if transitionErr == nil {
		current, candidate := transitionAuthority(transition)
		if current != nil {
			authoritative = *current
			states = append(states, *current)
		}
		if candidate != nil {
			states = append(states, *candidate)
		}
		if authoritative.Container == "" {
			authoritative = transition.Successor
		}
		detail = "removed unresolved " + transition.Phase + " transition"
	} else {
		state, err := l.ReadState()
		if err != nil {
			return worldfs.State{}, "", err
		}
		authoritative = state
		states = append(states, state)
	}

	seen := map[string]bool{}
	for _, state := range states {
		if state.Container == "" || seen[state.Container] {
			continue
		}
		seen[state.Container] = true
		exists, err := a.Backend.Exists(ctx, state.Container)
		if err != nil {
			return worldfs.State{}, "", err
		}
		if exists {
			if err := a.Backend.Destroy(ctx, state.Container); err != nil {
				return worldfs.State{}, "", err
			}
		}
	}
	if err := a.stopProxy(l); err != nil {
		return worldfs.State{}, "", err
	}
	return authoritative, detail, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
func (a *App) Enter(ctx context.Context, name string, repair bool) error {
	if err := naming.World(name); err != nil {
		return err
	}
	l := worldfs.For(a.BaseDir, name)
	s, err := authoritativeState(l)
	if err != nil {
		return err
	}
	command := []string{"/usr/bin/tmux", "attach-session", "-t", "main"}
	if repair {
		command = []string{"/bin/sh"}
	}
	return a.Backend.Attach(ctx, s.Container, command)
}

// Connect opens one declared operator-facing interface in the authoritative
// generation. The world lock protects generation selection and descriptor
// acquisition, but is released before callers relay any bytes.
func (a *App) Connect(ctx context.Context, name, interfaceName string) (net.Conn, error) {
	if err := naming.World(name); err != nil {
		return nil, err
	}
	if err := naming.Interface(interfaceName); err != nil {
		return nil, err
	}
	l := worldfs.For(a.BaseDir, name)
	if _, err := os.Stat(l.Root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("world %q does not exist", name)
		}
		return nil, fmt.Errorf("inspect world %q: %w", name, err)
	}
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	state, err := authoritativeState(l)
	if err != nil {
		return nil, err
	}
	prepared, err := a.authoritativePrepared(l, state)
	if err != nil {
		return nil, fmt.Errorf("load authoritative plan: %w", err)
	}
	address := ""
	for _, endpoint := range prepared.Result.Plan.Interfaces {
		if endpoint.Name == interfaceName {
			address = endpoint.Address
			break
		}
	}
	if address == "" {
		return nil, fmt.Errorf("interface %q is not declared for world %q", interfaceName, name)
	}
	evidence, err := a.Backend.Inspect(ctx, state.Container)
	if err != nil {
		return nil, fmt.Errorf("inspect authoritative generation: %w", err)
	}
	expected := make([]backend.Mount, 0, len(prepared.Result.Plan.Workspace)+len(prepared.Result.Plan.Mounts))
	for _, target := range prepared.Result.Plan.Workspace {
		expected = append(expected, backend.Mount{Source: l.WorkspacePath(target), Target: target, Mode: "rw"})
	}
	for _, mount := range prepared.Result.Plan.Mounts {
		expected = append(expected, backend.Mount{Source: mount.Source, Target: mount.Target, Mode: mount.Mode})
	}
	if err := a.verifyRuntimeEvidence(evidence, prepared.Result, state.Generation, expected); err != nil {
		return nil, fmt.Errorf("verify authoritative generation: %w", err)
	}
	acquire := a.acquireConnection
	if acquire == nil {
		acquire = netns.AcquireConnection
	}
	connection, err := acquire(ctx, evidence.PID, evidence.ProcessStart, address, func() error {
		current, inspectErr := a.Backend.Inspect(ctx, state.Container)
		if inspectErr != nil {
			return inspectErr
		}
		if current.PID != evidence.PID || current.ProcessStart != evidence.ProcessStart {
			return fmt.Errorf("runtime process identity changed after namespace pin")
		}
		return a.verifyRuntimeEvidence(current, prepared.Result, state.Generation, expected)
	})
	if err != nil {
		return nil, fmt.Errorf("connect interface %q: %w", interfaceName, err)
	}
	return connection, nil
}

func (a *App) authoritativePrepared(l worldfs.Layout, state worldfs.State) (Prepared, error) {
	transition, err := l.ReadTransition()
	if err == nil {
		if transition.Phase == "commit" {
			path := transition.Successor.DeclarationPath
			if path == "" {
				path = l.Applied
			}
			return preparedFromRecovery(transition.SuccessorDeclaration, transition.SuccessorPlan, path, state)
		}
		if transition.Prior == nil {
			return Prepared{}, fmt.Errorf("rollback has no authoritative predecessor")
		}
		path := transition.Prior.DeclarationPath
		if path == "" {
			path = l.Applied
		}
		return preparedFromRecovery(transition.PriorDeclaration, transition.PriorPlan, path, state)
	}
	if !os.IsNotExist(err) {
		return Prepared{}, err
	}
	return a.loadPredecessor(l, state)
}
func (a *App) Allow(name, destination, duration string) error {
	if err := naming.World(name); err != nil {
		return err
	}
	d, err := proxy.ParseDestination(destination)
	if err != nil {
		return err
	}
	if _, err := time.ParseDuration(duration); err != nil {
		return err
	}
	l := worldfs.For(a.BaseDir, name)
	lock, lockErr := lockfile.Acquire(l.Lock)
	if lockErr != nil {
		return lockErr
	}
	defer lock.Release()
	if err := requireSettledTransition(l); err != nil {
		return err
	}
	if err := proxy.SendControl(l.ProxySocket, proxy.ControlRequest{Operation: "grant", Host: d.Host, Port: d.Port, Duration: duration}); err != nil {
		return err
	}
	s, _ := l.ReadState()
	_, err = history.Append(l.History, history.Record{Action: "allow", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "granted", Detail: destination + " for " + duration}, a.Now())
	return err
}
func (a *App) Revoke(name, destination string) error {
	if err := naming.World(name); err != nil {
		return err
	}
	d, err := proxy.ParseDestination(destination)
	if err != nil {
		return err
	}
	l := worldfs.For(a.BaseDir, name)
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := requireSettledTransition(l); err != nil {
		return err
	}
	if err := proxy.SendControl(l.ProxySocket, proxy.ControlRequest{Operation: "remove", Host: d.Host, Port: d.Port}); err != nil {
		return err
	}
	s, _ := l.ReadState()
	_, err = history.Append(l.History, history.Record{Action: "revoke", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "removed", Detail: destination}, a.Now())
	return err
}
func (a *App) RepairHistory(name string) error {
	if err := naming.World(name); err != nil {
		return err
	}
	l := worldfs.For(a.BaseDir, name)
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := requireSettledTransition(l); err != nil {
		return err
	}
	if err := history.RepairTruncatedTail(l.History); err != nil {
		return err
	}
	s, _ := l.ReadState()
	_, err = history.Append(l.History, history.Record{Action: "history-repair", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "truncated-tail-removed"}, a.Now())
	return err
}

func requireSettledTransition(l worldfs.Layout) error {
	transition, err := l.ReadTransition()
	if err == nil {
		return fmt.Errorf("world has an unresolved %s transition; run up, down, or destroy to recover it first", transition.Phase)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read transition before mutation: %w", err)
	}
	return nil
}
func transitionAuthority(transition worldfs.Transition) (authoritative, candidate *worldfs.State) {
	successor := transition.Successor
	if transition.Phase == "commit" {
		return &successor, transition.Prior
	}
	return transition.Prior, &successor
}

func authoritativeState(l worldfs.Layout) (worldfs.State, error) {
	transition, err := l.ReadTransition()
	if err == nil {
		authoritative, _ := transitionAuthority(transition)
		if authoritative == nil || authoritative.Container == "" {
			return worldfs.State{}, fmt.Errorf("no authoritative generation during %s recovery", transition.Phase)
		}
		return *authoritative, nil
	}
	if !os.IsNotExist(err) {
		return worldfs.State{}, err
	}
	return l.ReadState()
}

func (a *App) observeGeneration(ctx context.Context, state worldfs.State) (*GenerationObservation, error) {
	observation := &GenerationObservation{State: state}
	if state.Container == "" {
		return observation, nil
	}
	exists, err := a.Backend.Exists(ctx, state.Container)
	if err != nil || !exists {
		return observation, err
	}
	observation.Exists = true
	evidence, err := a.Backend.Inspect(ctx, state.Container)
	if err != nil {
		return observation, err
	}
	observation.Evidence = &evidence
	return observation, nil
}

func (a *App) Status(ctx context.Context, name string) (StatusResult, error) {
	if err := naming.World(name); err != nil {
		return StatusResult{}, err
	}
	l := worldfs.For(a.BaseDir, name)
	if transition, err := l.ReadTransition(); err == nil {
		authoritative, candidate := transitionAuthority(transition)
		result := StatusResult{RecoveryPhase: transition.Phase}
		if authoritative != nil {
			result.Authoritative, err = a.observeGeneration(ctx, *authoritative)
			if err != nil {
				return result, err
			}
		}
		if candidate != nil {
			result.Candidate, err = a.observeGeneration(ctx, *candidate)
			if err != nil {
				return result, err
			}
		}
		return result, nil
	} else if !os.IsNotExist(err) {
		return StatusResult{RecoveryPhase: "corrupt-transition"}, err
	}
	if _, err := history.Verify(l.History); err != nil {
		return StatusResult{}, fmt.Errorf("verify history: %w", err)
	}
	s, err := l.ReadState()
	if err != nil {
		return StatusResult{}, err
	}
	if s.Container == "" {
		return StatusResult{Authoritative: &GenerationObservation{State: s}}, nil
	}
	observation, err := a.observeGeneration(ctx, s)
	return StatusResult{Authoritative: observation}, err
}
func (a *App) Worlds() ([]worldfs.State, error) {
	entries, err := os.ReadDir(a.BaseDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	states := []worldfs.State{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == ".destroyed" {
			continue
		}
		l := worldfs.For(a.BaseDir, entry.Name())
		if transition, transitionErr := l.ReadTransition(); transitionErr == nil {
			authoritative, _ := transitionAuthority(transition)
			if authoritative == nil {
				states = append(states, worldfs.State{Name: entry.Name(), Status: "recovery-required:" + transition.Phase + ": no authoritative generation"})
			} else {
				state := *authoritative
				state.Status = "recovery-required:" + transition.Phase
				states = append(states, state)
			}
			continue
		} else if !os.IsNotExist(transitionErr) {
			states = append(states, worldfs.State{Name: entry.Name(), Status: "uncertain: " + transitionErr.Error()})
			continue
		}
		s, err := l.ReadState()
		if err != nil {
			states = append(states, worldfs.State{Name: entry.Name(), Status: "uncertain: " + err.Error()})
			continue
		}
		states = append(states, s)
	}
	return states, nil
}
func DeclarationDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

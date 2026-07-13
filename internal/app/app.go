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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/decl"
	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/lockfile"
	"github.com/idolum-ai/kenogram/internal/naming"
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
	return &App{Backend: backend.New(nil), BaseDir: base, Out: os.Stdout, Now: func() time.Time { return time.Now().UTC() }, Executable: executable}, nil
}

type Prepared struct {
	Raw         []byte
	Declaration decl.Declaration
	Result      plan.Result
	Path        string
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
	if err := a.recoverTransition(ctx, l); err != nil {
		return fmt.Errorf("recover interrupted transition: %w", err)
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
	generation := l.NextGeneration()
	before, _ := worldfs.Digest(l.Workspace)
	fmt.Fprintf(a.Out, "workspace: %d entries (%s)\n", len(before.Entries), worldfs.ShortDigest(before.Root))
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
	if err := a.materialize(ctx, l, container, generation, prepared); err != nil {
		return a.recordFailure(l, prepared, "materialize", err)
	}
	if err := l.ClearStaging(generation); err != nil {
		return a.recordFailure(l, prepared, "clear staging", err)
	}
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
	if priorActive {
		cutover = true
		if err := a.stopProxy(l); err != nil {
			return err
		}
		if err := a.Backend.Stop(ctx, prior.Container); err != nil {
			return a.recordFailure(l, prepared, "stop predecessor", err)
		}
	}
	if err := a.Backend.Start(ctx, container); err != nil {
		return a.recordFailure(l, prepared, "start successor", err)
	}
	successorStarted = true
	evidence, err := a.Backend.Inspect(ctx, container)
	if err != nil {
		return a.recordFailure(l, prepared, "inspect successor", err)
	}
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
	evidence, err = a.Backend.Inspect(ctx, container)
	if err != nil || backend.Verify(evidence, prepared.Result, generation) != nil {
		if err == nil {
			err = backend.Verify(evidence, prepared.Result, generation)
		}
		return a.recordFailure(l, prepared, "verify successor", err)
	}
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
	if _, err := l.WriteDigest(generation, after); err != nil {
		return err
	}
	if err := l.WriteApplied(prepared.Raw); err != nil {
		return err
	}
	if err := l.WriteAppliedPlan(transition.SuccessorPlan); err != nil {
		return err
	}
	if err := l.WriteState(state); err != nil {
		return err
	}
	if _, err := history.AppendOnce(l.History, history.Record{Action: "up", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, ImageDigests: []string{prepared.Result.Plan.World.Base}, WorkspaceDigest: after.Root, Outcome: "applied"}, a.Now()); err != nil {
		return err
	}
	success = true
	if priorErr == nil && priorExists && prior.Container != "" && prior.Container != container {
		if err := a.Backend.Destroy(ctx, prior.Container); err != nil {
			return fmt.Errorf("applied successor but remove predecessor: %w", err)
		}
	}
	if err := l.ClearTransition(); err != nil {
		return fmt.Errorf("applied successor but clear transition: %w", err)
	}
	fmt.Fprintf(a.Out, "applied %s generation g%d (%s)\n", prepared.Result.Plan.Name, generation, prepared.Result.PlanDigest)
	return nil
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
	if err := backend.Verify(evidence, prepared.Result, state.Generation); err != nil {
		return false, nil
	}
	proxyPID := state.ProxyPID
	if len(prepared.Result.Plan.NetworkAllow) > 0 && !a.proxyAlive(l) {
		proxyPID, err = a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return false, err
		}
		proxyStarted = true
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
	exists, existsErr := a.Backend.Exists(ctx, transition.Successor.Container)
	if prepareErr != nil || existsErr != nil || !exists {
		transition.Phase = "rollback"
		if err := l.WriteTransition(transition); err != nil {
			return err
		}
		return a.rollbackTransition(ctx, l, transition)
	}
	evidence, inspectErr := a.Backend.Inspect(ctx, transition.Successor.Container)
	if inspectErr != nil || backend.Verify(evidence, prepared.Result, transition.Successor.Generation) != nil {
		transition.Phase = "rollback"
		if err := l.WriteTransition(transition); err != nil {
			return err
		}
		return a.rollbackTransition(ctx, l, transition)
	}
	if len(prepared.Result.Plan.NetworkAllow) > 0 && !a.proxyAlive(l) {
		pid, err := a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return err
		}
		transition.Successor.ProxyPID = pid
		if err := l.WriteTransition(transition); err != nil {
			return err
		}
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
	restarted := false
	if !evidence.Running {
		if err := a.Backend.Start(ctx, prior.Container); err != nil {
			return err
		}
		restarted = true
		evidence, err = a.Backend.Inspect(ctx, prior.Container)
		if err != nil {
			return err
		}
	}
	if err := backend.Verify(evidence, prepared.Result, prior.Generation); err != nil {
		return err
	}
	proxyPID := 0
	if len(prepared.Result.Plan.NetworkAllow) > 0 {
		proxyPID, err = a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return err
		}
	}
	if restarted {
		if err := a.startServices(ctx, prior.Container, prepared.Result.Plan.Services); err != nil {
			return err
		}
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
	projection := map[string]any{"name": p.Result.Plan.Name, "generation": generation, "plan_digest": p.Result.PlanDigest, "declaration_digest": p.Result.DeclarationDigest, "mounts": p.Result.Plan.Mounts, "allowed_destinations": p.Result.Plan.NetworkAllow, "resources": p.Result.Plan.Resources, "workspace_paths": p.Result.Plan.Workspace, "door": doorAddress(p.Result.Plan.NetworkAllow)}
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
	prefix := "#!/bin/sh\nset -u\nstatus_dir=/run/kenogram/services\nmkdir -p \"$status_dir\"\nstatus=\"$status_dir/" + s.Name + "\"\nrun_service() {\n  printf 'starting\\n' >\"$status\"\n  " + line + " &\n  pid=$!\n  printf 'running %s\\n' \"$pid\" >\"$status\"\n  wait \"$pid\"\n  code=$?\n  printf 'exited %s\\n' \"$code\" >\"$status\"\n  return \"$code\"\n}\n"
	switch s.Restart {
	case "always":
		return prefix + "while :; do\n  run_service || :\n  sleep 1\ndone\n"
	case "on-failure":
		return prefix + "while :; do\n  run_service\n  code=$?\n  [ \"$code\" -eq 0 ] && exit 0\n  sleep 1\ndone\n"
	default:
		return prefix + "run_service\n"
	}
}

func (a *App) startServices(ctx context.Context, container string, services []plan.Service) error {
	for _, service := range services {
		if !service.Autostart {
			continue
		}
		script := "/etc/kenogram/services/" + service.Name + ".sh"
		if err := a.Backend.Exec(ctx, container, true, []string{"/bin/sh", script}); err != nil {
			return fmt.Errorf("%s: %w", service.Name, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			raw, err := a.Backend.ExecOutput(ctx, container, []string{"/bin/sh", "-c", "cat /run/kenogram/services/" + shellQuote(service.Name)})
			status := strings.TrimSpace(string(raw))
			if err == nil && strings.HasPrefix(status, "running ") {
				break
			}
			if err == nil && status == "exited 0" {
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

To request a change, describe what you need to your terminal:

    engram signal <what you need and why>

Signal discipline: one request at a time; the newest visible record wins.
If unanswered, re-emit. Processes restart when the world is replaced;
after resuming, re-state anything still pending.

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
	command := exec.CommandContext(ctx, a.Executable, args...)
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
		command.Process.Kill()
		return 0, fmt.Errorf("observe proxy process start")
	}
	if err := os.WriteFile(l.ProxyPID, []byte(strconv.Itoa(processPID)+" "+start+"\n"), 0o600); err != nil {
		command.Process.Kill()
		return 0, err
	}
	if err := command.Process.Release(); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(l.ProxySocket); err == nil {
			return processPID, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(processPID, syscall.SIGTERM)
	return 0, fmt.Errorf("proxy did not become ready")
}
func netJoin(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }
func (a *App) stopProxy(l worldfs.Layout) error {
	raw, err := os.ReadFile(l.ProxyPID)
	if os.IsNotExist(err) {
		return nil
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
	if err := a.recoverTransition(ctx, l); err != nil {
		lock.Release()
		return fmt.Errorf("recover interrupted transition: %w", err)
	}
	s, err := l.ReadState()
	if err != nil {
		lock.Release()
		return err
	}
	if s.Container != "" {
		exists, err := a.Backend.Exists(ctx, s.Container)
		if err != nil {
			lock.Release()
			return err
		}
		if exists {
			if err := a.Backend.Destroy(ctx, s.Container); err != nil {
				lock.Release()
				return err
			}
		}
	}
	if err := a.stopProxy(l); err != nil {
		lock.Release()
		return err
	}
	if _, err := history.Append(l.History, history.Record{Action: "destroy", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "destroyed"}, a.Now()); err != nil {
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
	s, err := l.ReadState()
	if err != nil {
		return err
	}
	command := []string{"/usr/bin/tmux", "attach-session", "-t", "main"}
	if repair {
		command = []string{"/bin/sh"}
	}
	return a.Backend.Attach(ctx, s.Container, command)
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
	if err := history.RepairTruncatedTail(l.History); err != nil {
		return err
	}
	s, _ := l.ReadState()
	_, err = history.Append(l.History, history.Record{Action: "history-repair", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "truncated-tail-removed"}, a.Now())
	return err
}
func (a *App) Status(ctx context.Context, name string) (worldfs.State, backend.Evidence, error) {
	if err := naming.World(name); err != nil {
		return worldfs.State{}, backend.Evidence{}, err
	}
	l := worldfs.For(a.BaseDir, name)
	if transition, err := l.ReadTransition(); err == nil {
		state := transition.Successor
		state.Status = "recovery-required:" + transition.Phase
		exists, observeErr := a.Backend.Exists(ctx, state.Container)
		if observeErr != nil || !exists {
			return state, backend.Evidence{}, observeErr
		}
		evidence, inspectErr := a.Backend.Inspect(ctx, state.Container)
		return state, evidence, inspectErr
	} else if !os.IsNotExist(err) {
		return worldfs.State{Name: name, Status: "recovery-required:corrupt-transition"}, backend.Evidence{}, err
	}
	if _, err := history.Verify(l.History); err != nil {
		return worldfs.State{}, backend.Evidence{}, fmt.Errorf("verify history: %w", err)
	}
	s, err := l.ReadState()
	if err != nil {
		return s, backend.Evidence{}, err
	}
	if s.Container == "" || s.Status != "running" {
		return s, backend.Evidence{}, nil
	}
	e, err := a.Backend.Inspect(ctx, s.Container)
	return s, e, err
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
		s, err := worldfs.For(a.BaseDir, entry.Name()).ReadState()
		if err != nil {
			states = append(states, worldfs.State{Name: entry.Name(), Status: "uncertain: " + err.Error()})
			continue
		}
		states = append(states, s)
	}
	return states, nil
}
func (a *App) WorkspaceDrift(name string) (string, error) {
	if err := naming.World(name); err != nil {
		return "", err
	}
	l := worldfs.For(a.BaseDir, name)
	current, err := worldfs.Digest(l.Workspace)
	if os.IsNotExist(err) {
		return "workspace: new (no carried state)", nil
	}
	if err != nil {
		return "", err
	}
	state, err := l.ReadState()
	if err != nil {
		return fmt.Sprintf("workspace: new (%d entries, %s)", len(current.Entries), worldfs.ShortDigest(current.Root)), nil
	}
	prior, err := l.ReadDigest(state.Generation)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("workspace: %d files changed since g%d (%s -> %s)", worldfs.ChangedFiles(prior, current), state.Generation, worldfs.ShortDigest(prior.Root), worldfs.ShortDigest(current.Root)), nil
}
func DeclarationDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

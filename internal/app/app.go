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

func (a *App) Up(ctx context.Context, prepared Prepared) error {
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
	prior, priorErr := l.ReadState()
	priorActive := false
	if priorErr == nil && prior.Status == "running" && prior.Container != "" {
		if evidence, inspectErr := a.Backend.Inspect(ctx, prior.Container); inspectErr == nil && evidence.Running {
			priorActive = true
		}
	}
	if priorErr == nil && prior.PlanDigest == prepared.Result.PlanDigest && prior.Container != "" {
		if adopted, adoptErr := a.adopt(ctx, l, prior, prepared, priorActive); adoptErr != nil {
			return adoptErr
		} else if adopted {
			return nil
		}
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
	rollback := func() {
		if rolledBack || !cutover || !priorActive {
			return
		}
		rolledBack = true
		_ = a.restorePredecessor(context.Background(), l, prior)
	}
	defer func() {
		if !success {
			if successorProxy {
				_ = a.stopProxy(l)
			}
			if successorStarted {
				_ = a.Backend.Stop(context.Background(), container)
			}
			if cutover {
				rollback()
			}
			_ = a.Backend.Destroy(context.Background(), container)
		}
	}()
	if err := a.materialize(ctx, l, container, generation, prepared); err != nil {
		return a.recordFailure(l, prepared, "materialize", err)
	}
	if priorActive {
		if err := a.stopProxy(l); err != nil {
			return err
		}
		if err := a.Backend.Stop(ctx, prior.Container); err != nil {
			return a.recordFailure(l, prepared, "stop predecessor", err)
		}
		cutover = true
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
	for _, service := range prepared.Result.Plan.Services {
		if service.Autostart {
			script := "/etc/kenogram/services/" + service.Name + ".sh"
			if err := a.Backend.Exec(ctx, container, true, []string{"/bin/sh", script}); err != nil {
				return a.recordFailure(l, prepared, "start service "+service.Name, err)
			}
		}
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
	if _, err := l.WriteDigest(generation, after); err != nil {
		return err
	}
	absoluteDeclarationPath, _ := filepath.Abs(prepared.Path)
	state := worldfs.State{Name: prepared.Result.Plan.Name, Generation: generation, Container: container, PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, DeclarationPath: absoluteDeclarationPath, Status: "running", ProxyPID: proxyPID}
	if err := l.WriteApplied(prepared.Raw); err != nil {
		return err
	}
	if err := l.WriteState(state); err != nil {
		return err
	}
	if _, err := history.Append(l.History, history.Record{Action: "up", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, ImageDigests: []string{prepared.Result.Plan.World.Base}, WorkspaceDigest: after.Root, Outcome: "applied"}, a.Now()); err != nil {
		return err
	}
	if priorErr == nil && prior.Container != "" && prior.Container != container {
		_ = a.Backend.Destroy(ctx, prior.Container)
	}
	success = true
	fmt.Fprintf(a.Out, "applied %s generation g%d (%s)\n", prepared.Result.Plan.Name, generation, prepared.Result.PlanDigest)
	return nil
}

func (a *App) adopt(ctx context.Context, l worldfs.Layout, state worldfs.State, prepared Prepared, running bool) (bool, error) {
	restarted := false
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
	}
	if restarted {
		for _, service := range prepared.Result.Plan.Services {
			if service.Autostart {
				if err := a.Backend.Exec(ctx, state.Container, true, []string{"/bin/sh", "/etc/kenogram/services/" + service.Name + ".sh"}); err != nil {
					return false, err
				}
			}
		}
	}
	state.Status = "running"
	state.ProxyPID = proxyPID
	if err := l.WriteState(state); err != nil {
		return false, err
	}
	tree, err := worldfs.Digest(l.Workspace)
	if err != nil {
		return false, err
	}
	outcome := "adopted"
	if restarted {
		outcome = "restarted"
	}
	if _, err := history.Append(l.History, history.Record{Action: "up", PlanDigest: state.PlanDigest, DeclarationDigest: state.DeclarationDigest, WorkspaceDigest: tree.Root, Outcome: outcome}, a.Now()); err != nil {
		return false, err
	}
	fmt.Fprintf(a.Out, "%s %s generation g%d (%s)\n", state.Name, outcome, state.Generation, state.PlanDigest)
	return true, nil
}
func (a *App) proxyAlive(l worldfs.Layout) bool {
	raw, err := os.ReadFile(l.ProxyPID)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
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

func (a *App) restorePredecessor(ctx context.Context, l worldfs.Layout, prior worldfs.State) error {
	if err := a.Backend.Start(ctx, prior.Container); err != nil {
		return err
	}
	evidence, err := a.Backend.Inspect(ctx, prior.Container)
	if err != nil {
		return err
	}
	raw, readErr := os.ReadFile(l.Applied)
	if readErr != nil {
		return readErr
	}
	sourcePath := prior.DeclarationPath
	if sourcePath == "" {
		sourcePath = l.Applied
	}
	prepared, err := PrepareBytes(raw, sourcePath)
	if err != nil {
		return err
	}
	proxyPID := 0
	if len(prepared.Result.Plan.NetworkAllow) > 0 {
		proxyPID, err = a.startProxy(ctx, l, evidence.PID, prepared.Result.Plan.NetworkAllow)
		if err != nil {
			return err
		}
	}
	for _, service := range prepared.Result.Plan.Services {
		if service.Autostart {
			if err := a.Backend.Exec(ctx, prior.Container, true, []string{"/bin/sh", "/etc/kenogram/services/" + service.Name + ".sh"}); err != nil {
				return err
			}
		}
	}
	prior.ProxyPID = proxyPID
	prior.Status = "running"
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
		stage, err := l.StageSource(generation, i, c.Source, c.Mode)
		if err != nil {
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
		if err := os.WriteFile(filepath.Join(root, "etc", "kenogram", "services", service.Name+".sh"), []byte(script), 0o555); err != nil {
			return err
		}
	}
	if err := a.Backend.Copy(ctx, container, filepath.Join(root, "KENOGRAM.md"), "/KENOGRAM.md"); err != nil {
		return err
	}
	return a.Backend.Copy(ctx, container, filepath.Join(root, "etc", "kenogram"), "/etc/kenogram")
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
	switch s.Restart {
	case "always":
		return "#!/bin/sh\nwhile :; do " + line + "; done\n"
	case "on-failure":
		return "#!/bin/sh\nwhile :; do\n  " + line + "\n  status=$?\n  [ \"$status\" -eq 0 ] && exit 0\ndone\n"
	default:
		return "#!/bin/sh\nexec " + line + "\n"
	}
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
	_, _ = history.Append(l.History, history.Record{Action: "up", PlanDigest: p.Result.PlanDigest, DeclarationDigest: p.Result.DeclarationDigest, Outcome: "failed", Detail: detail + ": " + cause.Error()}, a.Now())
	return fmt.Errorf("%s: %w", detail, cause)
}

func (a *App) startProxy(ctx context.Context, l worldfs.Layout, pid int, allows []plan.NetworkAllow) (int, error) {
	_ = a.stopProxy(l)
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
	if err := os.WriteFile(l.ProxyPID, []byte(strconv.Itoa(processPID)+"\n"), 0o600); err != nil {
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
func netJoin(host string, port int) string { return host + ":" + strconv.Itoa(port) }
func (a *App) stopProxy(l worldfs.Layout) error {
	raw, err := os.ReadFile(l.ProxyPID)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err == nil && pid > 1 {
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		owned := readErr == nil && strings.Contains(string(cmdline), "_proxy") && strings.Contains(string(cmdline), l.ProxySocket)
		if owned {
			if killErr := syscall.Kill(pid, syscall.SIGTERM); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
				return killErr
			}
		}
	}
	_ = os.Remove(l.ProxyPID)
	_ = os.Remove(l.ProxySocket)
	return nil
}

func (a *App) Down(ctx context.Context, name string) error {
	l := worldfs.For(a.BaseDir, name)
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	defer lock.Release()
	s, err := l.ReadState()
	if err != nil {
		return err
	}
	_ = a.stopProxy(l)
	if s.Status == "running" {
		if err := a.Backend.Stop(ctx, s.Container); err != nil {
			return err
		}
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
	l := worldfs.For(a.BaseDir, name)
	lock, err := lockfile.Acquire(l.Lock)
	if err != nil {
		return err
	}
	s, _ := l.ReadState()
	_ = a.stopProxy(l)
	if s.Container != "" {
		_ = a.Backend.Destroy(ctx, s.Container)
	}
	_, _ = history.Append(l.History, history.Record{Action: "destroy", PlanDigest: s.PlanDigest, DeclarationDigest: s.DeclarationDigest, Outcome: "destroyed"}, a.Now())
	historyBytes, _ := os.ReadFile(l.History)
	if err := lock.Release(); err != nil {
		return err
	}
	if err := os.RemoveAll(l.Root); err != nil {
		return err
	}
	tombstone := filepath.Join(a.BaseDir, ".destroyed", name+"-"+strconv.FormatInt(a.Now().UnixNano(), 10))
	if err := os.MkdirAll(tombstone, 0o700); err != nil {
		return err
	}
	if len(historyBytes) == 0 {
		return nil
	}
	file, err := os.OpenFile(filepath.Join(tombstone, "history.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(historyBytes); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
func (a *App) Enter(ctx context.Context, name string, repair bool) error {
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
func (a *App) Status(ctx context.Context, name string) (worldfs.State, backend.Evidence, error) {
	l := worldfs.For(a.BaseDir, name)
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
		if !entry.IsDir() {
			continue
		}
		s, err := worldfs.For(a.BaseDir, entry.Name()).ReadState()
		if err == nil {
			states = append(states, s)
		}
	}
	return states, nil
}
func (a *App) WorkspaceDrift(name string) (string, error) {
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

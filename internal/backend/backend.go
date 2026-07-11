// Package backend invokes the supported rootless container runtime through exact argv.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/idolum-ai/kenogram/internal/plan"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
	Start(context.Context, string, ...string) error
	Interactive(context.Context, string, ...string) error
}
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
func (ExecRunner) Start(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
func (ExecRunner) Interactive(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

type Podman struct {
	Runner Runner
	Binary string
}

func New(r Runner) *Podman {
	if r == nil {
		r = ExecRunner{}
	}
	return &Podman{Runner: r, Binary: "podman"}
}
func ContainerName(world string, generation int64) string {
	return fmt.Sprintf("kenogram-%s-g%d", world, generation)
}

func (p *Podman) Preflight(ctx context.Context) error {
	raw, err := p.Runner.Run(ctx, p.Binary, "info", "--format", "json")
	if err != nil {
		return err
	}
	var info struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
			CgroupVersion string `json:"cgroupVersion"`
			IDMappings    struct {
				UIDMap []struct {
					Size int64 `json:"size"`
				} `json:"uidmap"`
				GIDMap []struct {
					Size int64 `json:"size"`
				} `json:"gidmap"`
			} `json:"idMappings"`
		} `json:"host"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("decode podman info: %w", err)
	}
	if !info.Host.Security.Rootless {
		return fmt.Errorf("podman must run rootless")
	}
	if info.Host.CgroupVersion != "v2" {
		return fmt.Errorf("cgroups v2 required, got %q", info.Host.CgroupVersion)
	}
	var uidSize, gidSize int64
	for _, item := range info.Host.IDMappings.UIDMap {
		uidSize += item.Size
	}
	for _, item := range info.Host.IDMappings.GIDMap {
		gidSize += item.Size
	}
	if uidSize <= 1 || gidSize <= 1 {
		return fmt.Errorf("rootless Podman requires subordinate UID and GID ranges for keep-id")
	}
	return nil
}

type Mount struct {
	Source, Target, Mode string
	NoExec               bool
}

func (p *Podman) Create(ctx context.Context, result plan.Result, generation int64, mounts []Mount) (string, error) {
	name := ContainerName(result.Plan.Name, generation)
	args := []string{"create", "--name", name, "--network", "none", "--ipc", "private", "--pid", "private", "--uts", "private", "--userns", "keep-id", "--hostname", result.Plan.World.Hostname, "--user", result.Plan.World.User, "--workdir", result.Plan.World.Workdir, "--cpus", strconv.FormatInt(result.Plan.Resources.CPUs, 10), "--memory", strconv.FormatInt(result.Plan.Resources.MemoryBytes, 10), "--pids-limit", strconv.FormatInt(result.Plan.Resources.PIDs, 10), "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--label", "io.kenogram.world=" + result.Plan.Name, "--label", "io.kenogram.generation=" + strconv.FormatInt(generation, 10), "--label", "io.kenogram.plan-digest=" + result.PlanDigest, "--label", "io.kenogram.declaration-digest=" + result.DeclarationDigest, "--env", "NO_PROXY=localhost,127.0.0.1"}
	if len(result.Plan.NetworkAllow) > 0 {
		args = append(args, "--env", "HTTP_PROXY=http://127.0.0.1:3128", "--env", "HTTPS_PROXY=http://127.0.0.1:3128")
	}
	for _, m := range mounts {
		options := m.Mode + ",nodev,nosuid"
		if m.NoExec {
			options += ",noexec"
		}
		args = append(args, "--mount", "type=bind,src="+m.Source+",dst="+m.Target+","+options)
	}
	args = append(args, result.Plan.World.Base, "/usr/bin/tail", "-f", "/dev/null")
	if _, err := p.Runner.Run(ctx, p.Binary, args...); err != nil {
		return "", err
	}
	return name, nil
}
func (p *Podman) Copy(ctx context.Context, container, source, target string) error {
	_, err := p.Runner.Run(ctx, p.Binary, "cp", source, container+":"+target)
	return err
}
func (p *Podman) Start(ctx context.Context, name string) error {
	_, err := p.Runner.Run(ctx, p.Binary, "start", name)
	return err
}
func (p *Podman) Stop(ctx context.Context, name string) error {
	_, err := p.Runner.Run(ctx, p.Binary, "stop", "--time", "10", name)
	return err
}
func (p *Podman) Destroy(ctx context.Context, name string) error {
	_, err := p.Runner.Run(ctx, p.Binary, "rm", "--force", name)
	return err
}
func (p *Podman) Exec(ctx context.Context, name string, detach bool, command []string) error {
	args := []string{"exec"}
	if detach {
		args = append(args, "--detach")
	}
	args = append(args, name)
	args = append(args, command...)
	_, err := p.Runner.Run(ctx, p.Binary, args...)
	return err
}
func (p *Podman) Attach(ctx context.Context, name string, command []string) error {
	args := append([]string{"exec", "--interactive", "--tty", name}, command...)
	return p.Runner.Interactive(ctx, p.Binary, args...)
}

type Evidence struct {
	Name                   string
	Running                bool
	PID                    int
	NetworkMode            string
	Labels                 map[string]string
	MountTargets           []string
	Memory, NanoCPUs, PIDs int64
}
type inspectDocument struct {
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
		Pid     int  `json:"Pid"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
		Memory      int64  `json:"Memory"`
		NanoCPUs    int64  `json:"NanoCpus"`
		PidsLimit   int64  `json:"PidsLimit"`
	} `json:"HostConfig"`
	Mounts []struct {
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

func (p *Podman) Inspect(ctx context.Context, name string) (Evidence, error) {
	raw, err := p.Runner.Run(ctx, p.Binary, "inspect", name)
	if err != nil {
		return Evidence{}, err
	}
	var docs []inspectDocument
	if err := json.Unmarshal(raw, &docs); err != nil || len(docs) != 1 {
		return Evidence{}, fmt.Errorf("decode podman inspect: %w", err)
	}
	d := docs[0]
	e := Evidence{Name: strings.TrimPrefix(d.Name, "/"), Running: d.State.Running, PID: d.State.Pid, NetworkMode: d.HostConfig.NetworkMode, Labels: d.Config.Labels, Memory: d.HostConfig.Memory, NanoCPUs: d.HostConfig.NanoCPUs, PIDs: d.HostConfig.PidsLimit}
	for _, m := range d.Mounts {
		e.MountTargets = append(e.MountTargets, m.Destination)
	}
	return e, nil
}
func Verify(e Evidence, result plan.Result, generation int64) error {
	if !e.Running {
		return fmt.Errorf("container is not running")
	}
	if e.NetworkMode != "none" {
		return fmt.Errorf("network mode is %q, want none", e.NetworkMode)
	}
	expected := ContainerName(result.Plan.Name, generation)
	if e.Name != expected {
		return fmt.Errorf("container name %q, want %q", e.Name, expected)
	}
	checks := map[string]string{"io.kenogram.world": result.Plan.Name, "io.kenogram.generation": strconv.FormatInt(generation, 10), "io.kenogram.plan-digest": result.PlanDigest, "io.kenogram.declaration-digest": result.DeclarationDigest}
	for k, v := range checks {
		if e.Labels[k] != v {
			return fmt.Errorf("label %s mismatch", k)
		}
	}
	if e.Memory != result.Plan.Resources.MemoryBytes || e.PIDs != result.Plan.Resources.PIDs {
		return fmt.Errorf("resource evidence mismatch")
	}
	if e.NanoCPUs != result.Plan.Resources.CPUs*1_000_000_000 {
		return fmt.Errorf("cpu evidence mismatch")
	}
	targets := map[string]bool{}
	for _, target := range e.MountTargets {
		targets[target] = true
	}
	for _, target := range result.Plan.Workspace {
		if !targets[target] {
			return fmt.Errorf("workspace mount %q missing", target)
		}
	}
	for _, mount := range result.Plan.Mounts {
		if !targets[mount.Target] {
			return fmt.Errorf("declared mount %q missing", mount.Target)
		}
	}
	return nil
}

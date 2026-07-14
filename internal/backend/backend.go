// Package backend invokes the supported rootless container runtime through exact argv.
package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idolum-ai/kenogram/internal/plan"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
	Start(context.Context, string, ...string) error
	Interactive(context.Context, string, ...string) error
}

// SignalCause records the signal that canceled an operation context so an
// interactive child can receive the same signal before bounded escalation.
type SignalCause struct {
	Signal os.Signal
}

func (e *SignalCause) Error() string { return fmt.Sprintf("received %s", e.Signal) }

type ExecRunner struct {
	InterruptGrace time.Duration
}

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
func (r ExecRunner) Interactive(ctx context.Context, name string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.Command(name, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	signalToForward := os.Signal(syscall.SIGTERM)
	var signalCause *SignalCause
	if errors.As(context.Cause(ctx), &signalCause) && signalCause.Signal != nil {
		signalToForward = signalCause.Signal
	}
	if err := command.Process.Signal(signalToForward); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = command.Process.Kill()
		return <-done
	}
	grace := r.InterruptGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = command.Process.Kill()
		return <-done
	}
}

type Podman struct {
	Runner         Runner
	Binary         string
	ReadProcStatus func(int) ([]byte, error)
	MountIdentity  func(int, string, string) (bool, error)
}

func New(r Runner) *Podman {
	if r == nil {
		r = ExecRunner{}
	}
	return &Podman{
		Runner: r,
		Binary: "podman",
		ReadProcStatus: func(pid int) ([]byte, error) {
			return os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		},
		MountIdentity: mountIdentity,
	}
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
	args := []string{"create", "--name", name, "--network", "none", "--ipc", "private", "--pid", "private", "--uts", "private", "--userns", "keep-id", "--image-volume", "ignore", "--hostname", result.Plan.World.Hostname, "--user", result.Plan.World.User, "--workdir", result.Plan.World.Workdir, "--cpus", strconv.FormatInt(result.Plan.Resources.CPUs, 10), "--memory", strconv.FormatInt(result.Plan.Resources.MemoryBytes, 10), "--pids-limit", strconv.FormatInt(result.Plan.Resources.PIDs, 10), "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--label", "io.kenogram.world=" + result.Plan.Name, "--label", "io.kenogram.generation=" + strconv.FormatInt(generation, 10), "--label", "io.kenogram.plan-digest=" + result.PlanDigest, "--label", "io.kenogram.declaration-digest=" + result.DeclarationDigest, "--env", "NO_PROXY=localhost,127.0.0.1"}
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
	// A declaration owns the world's process model. Explicitly replace any
	// image entrypoint so a base image cannot run bootstrap code before the
	// inert holder or reinterpret tail's arguments as its own command.
	args = append(args, "--entrypoint", "/usr/bin/tail", result.Plan.World.Base, "-f", "/dev/null")
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

// Exists observes whether an exact container name is present without treating
// absence as a command failure.
func (p *Podman) Exists(ctx context.Context, name string) (bool, error) {
	raw, err := p.Runner.Run(ctx, p.Binary, "ps", "--all", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
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
func (p *Podman) ExecOutput(ctx context.Context, name string, command []string) ([]byte, error) {
	args := append([]string{"exec", name}, command...)
	return p.Runner.Run(ctx, p.Binary, args...)
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
	IPCMode                string
	PIDMode                string
	UTSMode                string
	UserNSMode             string
	User                   string
	Hostname               string
	WorkingDir             string
	CapDrop                []string
	BoundingCaps           []string
	SecurityOpt            []string
	SeccompMode            int
	Devices                int
	UIDMap                 []IDMap
	GIDMap                 []IDMap
	Labels                 map[string]string
	Mounts                 []EvidenceMount
	Memory, NanoCPUs, PIDs int64
}
type IDMap struct {
	ContainerID int64
	HostID      int64
	Size        int64
}
type EvidenceMount struct {
	Source           string
	Destination      string
	RW               bool
	Mode             string
	Options          []string
	IdentityVerified bool
}
type inspectDocument struct {
	Name         string   `json:"Name"`
	BoundingCaps []string `json:"BoundingCaps"`
	State        struct {
		Running bool `json:"Running"`
		Pid     int  `json:"Pid"`
	} `json:"State"`
	IDMappings struct {
		UIDMap []struct {
			ContainerID int64 `json:"ContainerID"`
			HostID      int64 `json:"HostID"`
			Size        int64 `json:"Size"`
		} `json:"UidMap"`
		GIDMap []struct {
			ContainerID int64 `json:"ContainerID"`
			HostID      int64 `json:"HostID"`
			Size        int64 `json:"Size"`
		} `json:"GidMap"`
	} `json:"IDMappings"`
	Config struct {
		Labels     map[string]string `json:"Labels"`
		User       string            `json:"User"`
		Hostname   string            `json:"Hostname"`
		WorkingDir string            `json:"WorkingDir"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode string   `json:"NetworkMode"`
		IpcMode     string   `json:"IpcMode"`
		PidMode     string   `json:"PidMode"`
		UTSMode     string   `json:"UTSMode"`
		UsernsMode  string   `json:"UsernsMode"`
		Memory      int64    `json:"Memory"`
		NanoCPUs    int64    `json:"NanoCpus"`
		PidsLimit   int64    `json:"PidsLimit"`
		CapDrop     []string `json:"CapDrop"`
		SecurityOpt []string `json:"SecurityOpt"`
		Devices     []any    `json:"Devices"`
	} `json:"HostConfig"`
	Mounts []struct {
		Source      string   `json:"Source"`
		Destination string   `json:"Destination"`
		RW          bool     `json:"RW"`
		Mode        string   `json:"Mode"`
		Options     []string `json:"Options"`
	} `json:"Mounts"`
}

func (p *Podman) Inspect(ctx context.Context, name string) (Evidence, error) {
	raw, err := p.Runner.Run(ctx, p.Binary, "inspect", name)
	if err != nil {
		return Evidence{}, err
	}
	var docs []inspectDocument
	if err := json.Unmarshal(raw, &docs); err != nil {
		return Evidence{}, fmt.Errorf("decode podman inspect: %w", err)
	}
	if len(docs) != 1 {
		return Evidence{}, fmt.Errorf("decode podman inspect: got %d documents, want 1", len(docs))
	}
	d := docs[0]
	e := Evidence{Name: strings.TrimPrefix(d.Name, "/"), Running: d.State.Running, PID: d.State.Pid, NetworkMode: d.HostConfig.NetworkMode, IPCMode: d.HostConfig.IpcMode, PIDMode: d.HostConfig.PidMode, UTSMode: d.HostConfig.UTSMode, UserNSMode: d.HostConfig.UsernsMode, User: d.Config.User, Hostname: d.Config.Hostname, WorkingDir: d.Config.WorkingDir, CapDrop: d.HostConfig.CapDrop, BoundingCaps: d.BoundingCaps, SecurityOpt: d.HostConfig.SecurityOpt, Devices: len(d.HostConfig.Devices), Labels: d.Config.Labels, Memory: d.HostConfig.Memory, NanoCPUs: d.HostConfig.NanoCPUs, PIDs: d.HostConfig.PidsLimit}
	if e.Running {
		if e.PID <= 0 {
			return Evidence{}, fmt.Errorf("runtime holder PID is absent")
		}
		if e.BoundingCaps == nil {
			e.BoundingCaps, err = readProcBoundingCaps(e.PID)
			if err != nil {
				return Evidence{}, fmt.Errorf("read runtime capability evidence: %w", err)
			}
		}
		status, statusErr := p.ReadProcStatus(e.PID)
		if statusErr != nil {
			return Evidence{}, fmt.Errorf("read runtime seccomp evidence: %w", statusErr)
		}
		e.SeccompMode, err = parseProcSeccomp(status)
		if err != nil {
			return Evidence{}, fmt.Errorf("read runtime seccomp evidence: %w", err)
		}
	}
	for _, m := range d.Mounts {
		identity := false
		if e.Running {
			var identityErr error
			identity, identityErr = p.MountIdentity(e.PID, m.Source, m.Destination)
			if identityErr != nil {
				return Evidence{}, fmt.Errorf("verify mount identity at %q: %w", m.Destination, identityErr)
			}
		}
		e.Mounts = append(e.Mounts, EvidenceMount{Source: m.Source, Destination: m.Destination, RW: m.RW, Mode: m.Mode, Options: m.Options, IdentityVerified: identity})
	}
	for _, mapping := range d.IDMappings.UIDMap {
		e.UIDMap = append(e.UIDMap, IDMap{ContainerID: mapping.ContainerID, HostID: mapping.HostID, Size: mapping.Size})
	}
	for _, mapping := range d.IDMappings.GIDMap {
		e.GIDMap = append(e.GIDMap, IDMap{ContainerID: mapping.ContainerID, HostID: mapping.HostID, Size: mapping.Size})
	}
	if len(e.UIDMap) == 0 && e.PID > 0 {
		e.UIDMap, err = readProcIDMap(e.PID, "uid_map")
		if err != nil {
			return Evidence{}, fmt.Errorf("read runtime UID mapping evidence: %w", err)
		}
	}
	if len(e.GIDMap) == 0 && e.PID > 0 {
		e.GIDMap, err = readProcIDMap(e.PID, "gid_map")
		if err != nil {
			return Evidence{}, fmt.Errorf("read runtime GID mapping evidence: %w", err)
		}
	}
	return e, nil
}

func mountIdentity(pid int, source, target string) (bool, error) {
	if pid <= 0 || !filepath.IsAbs(target) {
		return false, fmt.Errorf("invalid mount identity request")
	}
	inside := filepath.Join("/proc", strconv.Itoa(pid), "root", strings.TrimPrefix(filepath.Clean(target), string(os.PathSeparator)))
	return sameFileIdentity(source, inside)
}

func sameFileIdentity(first, second string) (bool, error) {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, err
	}
	firstStat, firstOK := firstInfo.Sys().(*syscall.Stat_t)
	secondStat, secondOK := secondInfo.Sys().(*syscall.Stat_t)
	if !firstOK || !secondOK {
		return false, fmt.Errorf("filesystem identity is unavailable")
	}
	return firstStat.Dev == secondStat.Dev && firstStat.Ino == secondStat.Ino, nil
}

func readProcIDMap(pid int, name string) ([]IDMap, error) {
	if pid <= 0 || name != "uid_map" && name != "gid_map" {
		return nil, fmt.Errorf("invalid process mapping request")
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
	if err != nil {
		return nil, err
	}
	return parseIDMap(raw)
}

func parseIDMap(raw []byte) ([]IDMap, error) {
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	mappings := make([]IDMap, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid ID mapping line")
		}
		values := [3]int64{}
		for index, field := range fields {
			value, err := strconv.ParseInt(field, 10, 64)
			if err != nil || value < 0 {
				return nil, fmt.Errorf("invalid ID mapping value")
			}
			values[index] = value
		}
		if values[2] <= 0 {
			return nil, fmt.Errorf("invalid ID mapping size")
		}
		mappings = append(mappings, IDMap{ContainerID: values[0], HostID: values[1], Size: values[2]})
	}
	if len(mappings) == 0 {
		return nil, fmt.Errorf("empty ID mapping")
	}
	return mappings, nil
}

func readProcBoundingCaps(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid process capability request")
	}
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return nil, err
	}
	return parseProcBoundingCaps(raw)
}

func parseProcBoundingCaps(raw []byte) ([]string, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "CapBnd:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "CapBnd:"))
		if value == "" {
			return nil, fmt.Errorf("empty capability bounding set")
		}
		for _, character := range value {
			digit := character >= '0' && character <= '9'
			lower := character >= 'a' && character <= 'f'
			upper := character >= 'A' && character <= 'F'
			if !digit && !lower && !upper {
				return nil, fmt.Errorf("invalid capability bounding set")
			}
		}
		if strings.Trim(value, "0") == "" {
			return []string{}, nil
		}
		return []string{strings.ToLower(value)}, nil
	}
	return nil, fmt.Errorf("capability bounding set is absent")
}

func parseProcSeccomp(raw []byte) (int, error) {
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "Seccomp:") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Seccomp:")))
		if err != nil || value < 0 || value > 2 {
			return 0, fmt.Errorf("invalid seccomp mode")
		}
		return value, nil
	}
	return 0, fmt.Errorf("seccomp mode is absent")
}

func Verify(e Evidence, result plan.Result, generation int64, expectedMounts []Mount) error {
	if !e.Running {
		return fmt.Errorf("container is not running")
	}
	if e.NetworkMode != "none" {
		return fmt.Errorf("network mode is %q, want none", e.NetworkMode)
	}
	if e.IPCMode != "private" || e.PIDMode != "private" || e.UTSMode != "private" {
		return fmt.Errorf("namespace evidence mismatch: ipc=%q pid=%q uts=%q", e.IPCMode, e.PIDMode, e.UTSMode)
	}
	if e.UserNSMode == "host" || !mapsIdentity(e.UIDMap, int64(os.Getuid())) || !mapsIdentity(e.GIDMap, int64(os.Getgid())) {
		return fmt.Errorf("keep-id mapping evidence missing (mode %q)", e.UserNSMode)
	}
	if e.User != result.Plan.World.User {
		return fmt.Errorf("runtime user is %q, want %q", e.User, result.Plan.World.User)
	}
	if e.Hostname != result.Plan.World.Hostname || e.WorkingDir != result.Plan.World.Workdir {
		return fmt.Errorf("hostname/workdir evidence mismatch")
	}
	if e.BoundingCaps == nil || len(e.BoundingCaps) != 0 {
		return fmt.Errorf("capability bounding set is not observably empty")
	}
	if !containsPrefixFold(e.SecurityOpt, "no-new-privileges") {
		return fmt.Errorf("no-new-privileges evidence missing")
	}
	if containsPrefixFold(e.SecurityOpt, "seccomp=unconfined") {
		return fmt.Errorf("unconfined seccomp evidence")
	}
	if e.SeccompMode != 2 {
		return fmt.Errorf("seccomp filtering is not active (mode %d)", e.SeccompMode)
	}
	if e.Devices != 0 {
		return fmt.Errorf("unexpected device mappings: %d", e.Devices)
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
	targets := map[string]EvidenceMount{}
	for _, mount := range e.Mounts {
		if _, duplicate := targets[mount.Destination]; duplicate {
			return fmt.Errorf("duplicate runtime mount at %q", mount.Destination)
		}
		targets[mount.Destination] = mount
		lower := strings.ToLower(mount.Source + " " + mount.Destination)
		if strings.Contains(lower, "podman.sock") || strings.Contains(lower, "docker.sock") {
			return fmt.Errorf("runtime control socket mount detected at %q", mount.Destination)
		}
	}
	if len(targets) != len(expectedMounts) {
		return fmt.Errorf("runtime mount count is %d, want %d", len(targets), len(expectedMounts))
	}
	for _, mount := range expectedMounts {
		evidence, ok := targets[mount.Target]
		if !ok {
			return fmt.Errorf("expected mount %q missing", mount.Target)
		}
		if canonicalHostPath(evidence.Source) != canonicalHostPath(mount.Source) {
			return fmt.Errorf("mount %q source mismatch", mount.Target)
		}
		if evidence.RW != (mount.Mode == "rw") {
			return fmt.Errorf("mount %q mode mismatch", mount.Target)
		}
		if !evidence.IdentityVerified {
			return fmt.Errorf("mount %q source identity mismatch", mount.Target)
		}
		if !mountHasOption(evidence, "nodev") || !mountHasOption(evidence, "nosuid") {
			return fmt.Errorf("mount %q lacks nodev/nosuid evidence", mount.Target)
		}
		if mount.NoExec && !mountHasOption(evidence, "noexec") {
			return fmt.Errorf("mount %q lacks noexec evidence", mount.Target)
		}
	}
	return nil
}

func canonicalHostPath(path string) string {
	clean := filepath.Clean(path)
	if evaluated, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(evaluated)
	}
	return clean
}

func mapsIdentity(mappings []IDMap, id int64) bool {
	for _, mapping := range mappings {
		if mapping.Size > 0 && id >= mapping.HostID && id < mapping.HostID+mapping.Size {
			containerID := mapping.ContainerID + id - mapping.HostID
			if containerID == id {
				return true
			}
		}
	}
	return false
}

func mountHasOption(mount EvidenceMount, option string) bool {
	values := append([]string{mount.Mode}, mount.Options...)
	for _, value := range values {
		for _, field := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(field), option) {
				return true
			}
		}
	}
	return false
}

func containsPrefixFold(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix)) {
			return true
		}
	}
	return false
}

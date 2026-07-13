package backend

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/idolum-ai/kenogram/internal/version"
)

const (
	RuntimeEnvironment  = "KENOGRAM_RUNTIME"
	MachineEnvironment  = "KENOGRAM_CONTAINER_MACHINE"
	BinaryEnvironment   = "KENOGRAM_MACHINE_KENOGRAM"
	AppleMachineRuntime = "apple-container-machine"
)

var machineNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// AppleMachineRunner executes a Linux command in one user-selected Apple
// container machine. It deliberately does not own the machine lifecycle.
type AppleMachineRunner struct {
	Runner  Runner
	Machine string
}

func (r AppleMachineRunner) command(name string, args ...string) (string, []string) {
	forwarded := []string{"machine", "run", "-n", r.Machine, "--", "/usr/bin/env", "-u", RuntimeEnvironment, "-u", MachineEnvironment, "-u", BinaryEnvironment, "--", name}
	return "container", append(forwarded, args...)
}

func (r AppleMachineRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command, forwarded := r.command(name, args...)
	return r.Runner.Run(ctx, command, forwarded...)
}

func (r AppleMachineRunner) Start(ctx context.Context, name string, args ...string) error {
	command, forwarded := r.command(name, args...)
	return r.Runner.Start(ctx, command, forwarded...)
}

func (r AppleMachineRunner) Interactive(ctx context.Context, name string, args ...string) error {
	command, forwarded := r.command(name, args...)
	return r.Runner.Interactive(ctx, command, forwarded...)
}

// AppleMachineLauncher moves the complete Kenogram operation across the host
// boundary. Inner Kenogram therefore retains Linux /proc and network namespace
// access instead of trying to infer those properties from macOS.
type AppleMachineLauncher struct {
	Runner   Runner
	Machine  string
	Kenogram string
}

// AppleMachineFromEnvironment selects the explicit macOS transport. A nil
// launcher means the local rootless-Podman runtime remains selected.
func AppleMachineFromEnvironment(goos string, getenv func(string) string, runner Runner) (*AppleMachineLauncher, error) {
	selected := strings.TrimSpace(getenv(RuntimeEnvironment))
	if selected == "" {
		if goos != "linux" {
			return nil, fmt.Errorf("Kenogram runtime operations require Linux; on macOS select %s with %s", AppleMachineRuntime, RuntimeEnvironment)
		}
		return nil, nil
	}
	if selected == "podman" {
		if goos != "linux" {
			return nil, fmt.Errorf("the direct Podman runtime requires Linux, got %s", goos)
		}
		return nil, nil
	}
	if selected != AppleMachineRuntime {
		return nil, fmt.Errorf("unsupported %s %q (want podman or %s)", RuntimeEnvironment, selected, AppleMachineRuntime)
	}
	if goos != "darwin" {
		return nil, fmt.Errorf("%s requires macOS, got %s", AppleMachineRuntime, goos)
	}
	machine := strings.TrimSpace(getenv(MachineEnvironment))
	if !machineNamePattern.MatchString(machine) {
		return nil, fmt.Errorf("%s must be a 1-63 character machine name using letters, digits, dot, underscore, or hyphen", MachineEnvironment)
	}
	binary := strings.TrimSpace(getenv(BinaryEnvironment))
	if binary == "" {
		binary = "kenogram"
	}
	if strings.ContainsRune(binary, '\x00') || (!filepath.IsAbs(binary) && filepath.Base(binary) != binary) {
		return nil, fmt.Errorf("%s must be an absolute path or command name", BinaryEnvironment)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &AppleMachineLauncher{Runner: runner, Machine: machine, Kenogram: binary}, nil
}

// Preflight proves that the named machine exists, can execute the Linux
// Kenogram binary, and supplies the same rootless Podman prerequisites as a
// native Linux host. machine run boots a stopped machine by Apple's contract.
func (l *AppleMachineLauncher) Preflight(ctx context.Context) error {
	if _, err := l.Runner.Run(ctx, "container", "machine", "inspect", l.Machine); err != nil {
		return fmt.Errorf("inspect Apple container machine %q: %w", l.Machine, err)
	}
	machine := AppleMachineRunner{Runner: l.Runner, Machine: l.Machine}
	rawVersion, err := machine.Run(ctx, l.Kenogram, "version")
	if err != nil {
		return fmt.Errorf("run Linux Kenogram in Apple container machine %q: %w", l.Machine, err)
	}
	if inner, outer := strings.TrimSpace(string(rawVersion)), version.String(); inner != outer {
		return fmt.Errorf("Apple container machine %q Kenogram identity is %q, want %q", l.Machine, inner, outer)
	}
	if err := New(machine).Preflight(ctx); err != nil {
		return fmt.Errorf("Apple container machine %q Podman preflight: %w", l.Machine, err)
	}
	return nil
}

// Launch forwards argv without a shell. It does not stop or remove the machine
// after the inner operation; the operator retains that persistent lifecycle.
func (l *AppleMachineLauncher) Launch(ctx context.Context, args []string) error {
	if err := l.Preflight(ctx); err != nil {
		return err
	}
	machine := AppleMachineRunner{Runner: l.Runner, Machine: l.Machine}
	return machine.Interactive(ctx, l.Kenogram, args...)
}

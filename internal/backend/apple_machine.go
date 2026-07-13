package backend

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/idolum-ai/kenogram/internal/version"
)

const (
	RuntimeEnvironment  = "KENOGRAM_RUNTIME"
	MachineEnvironment  = "KENOGRAM_CONTAINER_MACHINE"
	BinaryEnvironment   = "KENOGRAM_MACHINE_KENOGRAM"
	AppleMachineRuntime = "apple-container-machine"

	// AppleMachineBridgeCommand identifies the versioned, shell-inert argv
	// envelope decoded by the Linux Kenogram inside the machine.
	AppleMachineBridgeCommand = "_apple-machine-forward-v1"
)

var machineNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
var machineBinaryPattern = regexp.MustCompile(`^[A-Za-z0-9/][A-Za-z0-9_./-]*$`)
var encodedArgumentPattern = regexp.MustCompile(`^a[A-Za-z0-9_-]*$`)

// RemoteExitError distinguishes the final Linux Kenogram's status from a
// launcher or preflight failure. The outer CLI returns Code without adding a
// second, misleading transport error.
type RemoteExitError struct {
	Code int
}

func (e *RemoteExitError) Error() string {
	return fmt.Sprintf("remote Kenogram exited with status %d", e.Code)
}

// EncodeAppleMachineArguments maps arbitrary argv bytes into nonempty tokens
// from a shell-inert alphabet. Apple's machine init currently executes an
// explicit command through a shell, so raw user arguments must never cross
// that boundary.
func EncodeAppleMachineArguments(args []string) []string {
	encoded := make([]string, len(args))
	for i, arg := range args {
		encoded[i] = "a" + base64.RawURLEncoding.EncodeToString([]byte(arg))
	}
	return encoded
}

// DecodeAppleMachineArguments accepts only the canonical v1 envelope.
func DecodeAppleMachineArguments(encoded []string) ([]string, error) {
	args := make([]string, len(encoded))
	for i, token := range encoded {
		if !encodedArgumentPattern.MatchString(token) {
			return nil, fmt.Errorf("argument %d is not a canonical Apple machine envelope", i)
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "a"))
		if err != nil || "a"+base64.RawURLEncoding.EncodeToString(raw) != token {
			return nil, fmt.Errorf("argument %d is not a canonical Apple machine envelope", i)
		}
		args[i] = string(raw)
	}
	return args, nil
}

// AppleMachineRunner executes a Linux command in one user-selected Apple
// container machine. It deliberately does not own the machine lifecycle.
type AppleMachineRunner struct {
	Runner  Runner
	Machine string
}

func (r AppleMachineRunner) command(interactive, tty bool, name string, args ...string) (string, []string) {
	forwarded := []string{"machine", "run", "-n", r.Machine}
	if interactive {
		forwarded = append(forwarded, "--interactive")
	}
	if tty {
		forwarded = append(forwarded, "--tty")
	}
	forwarded = append(forwarded, "--", "/usr/bin/env", "-u", RuntimeEnvironment, "-u", MachineEnvironment, "-u", BinaryEnvironment, "--", name)
	return "container", append(forwarded, args...)
}

func (r AppleMachineRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	command, forwarded := r.command(false, false, name, args...)
	return r.Runner.Run(ctx, command, forwarded...)
}

func (r AppleMachineRunner) Start(ctx context.Context, name string, args ...string) error {
	command, forwarded := r.command(false, false, name, args...)
	return r.Runner.Start(ctx, command, forwarded...)
}

func (r AppleMachineRunner) Interactive(ctx context.Context, name string, args ...string) error {
	return r.interactive(ctx, false, name, args...)
}

func (r AppleMachineRunner) interactive(ctx context.Context, tty bool, name string, args ...string) error {
	command, forwarded := r.command(true, tty, name, args...)
	return r.Runner.Interactive(ctx, command, forwarded...)
}

// AppleMachineLauncher moves the complete Kenogram operation across the host
// boundary. Inner Kenogram therefore retains Linux /proc and network namespace
// access instead of trying to infer those properties from macOS.
type AppleMachineLauncher struct {
	Runner   Runner
	Machine  string
	Kenogram string
	Terminal bool
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
	if binary == "/" || !machineBinaryPattern.MatchString(binary) || filepath.Clean(binary) != binary || (!filepath.IsAbs(binary) && filepath.Base(binary) != binary) {
		return nil, fmt.Errorf("%s must be a shell-inert absolute path or command name using letters, digits, dot, underscore, slash, or hyphen", BinaryEnvironment)
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &AppleMachineLauncher{Runner: runner, Machine: machine, Kenogram: binary, Terminal: stdinIsTerminal()}, nil
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
		return fmt.Errorf("Apple container machine %q Kenogram version report is %q, want %q", l.Machine, inner, outer)
	}
	if err := New(machine).Preflight(ctx); err != nil {
		return fmt.Errorf("Apple container machine %q Podman preflight: %w", l.Machine, err)
	}
	return nil
}

// Launch forwards an encoded argv envelope through Apple's shell-mediated
// machine boundary. It does not stop or remove the machine after the inner
// operation; the operator retains that persistent lifecycle.
func (l *AppleMachineLauncher) Launch(ctx context.Context, args []string) error {
	wantsTerminal := appleOperationWantsTerminal(args)
	if wantsTerminal && len(args) > 0 && args[0] == "enter" && !l.Terminal {
		return fmt.Errorf("kenogram enter through an Apple container machine requires a terminal")
	}
	if err := l.Preflight(ctx); err != nil {
		return err
	}
	machine := AppleMachineRunner{Runner: l.Runner, Machine: l.Machine}
	forwarded := append([]string{AppleMachineBridgeCommand}, EncodeAppleMachineArguments(args)...)
	err := machine.interactive(ctx, l.Terminal && wantsTerminal, l.Kenogram, forwarded...)
	if err == nil {
		return nil
	}
	var exitError interface{ ExitCode() int }
	if errors.As(err, &exitError) {
		code := exitError.ExitCode()
		if code > 0 {
			return &RemoteExitError{Code: code}
		}
	}
	var processExit *exec.ExitError
	if errors.As(err, &processExit) {
		if status, ok := processExit.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return &RemoteExitError{Code: 128 + int(status.Signal())}
		}
	}
	return err
}

func appleOperationWantsTerminal(args []string) bool {
	if len(args) == 0 || hasAppleArgument(args[1:], "--help", "-h") {
		return false
	}
	switch args[0] {
	case "enter":
		return true
	case "up":
		return !hasAppleArgument(args[1:], "--yes", "--yes=true", "--dry-run", "--dry-run=true")
	default:
		return false
	}
}

func hasAppleArgument(args []string, values ...string) bool {
	for _, arg := range args {
		for _, value := range values {
			if arg == value {
				return true
			}
		}
	}
	return false
}

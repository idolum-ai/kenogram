package backend

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/idolum-ai/kenogram/internal/version"
)

type scriptedRunner struct {
	calls          []call
	fail           int
	versionOutput  string
	interactiveErr error
}

func (r *scriptedRunner) record(name string, args ...string) error {
	r.calls = append(r.calls, call{name: name, args: append([]string{}, args...)})
	if r.fail > 0 && len(r.calls) == r.fail {
		return errors.New("fixture failure")
	}
	return nil
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if err := r.record(name, args...); err != nil {
		return nil, err
	}
	if name == "container" && len(args) > 13 && args[13] == "podman" {
		return []byte(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}}}`), nil
	}
	if name == "container" && len(args) > 14 && args[len(args)-1] == "version" {
		if r.versionOutput != "" {
			return []byte(r.versionOutput), nil
		}
		return []byte(version.String() + "\n"), nil
	}
	return []byte("ok"), nil
}

func (r *scriptedRunner) Start(_ context.Context, name string, args ...string) error {
	return r.record(name, args...)
}

func (r *scriptedRunner) Interactive(_ context.Context, name string, args ...string) error {
	if err := r.record(name, args...); err != nil {
		return err
	}
	return r.interactiveErr
}

func TestAppleMachineLaunchUsesShellInertArgvEnvelope(t *testing.T) {
	runner := &scriptedRunner{}
	values := map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "proof-machine", BinaryEnvironment: "/opt/kenogram/bin/kenogram"}
	launcher, err := AppleMachineFromEnvironment("darwin", func(key string) string { return values[key] }, runner)
	if err != nil {
		t.Fatal(err)
	}
	launcher.Terminal = false
	args := []string{"up", "--yes", "/home/proof/world $HOME 'quoted' \"double\" $(touch /tmp/no) && false\n*.toml", "", "café"}
	if err := launcher.Launch(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	prefix := []string{"machine", "run", "-n", "proof-machine", "--", "/usr/bin/env", "-u", RuntimeEnvironment, "-u", MachineEnvironment, "-u", BinaryEnvironment, "--"}
	interactivePrefix := []string{"machine", "run", "-n", "proof-machine", "--interactive", "--", "/usr/bin/env", "-u", RuntimeEnvironment, "-u", MachineEnvironment, "-u", BinaryEnvironment, "--"}
	encoded := EncodeAppleMachineArguments(args)
	want := []call{
		{name: "container", args: []string{"machine", "inspect", "proof-machine"}},
		{name: "container", args: append(append([]string{}, prefix...), "/opt/kenogram/bin/kenogram", "version")},
		{name: "container", args: append(append([]string{}, prefix...), "podman", "info", "--format", "json")},
		{name: "container", args: append(append(append([]string{}, interactivePrefix...), "/opt/kenogram/bin/kenogram", AppleMachineBridgeCommand), encoded...)},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
	decoded, err := DecodeAppleMachineArguments(encoded)
	if err != nil || !reflect.DeepEqual(decoded, args) {
		t.Fatalf("decoded=%q err=%v, want %q", decoded, err, args)
	}
	for _, token := range encoded {
		if !encodedArgumentPattern.MatchString(token) {
			t.Fatalf("encoded token is not shell-inert: %q", token)
		}
	}
	for _, call := range runner.calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "machine create") || strings.Contains(joined, "machine stop") || strings.Contains(joined, "machine rm") {
			t.Fatalf("launcher took ownership of persistent machine lifecycle: %s", joined)
		}
	}
}

func TestAppleMachineEnterRequestsStdinAndTTY(t *testing.T) {
	runner := &scriptedRunner{}
	launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram", Terminal: true}
	if err := launcher.Launch(context.Background(), []string{"enter", "world"}); err != nil {
		t.Fatal(err)
	}
	final := runner.calls[len(runner.calls)-1].args
	wantPrefix := []string{"machine", "run", "-n", "proof", "--interactive", "--tty", "--"}
	if len(final) < len(wantPrefix) || !reflect.DeepEqual(final[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("final argv = %q, want prefix %q", final, wantPrefix)
	}
}

func TestAppleMachineTTYPolicyPreservesMachineReadableOutput(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		wantTTY bool
	}{
		{name: "up confirmation", args: []string{"up", "world.toml"}, wantTTY: true},
		{name: "confirmed up", args: []string{"up", "--yes", "world.toml"}},
		{name: "dry run", args: []string{"up", "--dry-run", "world.toml"}},
		{name: "JSON status", args: []string{"status", "--json", "world"}},
		{name: "connect stream", args: []string{"connect", "world", "ssh"}},
		{name: "enter help", args: []string{"enter", "--help"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{}
			launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram", Terminal: true}
			if err := launcher.Launch(context.Background(), test.args); err != nil {
				t.Fatal(err)
			}
			final := runner.calls[len(runner.calls)-1].args
			gotTTY := false
			for _, arg := range final {
				if arg == "--tty" {
					gotTTY = true
				}
			}
			if gotTTY != test.wantTTY {
				t.Fatalf("argv=%q tty=%t, want %t", final, gotTTY, test.wantTTY)
			}
		})
	}
}

func TestAppleMachineEnterRejectsNonTerminalBeforePreflight(t *testing.T) {
	runner := &scriptedRunner{}
	launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram"}
	err := launcher.Launch(context.Background(), []string{"enter", "world"})
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") || len(runner.calls) != 0 {
		t.Fatalf("err=%v calls=%#v", err, runner.calls)
	}
}

func TestAppleMachineEnvelopeRejectsNonCanonicalTokens(t *testing.T) {
	for _, encoded := range [][]string{{""}, {"YQ"}, {"a="}, {"a$HOME"}, {"aZh"}} {
		if _, err := DecodeAppleMachineArguments(encoded); err == nil {
			t.Fatalf("accepted noncanonical envelope %q", encoded)
		}
	}
}

func FuzzAppleMachineArgumentEnvelope(f *testing.F) {
	f.Add("plain", "")
	f.Add("$HOME $(touch /tmp/no) && false\n*.toml", "café")
	f.Fuzz(func(t *testing.T, first, second string) {
		want := []string{first, second}
		encoded := EncodeAppleMachineArguments(want)
		for _, token := range encoded {
			if !encodedArgumentPattern.MatchString(token) {
				t.Fatalf("non-inert token %q", token)
			}
		}
		got, err := DecodeAppleMachineArguments(encoded)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded=%q err=%v, want %q", got, err, want)
		}
	})
}

type exitCodeError int

func (e exitCodeError) Error() string { return "remote exit" }
func (e exitCodeError) ExitCode() int { return int(e) }

func TestAppleMachinePreservesFinalExitCode(t *testing.T) {
	runner := &scriptedRunner{interactiveErr: exitCodeError(2)}
	launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram"}
	err := launcher.Launch(context.Background(), []string{"status"})
	var remote *RemoteExitError
	if !errors.As(err, &remote) || remote.Code != 2 {
		t.Fatalf("err=%#v", err)
	}
}

func TestAppleMachinePreflightFailsBeforeUserCommand(t *testing.T) {
	for fail := 1; fail <= 3; fail++ {
		runner := &scriptedRunner{fail: fail}
		launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram"}
		err := launcher.Launch(context.Background(), []string{"destroy", "--yes", "world"})
		if err == nil || len(runner.calls) != fail {
			t.Fatalf("fail=%d err=%v calls=%#v", fail, err, runner.calls)
		}
	}
}

func TestAppleMachineRejectsDifferentKenogramVersionReport(t *testing.T) {
	runner := &scriptedRunner{versionOutput: "kenogram old commit=wrong date=wrong go=wrong"}
	launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram"}
	err := launcher.Launch(context.Background(), []string{"status", "world"})
	if err == nil || !strings.Contains(err.Error(), "version report") || len(runner.calls) != 2 {
		t.Fatalf("err=%v calls=%#v", err, runner.calls)
	}
}

func TestAppleMachineSelectionFailsClosed(t *testing.T) {
	tests := []struct {
		goos    string
		values  map[string]string
		wantErr bool
	}{
		{goos: "linux", values: map[string]string{}, wantErr: false},
		{goos: "darwin", values: map[string]string{}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: "podman"}, wantErr: true},
		{goos: "linux", values: map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "proof"}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: AppleMachineRuntime}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "../proof"}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "proof", BinaryEnvironment: "relative/path"}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "proof", BinaryEnvironment: "/opt/kenogram with spaces/bin/kenogram"}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "proof", BinaryEnvironment: "/opt/$HOME/kenogram"}, wantErr: true},
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: "docker", MachineEnvironment: "proof"}, wantErr: true},
	}
	for _, test := range tests {
		launcher, err := AppleMachineFromEnvironment(test.goos, func(key string) string { return test.values[key] }, &scriptedRunner{})
		if (err != nil) != test.wantErr {
			t.Fatalf("goos=%s values=%v launcher=%v err=%v", test.goos, test.values, launcher, err)
		}
	}
}

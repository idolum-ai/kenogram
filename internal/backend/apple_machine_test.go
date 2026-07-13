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
	calls         []call
	fail          int
	versionOutput string
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
	return r.record(name, args...)
}

func TestAppleMachineLaunchExactArgv(t *testing.T) {
	runner := &scriptedRunner{}
	values := map[string]string{RuntimeEnvironment: AppleMachineRuntime, MachineEnvironment: "proof-machine", BinaryEnvironment: "/opt/kenogram/bin/kenogram"}
	launcher, err := AppleMachineFromEnvironment("darwin", func(key string) string { return values[key] }, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Launch(context.Background(), []string{"up", "--yes", "/home/proof/world.toml"}); err != nil {
		t.Fatal(err)
	}
	prefix := []string{"machine", "run", "-n", "proof-machine", "--", "/usr/bin/env", "-u", RuntimeEnvironment, "-u", MachineEnvironment, "-u", BinaryEnvironment, "--"}
	want := []call{
		{name: "container", args: []string{"machine", "inspect", "proof-machine"}},
		{name: "container", args: append(append([]string{}, prefix...), "/opt/kenogram/bin/kenogram", "version")},
		{name: "container", args: append(append([]string{}, prefix...), "podman", "info", "--format", "json")},
		{name: "container", args: append(append([]string{}, prefix...), "/opt/kenogram/bin/kenogram", "up", "--yes", "/home/proof/world.toml")},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "machine create") || strings.Contains(joined, "machine stop") || strings.Contains(joined, "machine rm") {
			t.Fatalf("launcher took ownership of persistent machine lifecycle: %s", joined)
		}
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

func TestAppleMachineRejectsDifferentKenogramIdentity(t *testing.T) {
	runner := &scriptedRunner{versionOutput: "kenogram old commit=wrong date=wrong go=wrong"}
	launcher := &AppleMachineLauncher{Runner: runner, Machine: "proof", Kenogram: "kenogram"}
	err := launcher.Launch(context.Background(), []string{"status", "world"})
	if err == nil || !strings.Contains(err.Error(), "identity") || len(runner.calls) != 2 {
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
		{goos: "darwin", values: map[string]string{RuntimeEnvironment: "docker", MachineEnvironment: "proof"}, wantErr: true},
	}
	for _, test := range tests {
		launcher, err := AppleMachineFromEnvironment(test.goos, func(key string) string { return test.values[key] }, &scriptedRunner{})
		if (err != nil) != test.wantErr {
			t.Fatalf("goos=%s values=%v launcher=%v err=%v", test.goos, test.values, launcher, err)
		}
	}
}

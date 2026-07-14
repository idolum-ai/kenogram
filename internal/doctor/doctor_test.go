package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeReportsReadyHostAndEntrySurfaces(t *testing.T) {
	state := t.TempDir()
	p := Probe{
		GOOS: "linux",
		LookPath: func(name string) (string, error) {
			return "/usr/bin/" + name, nil
		},
		ReadFile: func(path string) ([]byte, error) {
			if path != "/sys/fs/cgroup/cgroup.controllers" {
				t.Fatalf("unexpected read: %s", path)
			}
			return []byte("cpu memory pids\n"), nil
		},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "/usr/bin/podman" {
				t.Fatalf("unexpected command: %s %v", name, args)
			}
			switch strings.Join(args, " ") {
			case "unshare true":
				return nil, nil
			case "info --format json":
				return podmanInfo(state), nil
			default:
				t.Fatalf("unexpected command: %s %v", name, args)
				return nil, nil
			}
		},
		Access: func(path string) error {
			if path != state {
				t.Fatalf("unexpected access path: %s", path)
			}
			return nil
		},
		DiskFree: func(path string) (uint64, error) {
			if path != state {
				t.Fatalf("unexpected disk path: %s", path)
			}
			return 12 * bytesPerGiB, nil
		},
		StateDir: filepath.Join(state, "worlds"),
	}
	report := p.Inspect(context.Background())
	if !report.Ready {
		t.Fatalf("report = %#v", report)
	}
	if got, want := len(report.Checks), 13; got != want {
		t.Fatalf("checks = %d, want %d: %#v", got, want, report.Checks)
	}
	if checkByName(t, report, "repair_entry_surface").Status != "info" || checkByName(t, report, "normal_entry_surface").Status != "info" {
		t.Fatalf("entry observations = %#v", report.Checks)
	}
}

func TestProbeRetainsStableCheckSetAcrossFailures(t *testing.T) {
	missing := errors.New("not found")
	p := Probe{
		GOOS: "darwin",
		LookPath: func(string) (string, error) {
			return "", missing
		},
		ReadFile: func(string) ([]byte, error) {
			return nil, missing
		},
		Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("run called without podman")
			return nil, nil
		},
		DiskFree: func(string) (uint64, error) {
			return 0, errors.New("unavailable")
		},
		StateDir: t.TempDir(),
	}
	report := p.Inspect(context.Background())
	if report.Ready {
		t.Fatal("unready host reported ready")
	}
	if got, want := len(report.Checks), 13; got != want {
		t.Fatalf("checks = %d, want stable set of %d: %#v", got, want, report.Checks)
	}
	for _, want := range []string{"operating_system", "cgroups_v2", "podman_executable", "podman_user_namespace", "podman_info", "podman_rootless", "podman_cgroups_v2", "subordinate_ids", "nsenter_executable", "state_storage", "container_storage"} {
		if checkByName(t, report, want).Status != "fail" {
			t.Fatalf("%s did not fail: %#v", want, report.Checks)
		}
	}
}

func TestProbeRequiresEveryResourceController(t *testing.T) {
	p := readyProbe(t)
	p.ReadFile = func(string) ([]byte, error) { return []byte("cpu pids"), nil }
	check := checkByName(t, p.Inspect(context.Background()), "cgroups_v2")
	if check.Status != "fail" || !strings.Contains(check.Observed, "missing=memory") {
		t.Fatalf("cgroups_v2 = %#v", check)
	}
}

func TestProbeMarksPodmanDependentChecksWhenInfoFails(t *testing.T) {
	p := readyProbe(t)
	p.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("runtime unavailable")
	}
	report := p.Inspect(context.Background())
	for _, name := range []string{"podman_rootless", "podman_cgroups_v2", "subordinate_ids", "container_storage"} {
		check := checkByName(t, report, name)
		if check.Status != "fail" || !strings.Contains(check.Observed, "podman_info failed") {
			t.Fatalf("%s = %#v", name, check)
		}
	}
}

func TestProbeRequiresPodmanUserNamespace(t *testing.T) {
	p := readyProbe(t)
	p.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "unshare true":
			return nil, errors.New("newuidmap unavailable")
		case "info --format json":
			return podmanInfo(p.StateDir), nil
		default:
			t.Fatalf("unexpected args: %v", args)
			return nil, nil
		}
	}
	check := checkByName(t, p.Inspect(context.Background()), "podman_user_namespace")
	if check.Status != "fail" || !strings.Contains(check.Observed, "newuidmap unavailable") {
		t.Fatalf("podman_user_namespace = %#v", check)
	}
}

func TestProbeDoesNotAscendPastStateStatError(t *testing.T) {
	p := readyProbe(t)
	denied := errors.New("permission denied")
	p.StateDir = "/denied/worlds"
	p.Stat = func(path string) (os.FileInfo, error) {
		if path == p.StateDir {
			return nil, denied
		}
		if path == "/denied" {
			t.Fatalf("stat ascended past the permission error to %q", path)
		}
		return os.Stat(path)
	}
	check := checkByName(t, p.Inspect(context.Background()), "state_storage")
	if check.Status != "fail" || !strings.Contains(check.Observed, denied.Error()) {
		t.Fatalf("state_storage = %#v", check)
	}
}

func TestProbeRequiresEffectiveStateAccess(t *testing.T) {
	p := readyProbe(t)
	p.Access = func(path string) error {
		if path == p.StateDir {
			return errors.New("read-only filesystem")
		}
		return nil
	}
	check := checkByName(t, p.Inspect(context.Background()), "state_storage")
	if check.Status != "fail" || !strings.Contains(check.Observed, "read-only filesystem") {
		t.Fatalf("state_storage = %#v", check)
	}
}

func TestProbeRejectsRegularFileAsStorage(t *testing.T) {
	p := readyProbe(t)
	stateFile := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(stateFile, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	p.StateDir = stateFile
	check := checkByName(t, p.Inspect(context.Background()), "state_storage")
	if check.Status != "fail" || !strings.Contains(check.Observed, "not a directory") {
		t.Fatalf("state_storage = %#v", check)
	}
}

func readyProbe(t *testing.T) Probe {
	t.Helper()
	state := t.TempDir()
	return Probe{
		GOOS:     "linux",
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		ReadFile: func(string) ([]byte, error) { return []byte("cpu memory pids"), nil },
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "unshare true":
				return nil, nil
			case "info --format json":
				return podmanInfo(state), nil
			default:
				t.Fatalf("unexpected args: %v", args)
				return nil, nil
			}
		},
		Access:   func(string) error { return nil },
		DiskFree: func(string) (uint64, error) { return bytesPerGiB, nil },
		StateDir: state,
	}
}

func podmanInfo(graphRoot string) []byte {
	return []byte(fmt.Sprintf(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}} ,"store":{"graphRoot":%q}}`, graphRoot))
}

func checkByName(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("missing check %q: %#v", name, report.Checks)
	return Check{}
}

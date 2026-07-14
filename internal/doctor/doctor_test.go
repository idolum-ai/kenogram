package doctor

import (
	"context"
	"errors"
	"fmt"
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
			if name != "/usr/bin/podman" || strings.Join(args, " ") != "info --format json" {
				t.Fatalf("unexpected command: %s %v", name, args)
			}
			return []byte(fmt.Sprintf(`{"host":{"security":{"rootless":true},"cgroupVersion":"v2","idMappings":{"uidmap":[{"size":65536}],"gidmap":[{"size":65536}]}},"store":{"graphRoot":%q}}`, state)), nil
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
	if got, want := len(report.Checks), 12; got != want {
		t.Fatalf("checks = %d, want %d: %#v", got, want, report.Checks)
	}
	if report.Checks[10].Status != "info" || report.Checks[11].Status != "info" {
		t.Fatalf("entry observations = %#v", report.Checks[10:])
	}
}

func TestProbeRetainsIndependentFailures(t *testing.T) {
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
	failures := map[string]bool{}
	for _, check := range report.Checks {
		if check.Status == "fail" {
			failures[check.Name] = true
		}
	}
	for _, want := range []string{"operating_system", "cgroups_v2", "podman_executable", "podman_info", "nsenter_executable", "state_storage"} {
		if !failures[want] {
			t.Fatalf("missing failure %q: %#v", want, report.Checks)
		}
	}
}

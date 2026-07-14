// Package doctor observes the host prerequisites required by Kenogram.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const bytesPerGiB = uint64(1024 * 1024 * 1024)

// Check is one stable, machine-readable host observation.
type Check struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Observed    string `json:"observed"`
	Remediation string `json:"remediation,omitempty"`
}

// Report retains every observation even when one prerequisite fails.
type Report struct {
	Ready  bool    `json:"ready"`
	Checks []Check `json:"checks"`
}

type mapItem struct {
	Size int64 `json:"size"`
}

// Probe makes host observations injectable without teaching tests about the
// machine on which they happen to run.
type Probe struct {
	GOOS     string
	LookPath func(string) (string, error)
	ReadFile func(string) ([]byte, error)
	Run      func(context.Context, string, ...string) ([]byte, error)
	DiskFree func(string) (uint64, error)
	StateDir string
}

// Inspect observes the real host without changing it.
func Inspect(ctx context.Context, stateDir string) Report {
	p := Probe{
		GOOS:     runtime.GOOS,
		LookPath: exec.LookPath,
		ReadFile: os.ReadFile,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return out, nil
		},
		DiskFree: diskFree,
		StateDir: stateDir,
	}
	return p.Inspect(ctx)
}

// Inspect executes all independent checks and never stops at the first failure.
func (p Probe) Inspect(ctx context.Context) Report {
	checks := []Check{}
	add := func(name string, ok bool, observed, remediation string) {
		status := "pass"
		if ok {
			remediation = ""
		} else {
			status = "fail"
		}
		checks = append(checks, Check{Name: name, Status: status, Observed: observed, Remediation: remediation})
	}

	add("operating_system", p.GOOS == "linux", p.GOOS, "run Kenogram on Linux, or select an operator-managed Apple container machine")
	controllers, cgroupErr := p.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	add("cgroups_v2", cgroupErr == nil, observation(controllers, cgroupErr), "enable the unified cgroups v2 hierarchy")

	podmanPath, podmanPathErr := p.LookPath("podman")
	add("podman_executable", podmanPathErr == nil, observation([]byte(podmanPath), podmanPathErr), "install rootless Podman")

	var info struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
			CgroupVersion string `json:"cgroupVersion"`
			IDMappings    struct {
				UIDMap []mapItem `json:"uidmap"`
				GIDMap []mapItem `json:"gidmap"`
			} `json:"idMappings"`
		} `json:"host"`
		Store struct {
			GraphRoot string `json:"graphRoot"`
		} `json:"store"`
	}
	podmanInfoOK := false
	if podmanPathErr == nil {
		raw, runErr := p.Run(ctx, podmanPath, "info", "--format", "json")
		if runErr != nil {
			add("podman_info", false, runErr.Error(), "initialize and repair the rootless Podman store")
		} else if err := json.Unmarshal(raw, &info); err != nil {
			add("podman_info", false, "invalid JSON: "+err.Error(), "use a Podman version that supports `podman info --format json`")
		} else {
			podmanInfoOK = true
			add("podman_info", true, "available", "")
		}
	} else {
		add("podman_info", false, "podman executable unavailable", "install rootless Podman")
	}
	if podmanInfoOK {
		add("podman_rootless", info.Host.Security.Rootless, fmt.Sprintf("rootless=%t", info.Host.Security.Rootless), "configure Podman for the current unprivileged user; do not run Kenogram as root")
		add("podman_cgroups_v2", info.Host.CgroupVersion == "v2", info.Host.CgroupVersion, "configure Podman to use cgroups v2")
		uidSize, gidSize := mappingSize(info.Host.IDMappings.UIDMap), mappingSize(info.Host.IDMappings.GIDMap)
		add("subordinate_ids", uidSize > 1 && gidSize > 1, fmt.Sprintf("uid_range=%d gid_range=%d", uidSize, gidSize), "configure /etc/subuid and /etc/subgid for the current user, then migrate the rootless Podman store if required")
	}

	nsenterPath, nsenterErr := p.LookPath("nsenter")
	add("nsenter_executable", nsenterErr == nil, observation([]byte(nsenterPath), nsenterErr), "install util-linux so Kenogram can hand a listener into the world network namespace")

	statePath := nearestExisting(p.StateDir)
	stateFree, stateErr := p.DiskFree(statePath)
	add("state_storage", stateErr == nil && stateFree > 0, diskObservation(statePath, stateFree, stateErr), "make the Kenogram state filesystem available and writable")
	if podmanInfoOK {
		storePath := nearestExisting(info.Store.GraphRoot)
		storeFree, storeErr := p.DiskFree(storePath)
		add("container_storage", storeErr == nil && storeFree > 0, diskObservation(storePath, storeFree, storeErr), "free space in the rootless Podman graph root; composition guides state their larger lane-specific floors")
	}

	checks = append(checks,
		Check{Name: "repair_entry_surface", Status: "info", Observed: "each world image needs /bin/sh"},
		Check{Name: "normal_entry_surface", Status: "info", Observed: "normal entry additionally needs /usr/bin/tmux and a main session"},
	)
	ready := true
	for _, check := range checks {
		if check.Status == "fail" {
			ready = false
		}
	}
	return Report{Ready: ready, Checks: checks}
}

func mappingSize(items []mapItem) int64 {
	var size int64
	for _, item := range items {
		size += item.Size
	}
	return size
}

func observation(value []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return "available"
	}
	return trimmed
}

func nearestExisting(path string) string {
	if path == "" {
		return "."
	}
	path = filepath.Clean(path)
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func diskObservation(path string, free uint64, err error) string {
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	return fmt.Sprintf("%s: %.1f GiB free", path, float64(free)/float64(bytesPerGiB))
}

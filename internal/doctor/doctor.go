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
	Stat     func(string) (os.FileInfo, error)
	Access   func(string) error
	DiskFree func(string) (uint64, error)
	StateDir string
}

// Inspect observes the real host without changing Kenogram worlds or state.
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
		Stat:     os.Stat,
		Access:   directoryAccess,
		DiskFree: diskFree,
		StateDir: stateDir,
	}
	return p.Inspect(ctx)
}

// Inspect executes all independent checks and never stops at the first failure.
func (p Probe) Inspect(ctx context.Context) Report {
	stat := p.Stat
	if stat == nil {
		stat = os.Stat
	}
	access := p.Access
	if access == nil {
		access = directoryAccess
	}
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
	controllersOK, controllerObservation := requiredControllers(controllers, cgroupErr)
	add("cgroups_v2", controllersOK, controllerObservation, "enable cgroups v2 with the cpu, memory, and pids controllers available")

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
	if podmanPathErr == nil {
		_, unshareErr := p.Run(ctx, podmanPath, "unshare", "true")
		add("podman_user_namespace", unshareErr == nil, observation(nil, unshareErr), "repair rootless Podman user namespaces and subordinate UID/GID mappings")
	} else {
		add("podman_user_namespace", false, "not observed: podman executable unavailable", "install rootless Podman, then rerun doctor")
	}
	if podmanInfoOK {
		add("podman_rootless", info.Host.Security.Rootless, fmt.Sprintf("rootless=%t", info.Host.Security.Rootless), "configure Podman for the current unprivileged user; do not run Kenogram as root")
		add("podman_cgroups_v2", info.Host.CgroupVersion == "v2", info.Host.CgroupVersion, "configure Podman to use cgroups v2")
		uidSize, gidSize := mappingSize(info.Host.IDMappings.UIDMap), mappingSize(info.Host.IDMappings.GIDMap)
		add("subordinate_ids", uidSize > 1 && gidSize > 1, fmt.Sprintf("uid_range=%d gid_range=%d", uidSize, gidSize), "configure /etc/subuid and /etc/subgid for the current user, then migrate the rootless Podman store if required")
	} else {
		add("podman_rootless", false, "not observed: podman_info failed", "repair podman_info, then rerun doctor")
		add("podman_cgroups_v2", false, "not observed: podman_info failed", "repair podman_info, then rerun doctor")
		add("subordinate_ids", false, "not observed: podman_info failed", "repair podman_info, then rerun doctor")
	}

	nsenterPath, nsenterErr := p.LookPath("nsenter")
	add("nsenter_executable", nsenterErr == nil, observation([]byte(nsenterPath), nsenterErr), "install util-linux so Kenogram can hand a listener into the world network namespace")

	stateOK, stateObserved := storageObservation(p.StateDir, stat, access, p.DiskFree)
	add("state_storage", stateOK, stateObserved, "make the Kenogram state filesystem available with effective write and traverse access")
	if podmanInfoOK {
		storeOK, storeObserved := storageObservation(info.Store.GraphRoot, stat, access, p.DiskFree)
		add("container_storage", storeOK, storeObserved, "make the rootless Podman graph root accessible and free space; composition guides state their larger lane-specific floors")
	} else {
		add("container_storage", false, "not observed: podman_info failed", "repair podman_info, then rerun doctor")
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

func requiredControllers(value []byte, err error) (bool, string) {
	if err != nil {
		return false, err.Error()
	}
	present := map[string]bool{}
	for _, controller := range strings.Fields(string(value)) {
		present[controller] = true
	}
	missing := []string{}
	for _, required := range []string{"cpu", "memory", "pids"} {
		if !present[required] {
			missing = append(missing, required)
		}
	}
	observed := strings.TrimSpace(string(value))
	if observed == "" {
		observed = "none"
	}
	if len(missing) != 0 {
		return false, fmt.Sprintf("missing=%s observed=%s", strings.Join(missing, ","), observed)
	}
	return true, observed
}

func nearestExisting(path string, stat func(string) (os.FileInfo, error)) (string, os.FileInfo, error) {
	if path == "" {
		path = "."
	}
	path = filepath.Clean(path)
	for {
		if info, err := stat(path); err == nil {
			return path, info, nil
		} else if !os.IsNotExist(err) {
			return path, nil, err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil, fmt.Errorf("no existing ancestor")
		}
		path = parent
	}
}

func storageObservation(path string, stat func(string) (os.FileInfo, error), access func(string) error, freeSpace func(string) (uint64, error)) (bool, string) {
	if strings.TrimSpace(path) == "" {
		return false, "path not reported"
	}
	existing, info, err := nearestExisting(path, stat)
	if err != nil {
		return false, fmt.Sprintf("%s: %v", existing, err)
	}
	if !info.IsDir() {
		return false, fmt.Sprintf("%s: not a directory", existing)
	}
	if err := access(existing); err != nil {
		return false, fmt.Sprintf("%s: effective write/traverse access: %v", existing, err)
	}
	free, err := freeSpace(existing)
	if err != nil || free == 0 {
		return false, diskObservation(existing, free, err)
	}
	return true, diskObservation(existing, free, nil) + "; effective write/traverse access confirmed"
}

func diskObservation(path string, free uint64, err error) string {
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	return fmt.Sprintf("%s: %.1f GiB free", path, float64(free)/float64(bytesPerGiB))
}

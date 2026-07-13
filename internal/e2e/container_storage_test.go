//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	defaultVFSMinimumFreeGiB = uint64(96)
	vfsMinimumFreeEnv        = "KENOGRAM_E2E_VFS_MIN_FREE_GIB"
)

type podmanCommandResult struct {
	output   string
	exitCode int
	err      error
}

type podmanRunner func(context.Context, ...string) podmanCommandResult

type containerStorageInfo struct {
	Rootless        bool
	GraphDriverName string
	GraphRoot       string
}

type imageLease struct {
	reference     string
	existedBefore bool
}

type e2eContainerResources struct {
	runner     podmanRunner
	containers []string
	images     []imageLease
}

func prepareContainerE2E(t *testing.T, ctx context.Context) *e2eContainerResources {
	t.Helper()
	runner := execPodman
	infoResult := runner(ctx, "info", "--format", "json")
	if infoResult.err != nil {
		t.Fatalf("inspect Podman storage before E2E: %v", podmanCommandError(infoResult))
	}
	info, err := parseContainerStorageInfo(infoResult.output)
	if err != nil {
		t.Fatalf("parse Podman storage before E2E: %v", err)
	}
	minimumFree, err := vfsMinimumFreeBytes(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	available, err := assessContainerStorage(info, minimumFree, availableBytes)
	if err != nil {
		t.Fatal(err)
	}
	if info.Rootless && info.GraphDriverName == "vfs" {
		t.Logf("container storage preflight rootless=true driver=vfs graph_root=%s available=%.1f GiB required=%.1f GiB", info.GraphRoot, bytesToGiB(available), bytesToGiB(minimumFree))
	} else {
		t.Logf("container storage preflight rootless=%t driver=%s", info.Rootless, info.GraphDriverName)
	}

	resources := &e2eContainerResources{runner: runner}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if cleanupErr := resources.cleanup(cleanupCtx); cleanupErr != nil {
			// Cleanup is a separate test error, so it remains visible without
			// replacing the failure that caused the cleanup path to run.
			t.Errorf("container E2E cleanup: %v", cleanupErr)
		}
	})
	return resources
}

func (r *e2eContainerResources) trackContainer(names ...string) {
	r.containers = append(r.containers, names...)
}

func (r *e2eContainerResources) trackImage(t *testing.T, ctx context.Context, reference string) {
	t.Helper()
	exists, err := podmanImageExists(ctx, r.runner, reference)
	if err != nil {
		t.Fatalf("record whether image %q predates E2E: %v", reference, err)
	}
	r.images = append(r.images, imageLease{reference: reference, existedBefore: exists})
}

func (r *e2eContainerResources) cleanup(ctx context.Context) error {
	var cleanupErrors []error
	for _, name := range r.containers {
		result := r.runner(ctx, "rm", "--force", "--ignore", name)
		if result.err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove container %q: %w", name, podmanCommandError(result)))
		}
	}
	// Derived images are tracked after their bases, so reverse order releases
	// dependants before a base image that this test pulled.
	for index := len(r.images) - 1; index >= 0; index-- {
		lease := r.images[index]
		if lease.existedBefore {
			continue
		}
		exists, err := podmanImageExists(ctx, r.runner, lease.reference)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect test-owned image %q: %w", lease.reference, err))
			continue
		}
		if !exists {
			continue
		}
		result := r.runner(ctx, "image", "rm", "--force", lease.reference)
		if result.err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove test-owned image %q: %w", lease.reference, podmanCommandError(result)))
		}
	}
	return errors.Join(cleanupErrors...)
}

func execPodman(ctx context.Context, args ...string) podmanCommandResult {
	command := exec.CommandContext(ctx, "podman", args...)
	output, err := command.CombinedOutput()
	result := podmanCommandResult{output: strings.TrimSpace(string(output)), err: err}
	if err == nil {
		return result
	}
	result.exitCode = -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.exitCode = exitError.ExitCode()
	}
	return result
}

func podmanImageExists(ctx context.Context, runner podmanRunner, reference string) (bool, error) {
	result := runner(ctx, "image", "exists", reference)
	switch result.exitCode {
	case 0:
		if result.err != nil {
			return false, podmanCommandError(result)
		}
		return true, nil
	case 1:
		return false, nil
	default:
		return false, podmanCommandError(result)
	}
}

func podmanCommandError(result podmanCommandResult) error {
	if result.err == nil {
		if result.output == "" {
			return fmt.Errorf("Podman exited with status %d", result.exitCode)
		}
		return fmt.Errorf("Podman exited with status %d: %s", result.exitCode, result.output)
	}
	if result.output == "" {
		return result.err
	}
	return fmt.Errorf("%w: %s", result.err, result.output)
}

func parseContainerStorageInfo(raw string) (containerStorageInfo, error) {
	var payload struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
		Store struct {
			GraphDriverName string `json:"graphDriverName"`
			GraphRoot       string `json:"graphRoot"`
		} `json:"store"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return containerStorageInfo{}, err
	}
	info := containerStorageInfo{
		Rootless:        payload.Host.Security.Rootless,
		GraphDriverName: strings.ToLower(strings.TrimSpace(payload.Store.GraphDriverName)),
		GraphRoot:       strings.TrimSpace(payload.Store.GraphRoot),
	}
	if info.GraphDriverName == "" {
		return containerStorageInfo{}, errors.New("Podman info did not report store.graphDriverName")
	}
	return info, nil
}

func vfsMinimumFreeBytes(getenv func(string) string) (uint64, error) {
	minimumGiB := defaultVFSMinimumFreeGiB
	if configured := strings.TrimSpace(getenv(vfsMinimumFreeEnv)); configured != "" {
		parsed, err := strconv.ParseUint(configured, 10, 64)
		if err != nil || parsed == 0 || parsed > math.MaxUint64/(1<<30) {
			return 0, fmt.Errorf("%s must be a positive whole number of GiB, got %q", vfsMinimumFreeEnv, configured)
		}
		minimumGiB = parsed
	}
	return minimumGiB << 30, nil
}

func assessContainerStorage(info containerStorageInfo, minimumFree uint64, freeBytes func(string) (uint64, error)) (uint64, error) {
	if !info.Rootless || info.GraphDriverName != "vfs" {
		return 0, nil
	}
	if info.GraphRoot == "" {
		return 0, errors.New("unsafe rootless Podman vfs storage: Podman info did not report store.graphRoot")
	}
	available, err := freeBytes(info.GraphRoot)
	if err != nil {
		return 0, fmt.Errorf("inspect free space for rootless Podman vfs at %q: %w", info.GraphRoot, err)
	}
	if available < minimumFree {
		return available, fmt.Errorf("unsafe rootless Podman vfs storage: %.1f GiB available at %q, require %.1f GiB; vfs duplicates unpacked layers and these image builds need transient headroom. Free space, select rootless overlay storage, or set %s to a locally validated positive GiB threshold (use `podman system df` to inspect usage)", bytesToGiB(available), info.GraphRoot, bytesToGiB(minimumFree), vfsMinimumFreeEnv)
	}
	return available, nil
}

func availableBytes(path string) (uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	if stats.Bsize <= 0 {
		return 0, fmt.Errorf("invalid filesystem block size %d", stats.Bsize)
	}
	blockSize := uint64(stats.Bsize)
	if stats.Bavail > math.MaxUint64/blockSize {
		return math.MaxUint64, nil
	}
	return stats.Bavail * blockSize, nil
}

func bytesToGiB(value uint64) float64 {
	return float64(value) / float64(uint64(1)<<30)
}

//go:build linux

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
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
	vfsMinimumFreeHermesGiB = uint64(96)
	vfsMinimumFreeEnv       = "KENOGRAM_E2E_VFS_MIN_FREE_GIB"
	cleanupCommandTimeout   = 15 * time.Second
	imageClaimTimeout       = 30 * time.Second
)

type containerE2ELane string

const (
	e2eLaneEngram   containerE2ELane = "engram"
	e2eLaneOpenClaw containerE2ELane = "openclaw"
	e2eLaneHermes   containerE2ELane = "hermes"
)

type podmanCommandResult struct {
	output   string
	stderr   string
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
	claimedID     string
}

type containerLease struct {
	name       string
	world      string
	generation int
}

type e2eContainerResources struct {
	runner     podmanRunner
	containers []containerLease
	images     []imageLease
}

func prepareContainerE2E(t *testing.T, ctx context.Context, lane containerE2ELane) *e2eContainerResources {
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
	if err := validateContainerStorageInfo(info); err != nil {
		t.Fatal(err)
	}
	laneMinimumGiB, err := laneVFSMinimumFreeGiB(lane)
	if err != nil {
		t.Fatal(err)
	}
	available, minimumFree, err := assessContainerStorage(info, laneMinimumGiB, os.Getenv, availableBytes)
	if err != nil {
		t.Fatalf("container storage policy for %s lane: %v", lane, err)
	}
	if info.Rootless && info.GraphDriverName == "vfs" {
		t.Logf("container storage preflight rootless=true driver=vfs graph_root=%s available=%.1f GiB required=%.1f GiB", info.GraphRoot, bytesToGiB(available), bytesToGiB(minimumFree))
	} else {
		t.Logf("container storage preflight rootless=%t driver=%s", info.Rootless, info.GraphDriverName)
	}

	resources := &e2eContainerResources{runner: runner}
	registerContainerCleanup(t, resources)
	return resources
}

func laneVFSMinimumFreeGiB(lane containerE2ELane) (uint64, error) {
	switch lane {
	case e2eLaneEngram, e2eLaneOpenClaw:
		// No default is invented until a reproducible rootless-vfs peak is
		// recorded for these lanes. Overlay remains unaffected; vfs operators
		// must provide a locally measured override.
		return 0, nil
	case e2eLaneHermes:
		return vfsMinimumFreeHermesGiB, nil
	default:
		return 0, fmt.Errorf("unknown container E2E lane %q", lane)
	}
}

func validateContainerStorageInfo(info containerStorageInfo) error {
	if !info.Rootless {
		return errors.New("container E2Es require rootless Podman; storage evidence reported rootless=false")
	}
	return nil
}

func registerContainerCleanup(t *testing.T, resources *e2eContainerResources) {
	t.Helper()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if cleanupErr := resources.cleanup(cleanupCtx); cleanupErr != nil {
			// Cleanup is a separate test error, so it remains visible without
			// replacing the failure that caused the cleanup path to run.
			t.Errorf("container E2E cleanup: %v", cleanupErr)
		}
	})
}

func e2eWorldName(t *testing.T, prefix string) string {
	t.Helper()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("generate E2E world nonce: %v", err)
	}
	return fmt.Sprintf("%s-%x", prefix, nonce)
}

func (r *e2eContainerResources) trackContainer(t *testing.T, ctx context.Context, world string, generation int) {
	t.Helper()
	name := containerName(world, generation)
	exists, err := podmanObjectExists(ctx, r.runner, "container", name)
	if err != nil {
		t.Fatalf("record whether container %q predates E2E: %v", name, err)
	}
	if exists {
		t.Fatalf("refuse to lease pre-existing container %q", name)
	}
	r.containers = append(r.containers, containerLease{name: name, world: world, generation: generation})
}

func (r *e2eContainerResources) trackImage(t *testing.T, ctx context.Context, reference string) {
	t.Helper()
	exists, err := podmanImageExists(ctx, r.runner, reference)
	if err != nil {
		t.Fatalf("record whether image %q predates E2E: %v", reference, err)
	}
	r.images = append(r.images, imageLease{reference: reference, existedBefore: exists})
}

func (r *e2eContainerResources) claimImage(t *testing.T, ctx context.Context, reference string) {
	t.Helper()
	for index := range r.images {
		lease := &r.images[index]
		if lease.reference != reference {
			continue
		}
		if lease.existedBefore {
			return
		}
		identity, err := podmanImageIdentity(ctx, r.runner, reference)
		if err != nil {
			t.Fatalf("claim acquired image %q: %v", reference, err)
		}
		lease.claimedID = identity
		return
	}
	t.Fatalf("claim untracked image %q", reference)
}

func (r *e2eContainerResources) claimAppearedImages(ctx context.Context, references ...string) error {
	var claimErrors []error
	for _, reference := range references {
		var lease *imageLease
		for index := range r.images {
			if r.images[index].reference == reference {
				lease = &r.images[index]
				break
			}
		}
		if lease == nil {
			claimErrors = append(claimErrors, fmt.Errorf("claim untracked image %q", reference))
			continue
		}
		if lease.existedBefore || lease.claimedID != "" {
			continue
		}
		exists, err := podmanImageExists(ctx, r.runner, reference)
		if err != nil {
			claimErrors = append(claimErrors, fmt.Errorf("inspect acquired image %q: %w", reference, err))
			continue
		}
		if !exists {
			continue
		}
		identity, err := podmanImageIdentity(ctx, r.runner, reference)
		if err != nil {
			claimErrors = append(claimErrors, fmt.Errorf("claim acquired image %q: %w", reference, err))
			continue
		}
		lease.claimedID = identity
	}
	return errors.Join(claimErrors...)
}

func (r *e2eContainerResources) imageClaimedOrPreexisting(reference string) bool {
	for _, lease := range r.images {
		if lease.reference == reference {
			return lease.existedBefore || lease.claimedID != ""
		}
	}
	return false
}

func runImageAcquisition(t *testing.T, ctx context.Context, resources *e2eContainerResources, references []string, dir string, env []string, name string, args ...string) string {
	t.Helper()
	output, operationErr := runResult(ctx, dir, env, name, args...)
	claimCtx, cancel := context.WithTimeout(context.Background(), imageClaimTimeout)
	defer cancel()
	claimErr := resources.claimAppearedImages(claimCtx, references...)
	if operationErr == nil {
		for _, reference := range references {
			if !resources.imageClaimedOrPreexisting(reference) {
				claimErr = errors.Join(claimErr, fmt.Errorf("successful acquisition did not materialize tracked image %q", reference))
			}
		}
	}
	if operationErr != nil {
		if claimErr != nil {
			t.Fatalf("%s %v: %v\n%s\npost-failure image claim: %v", name, args, operationErr, output, claimErr)
		}
		t.Fatalf("%s %v: %v\n%s", name, args, operationErr, output)
	}
	if claimErr != nil {
		t.Fatalf("claim images after %s %v: %v", name, args, claimErr)
	}
	return output
}

func (r *e2eContainerResources) cleanup(ctx context.Context) error {
	return r.cleanupWithTimeout(ctx, cleanupCommandTimeout)
}

func (r *e2eContainerResources) cleanupWithTimeout(ctx context.Context, commandTimeout time.Duration) error {
	var cleanupErrors []error
	runner := func(_ context.Context, args ...string) podmanCommandResult {
		commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		return r.runner(commandCtx, args...)
	}
	for index := len(r.containers) - 1; index >= 0; index-- {
		lease := r.containers[index]
		exists, err := podmanObjectExists(ctx, runner, "container", lease.name)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect tracked container %q: %w", lease.name, err))
			continue
		}
		if !exists {
			continue
		}
		identity, labels, err := podmanContainerIdentity(ctx, runner, lease.name)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("verify tracked container %q: %w", lease.name, err))
			continue
		}
		wantGeneration := strconv.Itoa(lease.generation)
		if labels["io.kenogram.world"] != lease.world || labels["io.kenogram.generation"] != wantGeneration {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("preserve container %q: ownership labels do not match world %q generation %s", lease.name, lease.world, wantGeneration))
			continue
		}
		result := runner(ctx, "rm", "--force", "--ignore", identity)
		if result.err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove container %q (%s): %w", lease.name, identity, podmanCommandError(result)))
		}
	}
	// Derived images are tracked after their bases, so reverse order releases
	// dependants before a base image that this test pulled.
	for index := len(r.images) - 1; index >= 0; index-- {
		lease := r.images[index]
		if lease.existedBefore {
			continue
		}
		exists, err := podmanImageExists(ctx, runner, lease.reference)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("inspect test-owned image %q: %w", lease.reference, err))
			continue
		}
		if !exists {
			continue
		}
		if lease.claimedID == "" {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("preserve unclaimed image %q: it appeared after the pre-test snapshot", lease.reference))
			continue
		}
		identity, err := podmanImageIdentity(ctx, runner, lease.reference)
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("verify test-acquired image %q: %w", lease.reference, err))
			continue
		}
		if identity != lease.claimedID {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("preserve image %q: identity changed from %s to %s", lease.reference, lease.claimedID, identity))
			continue
		}
		result := runner(ctx, "image", "rm", "--ignore", "--no-prune", lease.claimedID)
		if result.err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove test-owned image %q: %w", lease.reference, podmanCommandError(result)))
		}
	}
	return errors.Join(cleanupErrors...)
}

func execPodman(ctx context.Context, args ...string) podmanCommandResult {
	command := exec.CommandContext(ctx, "podman", args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := podmanCommandResult{output: strings.TrimSpace(stdout.String()), stderr: strings.TrimSpace(stderr.String()), err: err}
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
	return podmanObjectExists(ctx, runner, "image", reference)
}

func podmanObjectExists(ctx context.Context, runner podmanRunner, object, reference string) (bool, error) {
	result := runner(ctx, object, "exists", reference)
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

func podmanImageIdentity(ctx context.Context, runner podmanRunner, reference string) (string, error) {
	result := runner(ctx, "image", "inspect", reference)
	if result.err != nil {
		return "", podmanCommandError(result)
	}
	var payload []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal([]byte(result.output), &payload); err != nil {
		return "", err
	}
	if len(payload) != 1 || strings.TrimSpace(payload[0].ID) == "" {
		return "", fmt.Errorf("Podman image inspect returned %d objects without one identity", len(payload))
	}
	return strings.TrimSpace(payload[0].ID), nil
}

func podmanContainerIdentity(ctx context.Context, runner podmanRunner, reference string) (string, map[string]string, error) {
	result := runner(ctx, "container", "inspect", reference)
	if result.err != nil {
		return "", nil, podmanCommandError(result)
	}
	var payload []struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(result.output), &payload); err != nil {
		return "", nil, err
	}
	if len(payload) != 1 || strings.TrimSpace(payload[0].ID) == "" {
		return "", nil, fmt.Errorf("Podman container inspect returned %d objects without one identity", len(payload))
	}
	return strings.TrimSpace(payload[0].ID), payload[0].Config.Labels, nil
}

func podmanCommandError(result podmanCommandResult) error {
	diagnostic := strings.TrimSpace(strings.Join([]string{result.stderr, result.output}, "\n"))
	if result.err == nil {
		if diagnostic == "" {
			return fmt.Errorf("Podman exited with status %d", result.exitCode)
		}
		return fmt.Errorf("Podman exited with status %d: %s", result.exitCode, diagnostic)
	}
	if diagnostic == "" {
		return result.err
	}
	return fmt.Errorf("%w: %s", result.err, diagnostic)
}

func parseContainerStorageInfo(raw string) (containerStorageInfo, error) {
	var payload struct {
		Host struct {
			Security struct {
				Rootless *bool `json:"rootless"`
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
	if payload.Host.Security.Rootless == nil {
		return containerStorageInfo{}, errors.New("Podman info did not report host.security.rootless")
	}
	info := containerStorageInfo{
		Rootless:        *payload.Host.Security.Rootless,
		GraphDriverName: strings.ToLower(strings.TrimSpace(payload.Store.GraphDriverName)),
		GraphRoot:       strings.TrimSpace(payload.Store.GraphRoot),
	}
	if info.GraphDriverName == "" {
		return containerStorageInfo{}, errors.New("Podman info did not report store.graphDriverName")
	}
	return info, nil
}

func vfsMinimumFreeBytes(defaultGiB uint64, getenv func(string) string) (uint64, error) {
	minimumGiB := defaultGiB
	configured := strings.TrimSpace(getenv(vfsMinimumFreeEnv))
	if configured != "" {
		parsed, err := strconv.ParseUint(configured, 10, 64)
		if err != nil || parsed == 0 || parsed > math.MaxUint64/(1<<30) {
			return 0, fmt.Errorf("%s must be a positive whole number of GiB, got %q", vfsMinimumFreeEnv, configured)
		}
		minimumGiB = parsed
	}
	if minimumGiB == 0 {
		return 0, fmt.Errorf("no evidence-backed rootless-vfs floor is recorded; set %s to a locally measured positive whole number of GiB", vfsMinimumFreeEnv)
	}
	return minimumGiB << 30, nil
}

func assessContainerStorage(info containerStorageInfo, laneMinimumGiB uint64, getenv func(string) string, freeBytes func(string) (uint64, error)) (uint64, uint64, error) {
	if info.GraphDriverName != "vfs" {
		return 0, 0, nil
	}
	minimumFree, err := vfsMinimumFreeBytes(laneMinimumGiB, getenv)
	if err != nil {
		return 0, 0, err
	}
	if info.GraphRoot == "" {
		return 0, minimumFree, errors.New("unsafe rootless Podman vfs storage: Podman info did not report store.graphRoot")
	}
	available, err := freeBytes(info.GraphRoot)
	if err != nil {
		return 0, minimumFree, fmt.Errorf("inspect free space for rootless Podman vfs at %q: %w", info.GraphRoot, err)
	}
	if available < minimumFree {
		return available, minimumFree, fmt.Errorf("unsafe rootless Podman vfs storage: %.1f GiB available at %q, require %.1f GiB; vfs duplicates unpacked layers and these image builds need transient headroom. Free space, select rootless overlay storage, or set %s to a locally validated positive GiB threshold (use `df -h` on the graph root for capacity and `podman system df` for attribution)", bytesToGiB(available), info.GraphRoot, bytesToGiB(minimumFree), vfsMinimumFreeEnv)
	}
	return available, minimumFree, nil
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

//go:build linux

package e2e

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type scriptedPodman struct {
	responses map[string][]podmanCommandResult
	calls     []string
}

func (s *scriptedPodman) run(_ context.Context, args ...string) podmanCommandResult {
	key := strings.Join(args, "\x00")
	s.calls = append(s.calls, key)
	queue := s.responses[key]
	if len(queue) == 0 {
		return podmanCommandResult{exitCode: 125, err: errors.New("unexpected Podman command: " + strings.Join(args, " "))}
	}
	result := queue[0]
	s.responses[key] = queue[1:]
	return result
}

func imageInspect(id string) podmanCommandResult {
	return podmanCommandResult{output: `[{"Id":"` + id + `"}]`}
}

func containerInspect(id, world string, generation int) podmanCommandResult {
	return podmanCommandResult{output: `[{"Id":"` + id + `","Config":{"Labels":{"io.kenogram.world":"` + world + `","io.kenogram.generation":"` + strconv.Itoa(generation) + `"}}}]`}
}

func TestContainerCleanupPreservesPreexistingImage(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{runner: fake.run}
	resources.trackImage(t, context.Background(), "base")
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"image\x00exists\x00base"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Podman calls = %#v, want %#v", fake.calls, want)
	}
}

func TestContainerCleanupRemovesOnlyClaimedImageWithoutForce(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {
			{exitCode: 1, err: errors.New("image absent")},
			{exitCode: 0},
		},
		"image\x00inspect\x00base": {
			imageInspect("sha256:owned"),
			imageInspect("sha256:owned"),
		},
		"image\x00rm\x00--ignore\x00--no-prune\x00sha256:owned": {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{runner: fake.run}
	resources.trackImage(t, context.Background(), "base")
	resources.claimImage(t, context.Background(), "base")
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"image\x00exists\x00base",
		"image\x00inspect\x00base",
		"image\x00exists\x00base",
		"image\x00inspect\x00base",
		"image\x00rm\x00--ignore\x00--no-prune\x00sha256:owned",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Podman calls = %#v, want %#v", fake.calls, want)
	}
	for _, call := range fake.calls {
		if strings.Contains(call, "--force") {
			t.Fatalf("image cleanup used force: %q", call)
		}
	}
}

func TestContainerCleanupPreservesUnclaimedOrChangedImage(t *testing.T) {
	for _, test := range []struct {
		name      string
		claimedID string
		inspect   []podmanCommandResult
		want      string
	}{
		{name: "unclaimed", want: "unclaimed image"},
		{name: "changed", claimedID: "sha256:claimed", inspect: []podmanCommandResult{imageInspect("sha256:other")}, want: "identity changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			responses := map[string][]podmanCommandResult{
				"image\x00exists\x00base": {{exitCode: 0}},
			}
			if len(test.inspect) > 0 {
				responses["image\x00inspect\x00base"] = test.inspect
			}
			fake := &scriptedPodman{responses: responses}
			resources := &e2eContainerResources{runner: fake.run, images: []imageLease{{reference: "base", claimedID: test.claimedID}}}
			err := resources.cleanup(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("cleanup error = %v, want evidence %q", err, test.want)
			}
			for _, call := range fake.calls {
				if strings.HasPrefix(call, "image\x00rm") {
					t.Fatalf("unsafe image removal call = %q", call)
				}
			}
		})
	}
}

func TestContainerCleanupReportsInUseImageWithoutContainerSideEffects(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base":                               {{exitCode: 0}},
		"image\x00inspect\x00base":                              {imageInspect("sha256:owned")},
		"image\x00rm\x00--ignore\x00--no-prune\x00sha256:owned": {{exitCode: 2, stderr: "image is in use", err: errors.New("exit 2")}},
	}}
	resources := &e2eContainerResources{runner: fake.run, images: []imageLease{{reference: "base", claimedID: "sha256:owned"}}}
	err := resources.cleanup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "image is in use") {
		t.Fatalf("cleanup error = %v", err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "rm\x00") || strings.Contains(call, "--force") {
			t.Fatalf("cleanup affected a container: %q", call)
		}
	}
}

func TestContainerCleanupToleratesImageNeverPulled(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {
			{exitCode: 1, err: errors.New("image absent")},
			{exitCode: 1, err: errors.New("still absent")},
		},
	}}
	resources := &e2eContainerResources{runner: fake.run}
	resources.trackImage(t, context.Background(), "base")
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("Podman calls = %#v", fake.calls)
	}
}

func TestContainerCleanupContinuesAfterDerivedImageFailure(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00derived":                              {{exitCode: 0}},
		"image\x00inspect\x00derived":                             {imageInspect("sha256:derived")},
		"image\x00rm\x00--ignore\x00--no-prune\x00sha256:derived": {{exitCode: 2, stderr: "derived in use", err: errors.New("exit 2")}},
		"image\x00exists\x00base":                                 {{exitCode: 0}},
		"image\x00inspect\x00base":                                {imageInspect("sha256:base")},
		"image\x00rm\x00--ignore\x00--no-prune\x00sha256:base":    {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{runner: fake.run, images: []imageLease{
		{reference: "base", claimedID: "sha256:base"},
		{reference: "derived", claimedID: "sha256:derived"},
	}}
	err := resources.cleanup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "derived in use") {
		t.Fatalf("cleanup error = %v", err)
	}
	wantLast := "image\x00rm\x00--ignore\x00--no-prune\x00sha256:base"
	if got := fake.calls[len(fake.calls)-1]; got != wantLast {
		t.Fatalf("cleanup stopped before base removal; last call = %q, want %q", got, wantLast)
	}
}

func TestFailedAcquisitionClaimsAppearedBaseForCleanup(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {
			{exitCode: 0},
			{exitCode: 0},
		},
		"image\x00inspect\x00base": {
			imageInspect("sha256:base"),
			imageInspect("sha256:base"),
		},
		"image\x00exists\x00derived": {
			{exitCode: 1, err: errors.New("failed build did not create derived image")},
			{exitCode: 1, err: errors.New("derived image remains absent")},
		},
		"image\x00rm\x00--ignore\x00--no-prune\x00sha256:base": {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{runner: fake.run, images: []imageLease{
		{reference: "base"},
		{reference: "derived"},
	}}
	if err := resources.claimAppearedImages(context.Background(), "base", "derived"); err != nil {
		t.Fatal(err)
	}
	if resources.images[0].claimedID != "sha256:base" || resources.images[1].claimedID != "" {
		t.Fatalf("post-failure claims = %#v", resources.images)
	}
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.calls[len(fake.calls)-1]; got != "image\x00rm\x00--ignore\x00--no-prune\x00sha256:base" {
		t.Fatalf("failed acquisition cleanup last call = %q", got)
	}
}

func TestContainerCleanupUsesReverseOrderAndImmutableIDs(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"container\x00exists\x00kenogram-world-g2":  {{exitCode: 0}},
		"container\x00inspect\x00kenogram-world-g2": {containerInspect("id-two", "world", 2)},
		"rm\x00--force\x00--ignore\x00id-two":       {{exitCode: 0}},
		"container\x00exists\x00kenogram-world-g1":  {{exitCode: 0}},
		"container\x00inspect\x00kenogram-world-g1": {containerInspect("id-one", "world", 1)},
		"rm\x00--force\x00--ignore\x00id-one":       {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{runner: fake.run, containers: []containerLease{
		{name: "kenogram-world-g1", world: "world", generation: 1},
		{name: "kenogram-world-g2", world: "world", generation: 2},
	}}
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"container\x00exists\x00kenogram-world-g2",
		"container\x00inspect\x00kenogram-world-g2",
		"rm\x00--force\x00--ignore\x00id-two",
		"container\x00exists\x00kenogram-world-g1",
		"container\x00inspect\x00kenogram-world-g1",
		"rm\x00--force\x00--ignore\x00id-one",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Podman calls = %#v, want %#v", fake.calls, want)
	}
}

func TestContainerCleanupPreservesMismatchedContainer(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"container\x00exists\x00kenogram-world-g1":  {{exitCode: 0}},
		"container\x00inspect\x00kenogram-world-g1": {containerInspect("foreign", "other", 1)},
	}}
	resources := &e2eContainerResources{runner: fake.run, containers: []containerLease{{name: "kenogram-world-g1", world: "world", generation: 1}}}
	err := resources.cleanup(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ownership labels do not match") {
		t.Fatalf("cleanup error = %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("unsafe extra calls = %#v", fake.calls)
	}
}

func TestContainerCleanupRegistrationPrecedesEarlierCleanup(t *testing.T) {
	var events []string
	t.Run("scope", func(t *testing.T) {
		t.Cleanup(func() { events = append(events, "tempdir") })
		resources := &e2eContainerResources{
			runner: func(_ context.Context, args ...string) podmanCommandResult {
				events = append(events, strings.Join(args, " "))
				return podmanCommandResult{exitCode: 1, err: errors.New("absent")}
			},
			containers: []containerLease{{name: "kenogram-world-g1", world: "world", generation: 1}},
		}
		registerContainerCleanup(t, resources)
	})
	want := []string{"container exists kenogram-world-g1", "tempdir"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("cleanup events = %#v, want %#v", events, want)
	}
}

func TestContainerCleanupPerCommandTimeoutLeavesLaterWork(t *testing.T) {
	var calls []string
	runner := func(ctx context.Context, args ...string) podmanCommandResult {
		call := strings.Join(args, "\x00")
		calls = append(calls, call)
		if call == "container\x00exists\x00kenogram-world-g2" {
			<-ctx.Done()
			return podmanCommandResult{exitCode: -1, err: ctx.Err()}
		}
		return podmanCommandResult{exitCode: 1, err: errors.New("absent")}
	}
	resources := &e2eContainerResources{runner: runner, containers: []containerLease{
		{name: "kenogram-world-g1", world: "world", generation: 1},
		{name: "kenogram-world-g2", world: "world", generation: 2},
	}}
	err := resources.cleanupWithTimeout(context.Background(), 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("cleanup error = %v", err)
	}
	wantLast := "container\x00exists\x00kenogram-world-g1"
	if got := calls[len(calls)-1]; got != wantLast {
		t.Fatalf("cleanup did not continue after timeout; last call = %q", got)
	}
}

func TestPodmanImageExistsRejectsOperationalFailure(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {{exitCode: 125, stderr: "storage unavailable", err: errors.New("exit 125")}},
	}}
	exists, err := podmanImageExists(context.Background(), fake.run, "base")
	if err == nil || exists || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("image existence = (%t, %v)", exists, err)
	}
}

func TestParseContainerStorageRequiresRootlessEvidence(t *testing.T) {
	if _, err := parseContainerStorageInfo(`{"host":{"security":{}},"store":{"graphDriverName":"vfs","graphRoot":"/storage"}}`); err == nil || !strings.Contains(err.Error(), "host.security.rootless") {
		t.Fatalf("missing-rootless error = %v", err)
	}
	info, err := parseContainerStorageInfo(`{"host":{"security":{"rootless":false}},"store":{"graphDriverName":"vfs","graphRoot":"/storage"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateContainerStorageInfo(info); err == nil || !strings.Contains(err.Error(), "rootless=false") {
		t.Fatalf("rootful validation error = %v", err)
	}
}

func TestParseContainerStorageUsesStdoutDespiteStderrWarning(t *testing.T) {
	result := podmanCommandResult{
		output: `{"host":{"security":{"rootless":true}},"store":{"graphDriverName":"vfs","graphRoot":"/storage"}}`,
		stderr: "warning: cgroup manager fallback",
	}
	info, err := parseContainerStorageInfo(result.output)
	if err != nil || !info.Rootless || info.GraphDriverName != "vfs" {
		t.Fatalf("storage info = %#v, err = %v", info, err)
	}
}

func TestContainerStorageVFSPolicy(t *testing.T) {
	info := containerStorageInfo{Rootless: true, GraphDriverName: "vfs", GraphRoot: "/storage"}
	for _, test := range []struct {
		name      string
		available uint64
		statErr   error
		wantErr   string
	}{
		{name: "exact threshold", available: uint64(96) << 30},
		{name: "one byte low", available: (uint64(96) << 30) - 1, wantErr: "unsafe rootless Podman vfs storage"},
		{name: "stat failure", statErr: errors.New("stat failed"), wantErr: "stat failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			available, minimum, err := assessContainerStorage(info, 96, func(string) string { return "" }, func(path string) (uint64, error) {
				if path != "/storage" {
					t.Fatalf("stat path = %q", path)
				}
				return test.available, test.statErr
			})
			if test.wantErr == "" {
				if err != nil || available != test.available || minimum != uint64(96)<<30 {
					t.Fatalf("policy = (%d, %d, %v)", available, minimum, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("policy error = %v, want %q", err, test.wantErr)
			}
		})
	}

	_, _, err := assessContainerStorage(containerStorageInfo{Rootless: true, GraphDriverName: "vfs"}, 96, func(string) string { return "" }, func(string) (uint64, error) { return 0, nil })
	if err == nil || !strings.Contains(err.Error(), "graphRoot") {
		t.Fatalf("empty graph-root error = %v", err)
	}
}

func TestContainerStorageOverlayIgnoresVFSOverride(t *testing.T) {
	info := containerStorageInfo{Rootless: true, GraphDriverName: "overlay", GraphRoot: "/storage"}
	available, minimum, err := assessContainerStorage(info, 96, func(string) string { return "invalid" }, func(string) (uint64, error) {
		t.Fatal("overlay policy unexpectedly inspected free space")
		return 0, nil
	})
	if err != nil || available != 0 || minimum != 0 {
		t.Fatalf("overlay policy = (%d, %d, %v)", available, minimum, err)
	}
}

func TestContainerE2ELanePolicy(t *testing.T) {
	for _, test := range []struct {
		lane containerE2ELane
		want uint64
	}{
		{lane: e2eLaneEngram, want: 0},
		{lane: e2eLaneOpenClaw, want: 0},
		{lane: e2eLaneHermes, want: 96},
	} {
		got, err := laneVFSMinimumFreeGiB(test.lane)
		if err != nil || got != test.want {
			t.Errorf("lane %q floor = %d, err = %v; want %d", test.lane, got, err, test.want)
		}
	}
	if _, err := laneVFSMinimumFreeGiB("unknown"); err == nil {
		t.Fatal("unknown lane unexpectedly accepted")
	}
}

func TestVFSMinimumFreeOverride(t *testing.T) {
	minimum, err := vfsMinimumFreeBytes(96, func(name string) string {
		if name != vfsMinimumFreeEnv {
			t.Fatalf("environment lookup = %q", name)
		}
		return "112"
	})
	if err != nil || minimum != uint64(112)<<30 {
		t.Fatalf("minimum = %d, err = %v", minimum, err)
	}
	for _, invalid := range []string{"0", "-1", "1.5", "lots"} {
		if _, err := vfsMinimumFreeBytes(96, func(string) string { return invalid }); err == nil {
			t.Errorf("override %q unexpectedly accepted", invalid)
		}
	}
	if _, err := vfsMinimumFreeBytes(0, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "no evidence-backed") {
		t.Fatalf("unmeasured vfs floor error = %v", err)
	}
}

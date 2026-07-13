//go:build linux

package e2e

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
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

func TestContainerCleanupRemovesOnlyImagePulledByTest(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {
			{exitCode: 1, err: errors.New("image absent")},
			{exitCode: 0},
		},
		"image\x00rm\x00--force\x00base": {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{runner: fake.run}
	resources.trackImage(t, context.Background(), "base")
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"image\x00exists\x00base",
		"image\x00exists\x00base",
		"image\x00rm\x00--force\x00base",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Podman calls = %#v, want %#v", fake.calls, want)
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

func TestContainerCleanupCollectsFailuresAndContinues(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"rm\x00--force\x00--ignore\x00first":  {{exitCode: 125, output: "rm first", err: errors.New("exit 125")}},
		"rm\x00--force\x00--ignore\x00second": {{exitCode: 125, output: "rm second", err: errors.New("exit 125")}},
		"image\x00exists\x00derived":          {{exitCode: 0}},
		"image\x00rm\x00--force\x00derived":   {{exitCode: 125, output: "rm image", err: errors.New("exit 125")}},
	}}
	resources := &e2eContainerResources{
		runner:     fake.run,
		containers: []string{"first", "second"},
		images:     []imageLease{{reference: "derived"}},
	}
	err := resources.cleanup(context.Background())
	if err == nil {
		t.Fatal("cleanup unexpectedly succeeded")
	}
	for _, evidence := range []string{"first", "second", "derived"} {
		if !strings.Contains(err.Error(), evidence) {
			t.Errorf("cleanup error %q does not contain %q", err, evidence)
		}
	}
	if len(fake.calls) != 4 {
		t.Fatalf("cleanup stopped early; calls = %#v", fake.calls)
	}
}

func TestContainerCleanupReleasesDerivedImageBeforeBase(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00derived":        {{exitCode: 0}},
		"image\x00rm\x00--force\x00derived": {{exitCode: 0}},
		"image\x00exists\x00base":           {{exitCode: 0}},
		"image\x00rm\x00--force\x00base":    {{exitCode: 0}},
	}}
	resources := &e2eContainerResources{
		runner: fake.run,
		images: []imageLease{{reference: "base"}, {reference: "derived"}},
	}
	if err := resources.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"image\x00exists\x00derived",
		"image\x00rm\x00--force\x00derived",
		"image\x00exists\x00base",
		"image\x00rm\x00--force\x00base",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("Podman calls = %#v, want %#v", fake.calls, want)
	}
}

func TestPodmanImageExistsRejectsOperationalFailure(t *testing.T) {
	fake := &scriptedPodman{responses: map[string][]podmanCommandResult{
		"image\x00exists\x00base": {{exitCode: 125, output: "storage unavailable", err: errors.New("exit 125")}},
	}}
	exists, err := podmanImageExists(context.Background(), fake.run, "base")
	if err == nil || exists || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("image existence = (%t, %v)", exists, err)
	}
}

func TestParseAndAssessContainerStorage(t *testing.T) {
	info, err := parseContainerStorageInfo(`{
		"host":{"security":{"rootless":true}},
		"store":{"graphDriverName":"vfs","graphRoot":"/home/test/.local/share/containers/storage"}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Rootless || info.GraphDriverName != "vfs" || info.GraphRoot == "" {
		t.Fatalf("storage info = %#v", info)
	}
	statCalls := 0
	available := uint64(80) << 30
	_, err = assessContainerStorage(info, uint64(96)<<30, func(path string) (uint64, error) {
		statCalls++
		if path != info.GraphRoot {
			t.Fatalf("stat path = %q", path)
		}
		return available, nil
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe rootless Podman vfs storage") || !strings.Contains(err.Error(), vfsMinimumFreeEnv) {
		t.Fatalf("policy error = %v", err)
	}
	if statCalls != 1 {
		t.Fatalf("stat calls = %d", statCalls)
	}
}

func TestContainerStoragePolicyDoesNotWeakenOverlay(t *testing.T) {
	info := containerStorageInfo{Rootless: true, GraphDriverName: "overlay", GraphRoot: "/storage"}
	available, err := assessContainerStorage(info, uint64(96)<<30, func(string) (uint64, error) {
		t.Fatal("overlay policy unexpectedly inspected free space")
		return 0, nil
	})
	if err != nil || available != 0 {
		t.Fatalf("overlay policy = (%d, %v)", available, err)
	}
}

func TestVFSMinimumFreeOverride(t *testing.T) {
	minimum, err := vfsMinimumFreeBytes(func(name string) string {
		if name != vfsMinimumFreeEnv {
			t.Fatalf("environment lookup = %q", name)
		}
		return "112"
	})
	if err != nil || minimum != uint64(112)<<30 {
		t.Fatalf("minimum = %d, err = %v", minimum, err)
	}
	for _, invalid := range []string{"0", "-1", "1.5", "lots"} {
		if _, err := vfsMinimumFreeBytes(func(string) string { return invalid }); err == nil {
			t.Errorf("override %q unexpectedly accepted", invalid)
		}
	}
}

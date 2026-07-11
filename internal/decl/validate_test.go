package decl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validForValidation(t *testing.T) (Declaration, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("not-a-real-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := Declaration{
		Version: 1, Name: "engineering",
		World:     World{Hostname: "engineering", Base: "ubuntu@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Workdir: "/workspace", User: "agent"},
		Resources: Resources{CPUs: 2, MemoryBytes: 1024, PIDs: 32}, Workspace: Workspace{Paths: []string{"/workspace"}},
		Copies:   []Copy{{Source: "secret", Target: "/home/agent/token", Mode: "0600", Secret: true}},
		Mounts:   []Mount{{Source: "repo", Target: "/workspace/repo", Mode: "rw"}},
		Network:  Network{Allow: []NetworkAllow{{Host: "example.com", Port: 443}}},
		Services: []Service{{Name: "session", Command: []string{"tmux"}, Autostart: true, Restart: "on-failure"}},
	}
	return d, dir
}

func TestValidateAcceptsCompleteDeclaration(t *testing.T) {
	d, dir := validForValidation(t)
	if err := Validate(d, dir); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsUnpinnedImage(t *testing.T) {
	d, dir := validForValidation(t)
	d.World.Base = "ubuntu:latest"
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("got %v", err)
	}
	d.AllowUnpinned = true
	if err := Validate(d, dir); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsPermissiveSecret(t *testing.T) {
	d, dir := validForValidation(t)
	if err := os.Chmod(filepath.Join(dir, "secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRejectsReservedAndOverlappingMounts(t *testing.T) {
	d, dir := validForValidation(t)
	d.Mounts[0].Target = "/etc"
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("got %v", err)
	}
	d, dir = validForValidation(t)
	d.Mounts = append(d.Mounts, Mount{Source: "repo", Target: "/workspace/repo/sub", Mode: "ro"})
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRejectsDuplicateNetworkAndServices(t *testing.T) {
	d, dir := validForValidation(t)
	d.Network.Allow = append(d.Network.Allow, NetworkAllow{Host: "EXAMPLE.COM", Port: 443})
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "duplicate network") {
		t.Fatalf("got %v", err)
	}
	d, dir = validForValidation(t)
	d.Services = append(d.Services, d.Services[0])
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "duplicate service") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateRejectsSymlinkedSource(t *testing.T) {
	d, dir := validForValidation(t)
	if err := os.Symlink(filepath.Join(dir, "repo"), filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	d.Mounts[0].Source = "linked"
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("got %v", err)
	}
}

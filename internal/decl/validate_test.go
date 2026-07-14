package decl

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		Copies:     []Copy{{Source: "secret", Target: "/home/agent/token", Mode: "0600", Secret: true}},
		Mounts:     []Mount{{Source: "repo", Target: "/workspace/repo", Mode: "rw"}},
		Network:    Network{Allow: []NetworkAllow{{Host: "example.com", Port: 443}}},
		Interfaces: []Interface{{Name: "ssh", Address: "127.0.0.1:2222"}},
		Services:   []Service{{Name: "session", Command: []string{"tmux"}, Autostart: true, Restart: "on-failure"}},
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

func TestValidateAcceptsExactLocalImageID(t *testing.T) {
	d, dir := validForValidation(t)
	d.World.Base = "sha256:" + strings.Repeat("b", 64)
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

func TestValidateChecksEverySecretTreeNode(t *testing.T) {
	d, dir := validForValidation(t)
	secretDir := filepath.Join(dir, "secret-dir")
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(secretDir, "token")
	if err := os.WriteFile(nested, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.Copies[0].Source = "secret-dir"
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("permissive nested secret = %v", err)
	}
	if err := os.Chmod(nested, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Validate(d, dir); err != nil {
		t.Fatalf("private secret tree rejected: %v", err)
	}
}

func TestValidateRejectsSpecialMountSource(t *testing.T) {
	d, dir := validForValidation(t)
	special := filepath.Join(dir, "runtime.sock")
	if err := syscall.Mkfifo(special, 0o600); err != nil {
		t.Fatal(err)
	}
	d.Mounts[0].Source = "runtime.sock"
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "regular file or directory") {
		t.Fatalf("special mount source = %v", err)
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

func TestValidateInterfacesAreNamedCanonicalLoopbackEndpoints(t *testing.T) {
	for _, address := range []string{"0.0.0.0:22", "localhost:22", "127.0.0.1:0", "127.0.0.1:022", "127.0.0.1:65536", "http://127.0.0.1:22", "127.0.0.1:22/path"} {
		d, dir := validForValidation(t)
		d.Interfaces[0].Address = address
		if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "canonical 127.0.0.1:port") {
			t.Fatalf("address %q: %v", address, err)
		}
	}
	d, dir := validForValidation(t)
	d.Interfaces = append(d.Interfaces, d.Interfaces[0])
	if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "duplicate interface") {
		t.Fatalf("duplicate interface: %v", err)
	}
}

func TestValidateRejectsURLSyntaxInNetworkHost(t *testing.T) {
	for _, host := range []string{" user.example", "bad\thost", "*", "user@example.com", "example.com/path", "example.com?query", "example.com#fragment", "[2001:db8::1]", "not:ipv6"} {
		d, dir := validForValidation(t)
		d.Network.Allow[0].Host = host
		if err := Validate(d, dir); err == nil || !strings.Contains(err.Error(), "exact non-wildcard") {
			t.Fatalf("host %q: %v", host, err)
		}
	}
	d, dir := validForValidation(t)
	d.Network.Allow[0].Host = "2001:db8::1"
	if err := Validate(d, dir); err != nil {
		t.Fatalf("IPv6 host rejected: %v", err)
	}
}

func TestValidateRejectsUnsafeOperationalNames(t *testing.T) {
	for _, name := range []string{".", "..", "Upper", "-leading", "a/b", "a b", strings.Repeat("a", 64)} {
		d, dir := validForValidation(t)
		d.Name = name
		if err := Validate(d, dir); err == nil {
			t.Errorf("world name %q accepted", name)
		}
	}
	for _, name := range []string{".", "..", "Upper", "../escape", "a b", strings.Repeat("a", 64)} {
		d, dir := validForValidation(t)
		d.Services[0].Name = name
		if err := Validate(d, dir); err == nil {
			t.Errorf("service name %q accepted", name)
		}
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

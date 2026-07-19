//go:build linux

package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/lockfile"
)

func TestRootlessNetworkAbsenceAndDoor(t *testing.T) {
	if os.Getenv("KENOGRAM_INTEGRATION") != "1" {
		t.Skip("set KENOGRAM_INTEGRATION=1")
	}
	require(t, "podman")
	require(t, "nsenter")
	root := repoRoot(t)
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "fixture")
	buildEnv := append(os.Environ(), "CGO_ENABLED=0")
	run(t, root, buildEnv, "go", "build", "-buildvcs=false", "-o", fixture, "./internal/integration/testdata/probe")
	containerfile := filepath.Join(tmp, "Containerfile")
	if err := os.WriteFile(containerfile, []byte("FROM scratch\nCOPY fixture /usr/bin/tail\nCOPY fixture /usr/local/bin/probe\nCOPY fixture /bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	image := "localhost/kenogram-integration:" + fmt.Sprint(time.Now().UnixNano())
	run(t, tmp, nil, "podman", "build", "-t", image, "-f", containerfile, ".")
	bin := filepath.Join(tmp, "kenogram")
	run(t, root, nil, "go", "build", "-buildvcs=false", "-o", bin, "./cmd/kenogram")
	state := filepath.Join(tmp, "state")
	env := append(os.Environ(), "KENOGRAM_STATE_DIR="+state)
	t.Cleanup(func() {
		cleanupIntegrationWorld(t, tmp, env, bin, state)
		exec.Command("podman", "rm", "--force", "kenogram-integration-g1", "kenogram-integration-g2").Run()
		exec.Command("podman", "rmi", "--force", image).Run()
	})
	declaration := filepath.Join(tmp, "kenogram.toml")
	writeDeclaration(t, declaration, image, fmt.Sprintf("[[mounts]]\nsource = %q\ntarget = \"/host\"\nmode = \"ro\"\n", tmp))
	if out, err := runFailure(tmp, env, bin, "up", "--yes", declaration); err == nil || !strings.Contains(out, "protected host path") {
		t.Fatalf("state-parent mount was not rejected before creation: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(run(t, tmp, env, "podman", "ps", "--all", "--filter", "name=^kenogram-integration-g1$", "--format", "{{.Names}}")); got != "" {
		t.Fatalf("dangerous declaration created runtime %q", got)
	}
	writeDeclaration(t, declaration, image, "")
	run(t, tmp, env, bin, "up", "--yes", declaration)
	container := "kenogram-integration-g1"
	if got := strings.TrimSpace(run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "seccomp")); got != "2" {
		t.Fatalf("seccomp mode=%q, want filtered mode 2", got)
	}
	interfaces := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "interfaces")
	if strings.TrimSpace(interfaces) != "lo" {
		t.Fatalf("interfaces=%q", interfaces)
	}
	direct := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "dial", "1.1.1.1:443")
	if !strings.Contains(direct, "unroutable") {
		t.Fatalf("direct=%q", direct)
	}
	if got := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "resolve", "example.com"); !strings.Contains(got, "absent") {
		t.Fatalf("resolver=%q", got)
	}
	if got := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "udp", "1.1.1.1:53"); !strings.Contains(got, "unroutable") {
		t.Fatalf("udp=%q", got)
	}
	if got := strings.TrimSpace(run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "listeners")); got != "" {
		t.Fatalf("base listeners=%q", got)
	}
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go func() {
		for {
			conn, err := host.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	port := host.Addr().(*net.TCPAddr).Port
	writeDeclaration(t, declaration, image, fmt.Sprintf("[[network.allow]]\nhost = \"localhost\"\nport = %d\n", port))
	run(t, tmp, env, bin, "up", "--yes", declaration)
	container = "kenogram-integration-g2"
	proxied := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", port))
	if !strings.Contains(proxied, "200") {
		t.Fatalf("proxy=%q", proxied)
	}
	run(t, tmp, env, bin, "revoke", "integration", fmt.Sprintf("localhost:%d", port))
	revoked := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", port))
	if !strings.Contains(revoked, "403") {
		t.Fatalf("revoked declaration remained allowed: %q", revoked)
	}
	run(t, tmp, env, bin, "up", "--yes", declaration)
	reconciled := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", port))
	if !strings.Contains(reconciled, "200") {
		t.Fatalf("unchanged declaration did not restore policy: %q", reconciled)
	}
	direct = run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "dial", fmt.Sprintf("127.0.0.1:%d", port))
	if !strings.Contains(direct, "unroutable") {
		t.Fatalf("bypass=%q", direct)
	}
	if got := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "listeners"); !strings.Contains(got, "0C38") {
		t.Fatalf("door listener=%q", got)
	}
	denied := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", "denied.example:443")
	if !strings.Contains(denied, "403") {
		t.Fatalf("denied=%q", denied)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	run(t, tmp, env, bin, "allow", "integration", fmt.Sprintf("localhost:%d", closedPort), "--for", "1m")
	failed := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", closedPort))
	if !strings.Contains(failed, "502") {
		t.Fatalf("admitted unavailable destination=%q", failed)
	}
	historyPath := filepath.Join(state, "integration", "history.jsonl")
	historyBefore, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(state, "integration", "mutation.lock")
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := run(t, tmp, env, bin, "network-diagnostics", "--json", "--limit", "10", "--max-bytes", "4096", "integration")
	historyAfter, err := os.ReadFile(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(historyAfter) != string(historyBefore) {
		t.Fatal("read-only diagnostic changed authoritative history")
	}
	lockAfter, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(lockAfter) != string(lockBefore) {
		t.Fatal("read-only diagnostic changed mutation-lock metadata")
	}
	if len(diagnostic) > 4096 {
		t.Fatalf("diagnostic is %d bytes", len(diagnostic))
	}
	var observed struct {
		Generation int64 `json:"generation"`
		Events     []struct {
			Generation int64  `json:"generation"`
			Outcome    string `json:"outcome"`
			Host       string `json:"host"`
			Port       int    `json:"port"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(diagnostic), &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Generation != 2 || len(observed.Events) < 2 {
		t.Fatalf("network diagnostic = %#v", observed)
	}
	want := map[string]bool{"refused:denied.example:443": false, fmt.Sprintf("dial_failed:localhost:%d", closedPort): false}
	for _, event := range observed.Events {
		if event.Generation != observed.Generation {
			t.Fatalf("mixed generation event = %#v", event)
		}
		key := fmt.Sprintf("%s:%s:%d", event.Outcome, event.Host, event.Port)
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("diagnostic missing %s: %s", key, diagnostic)
		}
	}
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	go func() {
		for {
			conn, err := second.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	secondPort := second.Addr().(*net.TCPAddr).Port
	run(t, tmp, env, bin, "allow", "integration", fmt.Sprintf("localhost:%d", secondPort), "--for", "2s")
	granted := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", secondPort))
	if !strings.Contains(granted, "200") {
		t.Fatalf("grant=%q", granted)
	}
	time.Sleep(2100 * time.Millisecond)
	expired := run(t, tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", secondPort))
	if !strings.Contains(expired, "403") {
		t.Fatalf("expired=%q", expired)
	}
	var recorded struct {
		Generation int64 `json:"generation"`
		ProxyPID   int   `json:"proxy_pid"`
	}
	statePath := filepath.Join(state, "integration", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(recorded.ProxyPID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if out, err := runFailure(tmp, env, "podman", "exec", container, "/usr/local/bin/probe", "proxy", "127.0.0.1:3128", fmt.Sprintf("localhost:%d", port)); err == nil {
		t.Fatalf("door survived proxy death: %s", out)
	}
	if got := run(t, tmp, env, "podman", "inspect", "--format", "{{.State.Running}}", container); strings.TrimSpace(got) != "true" {
		t.Fatalf("world stopped with proxy: %q", got)
	}
	run(t, tmp, env, bin, "up", "--yes", declaration)
	raw, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.Generation != 2 {
		t.Fatalf("matching up replaced generation: %d", recorded.Generation)
	}
}

func cleanupIntegrationWorld(t *testing.T, dir string, env []string, bin, state string) {
	t.Helper()
	worldRoot := filepath.Join(state, "integration")
	proxyPID, proxyStart, identityErr := readIntegrationProxyIdentity(filepath.Join(worldRoot, "proxy.pid"))
	statePath := filepath.Join(worldRoot, "state.json")
	_, stateErr := os.Stat(statePath)
	if stateErr == nil {
		command := exec.Command(bin, "destroy", "--yes", "integration")
		command.Dir = dir
		command.Env = env
		if out, err := command.CombinedOutput(); err != nil {
			t.Errorf("destroy integration world: %v\n%s", err, out)
		}
	} else if !os.IsNotExist(stateErr) {
		t.Errorf("inspect integration cleanup state: %v", stateErr)
	} else if identityErr == nil {
		t.Errorf("integration proxy exists without durable world state")
	}
	if identityErr != nil && !os.IsNotExist(identityErr) {
		t.Errorf("read integration proxy identity: %v", identityErr)
	}
	if proxyStart != "" && lockfile.ProcessStart(proxyPID) == proxyStart {
		t.Errorf("integration proxy PID %d survived normal cleanup", proxyPID)
		if err := stopIntegrationProxy(proxyPID, proxyStart, filepath.Join(worldRoot, "proxy.sock")); err != nil {
			t.Errorf("fallback integration proxy cleanup: %v", err)
		}
	}
}

func readIntegrationProxyIdentity(path string) (int, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return 0, "", fmt.Errorf("invalid identity in %s", path)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 || fields[1] == "" {
		return 0, "", fmt.Errorf("invalid identity in %s", path)
	}
	return pid, fields[1], nil
}

func stopIntegrationProxy(pid int, start, socket string) error {
	if lockfile.ProcessStart(pid) != start {
		return nil
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(cmdline), "_proxy") || !strings.Contains(string(cmdline), socket) {
		return fmt.Errorf("proxy PID %d ownership is uncertain", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for lockfile.ProcessStart(pid) == start && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if lockfile.ProcessStart(pid) == start {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	deadline = time.Now().Add(2 * time.Second)
	for lockfile.ProcessStart(pid) == start && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if lockfile.ProcessStart(pid) == start {
		return fmt.Errorf("proxy PID %d survived fallback cleanup", pid)
	}
	return nil
}

func writeDeclaration(t *testing.T, path, image, network string) {
	t.Helper()
	raw := fmt.Sprintf("version = 1\nname = \"integration\"\nallow_unpinned = true\n[world]\nhostname = \"integration\"\nbase = %q\nworkdir = \"/workspace\"\nuser = \"0\"\n[resources]\ncpus = 1\nmemory_bytes = 268435456\npids = 128\n[workspace]\npaths = [\"/workspace\"]\n%s", image, network)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}
func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if env != nil {
		command.Env = env
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}
func runFailure(dir string, env []string, name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	command.Dir = dir
	if env != nil {
		command.Env = env
	}
	out, err := command.CombinedOutput()
	return string(out), err
}
func require(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required integration executable %s is unavailable: %v", name, err)
	}
}
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("root not found")
		}
		wd = parent
	}
}

//go:build linux

package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sshProof = "KENOGRAM_SSH_COMPOSITION_PROOF_7f67d903"
const sshWorldBase = "docker.io/library/ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90"

func TestSSHComposition(t *testing.T) {
	if os.Getenv("KENOGRAM_SSH_E2E") != "1" {
		t.Skip("set KENOGRAM_SSH_E2E=1 to run the SSH composition proof")
	}
	for _, executable := range []string{"podman", "nsenter", "ssh", "ssh-keygen"} {
		requireExecutable(t, executable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	tmp := t.TempDir()
	resources := prepareContainerE2E(t, ctx, e2eLaneSSH)
	root := repositoryRoot(t)

	port := unusedLoopbackPort(t)
	image := buildSSHImage(t, ctx, root, tmp, resources)
	stateRoot := filepath.Join(tmp, "state")
	world := e2eWorldName(t, "ssh-e2e", stateRoot)
	for generation := 1; generation <= 3; generation++ {
		resources.trackContainer(t, ctx, world, generation)
	}
	clientKey := filepath.Join(tmp, "client-key")
	wrongKey := filepath.Join(tmp, "wrong-key")
	hostKey := filepath.Join(tmp, "host-key")
	for _, key := range []string{clientKey, wrongKey, hostKey} {
		run(t, ctx, tmp, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	}
	authorizedKeys := filepath.Join(tmp, "authorized_keys")
	copyRegularFile(t, clientKey+".pub", authorizedKeys, 0o600)
	knownHosts := filepath.Join(tmp, "known_hosts")
	hostPublic, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(hostPublic))
	if len(fields) < 2 {
		t.Fatalf("invalid generated host public key: %q", hostPublic)
	}
	mustWrite(t, knownHosts, []byte(world+" "+fields[0]+" "+fields[1]+"\n"), 0o600)
	wrongKnownHosts := filepath.Join(tmp, "wrong_known_hosts")
	wrongHostPublic, err := os.ReadFile(wrongKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	wrongFields := strings.Fields(string(wrongHostPublic))
	if len(wrongFields) < 2 {
		t.Fatalf("invalid wrong host public key: %q", wrongHostPublic)
	}
	mustWrite(t, wrongKnownHosts, []byte(world+" "+wrongFields[0]+" "+wrongFields[1]+"\n"), 0o600)
	config := filepath.Join(tmp, "sshd_config")
	writeSSHConfig(t, config, port)
	revision := filepath.Join(tmp, "revision")
	mustWrite(t, revision, []byte("one\n"), 0o600)
	declaration := filepath.Join(tmp, "kenogram.toml")
	writeSSHDeclaration(t, declaration, world, image, port, config, hostKey, authorizedKeys, revision)
	kenogram := filepath.Join(tmp, "kenogram")
	run(t, ctx, root, append(os.Environ(), "CGO_ENABLED=0"), "go", "build", "-buildvcs=false", "-o", kenogram, "./cmd/kenogram")
	testEnv := append(os.Environ(), "KENOGRAM_STATE_DIR="+stateRoot)

	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	if version, versionErr := runResult(ctx, tmp, testEnv, "ssh", "-V"); versionErr == nil {
		t.Logf("SSH client: %s", strings.TrimSpace(version))
	}
	assertSSHEffectiveConfig(t, ctx, tmp, testEnv, world, 1)
	assertNoHostListener(t, port)
	assertSSHProof(t, ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts, "one")
	assertSSHPTYProof(t, ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts)
	if out, err := runSSH(ctx, tmp, testEnv, kenogram, world, wrongKey, knownHosts); err == nil {
		t.Fatalf("wrong SSH key was accepted:\n%s", out)
	}
	if out, err := runSSH(ctx, tmp, testEnv, kenogram, world, clientKey, wrongKnownHosts); err == nil {
		t.Fatalf("wrong SSH host key was accepted:\n%s", out)
	}
	if out, err := runResult(ctx, tmp, testEnv, kenogram, "connect", world, "undeclared"); err == nil || !strings.Contains(out, "not declared") {
		t.Fatalf("undeclared interface result err=%v output=%q", err, out)
	}

	mustWrite(t, revision, []byte("two\n"), 0o600)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	assertGeneration(t, filepath.Join(stateRoot, world, "state.json"), 2, "running")
	assertNoHostListener(t, port)
	assertSSHProof(t, ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts, "two")
	assertSSHPTYProof(t, ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts)
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", containerName(world, 1)); err == nil {
		t.Fatal("SSH predecessor survived replacement")
	}

	run(t, ctx, tmp, testEnv, kenogram, "down", world)
	if out, err := runSSH(ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts); err == nil || !strings.Contains(out, "container is not running") {
		t.Fatalf("down world remained connectable err=%v output=%q", err, out)
	}
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	assertSSHProof(t, ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts, "two")
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	if out, err := runSSH(ctx, tmp, testEnv, kenogram, world, clientKey, knownHosts); err == nil || !strings.Contains(out, `world "`+world+`" does not exist`) {
		t.Fatalf("destroyed world remained connectable err=%v output=%q", err, out)
	}
	assertDestroyedOutcomes(t, stateRoot, world, "applied", "stopped", "restarted", "destroyed")
}

func buildSSHImage(t *testing.T, ctx context.Context, root, tmp string, resources *e2eContainerResources) string {
	t.Helper()
	image := "localhost/kenogram-ssh-e2e:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	resources.trackImage(t, ctx, sshWorldBase)
	resources.trackImage(t, ctx, image)
	contextDir := filepath.Join(root, "images", "ssh-world")
	runImageAcquisition(t, ctx, resources, []string{sshWorldBase, image}, contextDir, nil, "podman", "build", "--pull=missing", "--build-arg", "USER_ID="+strconv.Itoa(os.Getuid()), "--build-arg", "GROUP_ID="+strconv.Itoa(os.Getgid()), "-t", image, ".")
	digest := strings.TrimSpace(run(t, ctx, tmp, nil, "podman", "image", "inspect", "--format", "{{.Digest}}", image))
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+sha256.Size*2 {
		t.Fatalf("invalid SSH image digest: %q", digest)
	}
	return image + "@" + digest
}

func writeSSHConfig(t *testing.T, path string, port int) {
	t.Helper()
	body := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey /home/agent/.ssh/host-key
AuthorizedKeysFile /home/agent/.ssh/authorized_keys
PidFile /tmp/kenogram-sshd.pid
PubkeyAuthentication yes
AuthenticationMethods publickey
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PermitEmptyPasswords no
AllowUsers agent
PermitRootLogin no
DisableForwarding yes
StrictModes yes
PrintMotd no
LogLevel VERBOSE
Subsystem sftp internal-sftp
`, port)
	mustWrite(t, path, []byte(body), 0o600)
}

func writeSSHDeclaration(t *testing.T, path, world, image string, port int, config, hostKey, authorizedKeys, revision string) {
	t.Helper()
	service := fmt.Sprintf(`["/usr/sbin/sshd", "-D", "-e", "-f", "/home/agent/.ssh/sshd_config"]`)
	body := fmt.Sprintf(`version = 1
name = %q
[world]
hostname = "ssh-e2e"
base = %q
workdir = "/workspace"
user = %q
[resources]
cpus = 1
memory_bytes = 536870912
pids = 128
[workspace]
paths = ["/workspace"]
[[copies]]
source = %q
target = "/home/agent/.ssh/sshd_config"
mode = "0600"
secret = false
[[copies]]
source = %q
target = "/home/agent/.ssh/host-key"
mode = "0600"
secret = true
[[copies]]
source = %q
target = "/home/agent/.ssh/authorized_keys"
mode = "0600"
secret = false
[[copies]]
source = %q
target = "/etc/ssh-revision"
mode = "0644"
secret = false
[[interfaces]]
name = "ssh"
address = %q
[[services]]
name = "ssh"
command = %s
autostart = true
restart = "on-failure"
`, world, image, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), config, hostKey, authorizedKeys, revision, "127.0.0.1:"+strconv.Itoa(port), service)
	mustWrite(t, path, []byte(body), 0o600)
}

func assertSSHProof(t *testing.T, ctx context.Context, dir string, env []string, kenogram, world, key, knownHosts, revision string) {
	t.Helper()
	waitFor(t, 15*time.Second, func() (bool, string) {
		out, err := runSSH(ctx, dir, env, kenogram, world, key, knownHosts)
		return err == nil && strings.Contains(out, sshProof+":"+revision), out
	})
}

func assertSSHPTYProof(t *testing.T, ctx context.Context, dir string, env []string, kenogram, world, key, knownHosts string) {
	t.Helper()
	proxy := kenogram + " connect " + world + " ssh"
	out, err := runResult(ctx, dir, env, "ssh", "-tt", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts, "-o", "HostKeyAlias="+world, "-o", "ProxyCommand="+proxy, "-i", key, "agent@"+world, "test -t 0 && test -t 1 && printf 'KENOGRAM_SSH_TTY_PROOF\\n'")
	if err != nil || !strings.Contains(out, "KENOGRAM_SSH_TTY_PROOF") {
		t.Fatalf("SSH PTY proof err=%v output=%q", err, out)
	}
}

func assertSSHEffectiveConfig(t *testing.T, ctx context.Context, dir string, env []string, world string, generation int) {
	t.Helper()
	out := strings.ToLower(run(t, ctx, dir, env, "podman", "exec", containerName(world, generation), "/usr/sbin/sshd", "-T", "-f", "/home/agent/.ssh/sshd_config"))
	for _, want := range []string{"pubkeyauthentication yes", "authenticationmethods publickey", "passwordauthentication no", "permitrootlogin no", "disableforwarding yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("effective sshd configuration lacks %q:\n%s", want, out)
		}
	}
	if version, err := runResult(ctx, dir, env, "podman", "exec", containerName(world, generation), "/usr/sbin/sshd", "-V"); err == nil {
		t.Logf("SSH server: %s", strings.TrimSpace(version))
	}
}

func runSSH(ctx context.Context, dir string, env []string, kenogram, world, key, knownHosts string) (string, error) {
	proxy := kenogram + " connect " + world + " ssh"
	return runResult(ctx, dir, env, "ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts, "-o", "HostKeyAlias="+world, "-o", "ProxyCommand="+proxy, "-i", key, "agent@"+world, "printf '"+sshProof+":'; cat /etc/ssh-revision")
}

func unusedLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func assertNoHostListener(t *testing.T, port int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), 250*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatalf("world interface leaked onto host port %d", port)
	}
}

//go:build linux

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const openClawProof = "KENOGRAM_OPENCLAW_PROOF_8d16d26f"
const openClawSecretCanary = "sk-kenogram-openclaw-canary-12c23bb884e94f89"
const openClawGatewayToken = "kenogram-gateway-token-c91366fb"
const openClawSignalID = "89abcdef0123456789abcdef01234567"
const openClawTelegramProof = "KENOGRAM_ENGRAM_OPENCLAW_PROOF_71c94f26"

type openClawLock struct {
	Version      string `json:"version"`
	NPMAsset     string `json:"npm_asset"`
	NPMSHA256    string `json:"npm_sha256"`
	NPMIntegrity string `json:"npm_integrity"`
	NPMURL       string `json:"npm_url"`
	Image        string `json:"image"`
}

func TestOpenClawReleaseLock(t *testing.T) {
	lock := readOpenClawLock(t)
	if lock.Version != "2026.6.11" || lock.NPMAsset != "openclaw-2026.6.11.tgz" {
		t.Fatalf("unexpected OpenClaw identity: %#v", lock)
	}
	if len(lock.NPMSHA256) != 64 || !strings.HasPrefix(lock.NPMIntegrity, "sha512-") {
		t.Fatalf("invalid OpenClaw integrity lock: %#v", lock)
	}
	if _, err := hex.DecodeString(lock.NPMSHA256); err != nil {
		t.Fatalf("invalid OpenClaw SHA-256: %v", err)
	}
	if digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lock.NPMIntegrity, "sha512-")); err != nil || len(digest) != sha512.Size {
		t.Fatalf("invalid OpenClaw npm integrity: %v", err)
	}
	if !strings.HasPrefix(lock.NPMURL, "https://registry.npmjs.org/openclaw/-/") {
		t.Fatalf("unexpected npm origin: %q", lock.NPMURL)
	}
	if !strings.HasPrefix(lock.Image, "docker.io/openclaw/openclaw@sha256:") {
		t.Fatalf("OpenClaw image is not official and digest-pinned: %q", lock.Image)
	}
}

func TestOpenClawTUIInsideKenogram(t *testing.T) {
	if os.Getenv("KENOGRAM_OPENCLAW_E2E") != "1" {
		t.Skip("set KENOGRAM_OPENCLAW_E2E=1 to run the OpenClaw composition proof")
	}
	requireExecutable(t, "podman")
	requireExecutable(t, "nsenter")
	if runtime.GOOS != "linux" {
		t.Fatalf("OpenClaw composition requires Linux, got %s", runtime.GOOS)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	doorHost := hostDoorIPv4(t)
	provider := newOpenAIProvider(t, doorHost)
	defer provider.Close()
	providerPort := mustPort(t, provider.URL)
	root := repositoryRoot(t)
	tmp := t.TempDir()
	lock := readOpenClawLock(t)
	t.Logf("evidence openclaw_version=%s npm_sha256=%s image=%s", lock.Version, lock.NPMSHA256, lock.Image)
	verifyOpenClawArtifact(t, ctx, tmp, lock)

	containerfile := filepath.Join(tmp, "Containerfile")
	containerBody := fmt.Sprintf(`FROM %s
USER root
RUN apt-get update && apt-get install --no-install-recommends -y tmux curl procps && rm -rf /var/lib/apt/lists/*
ARG KENOGRAM_UID
ARG KENOGRAM_GID
RUN getent group "$KENOGRAM_GID" >/dev/null || groupadd --gid "$KENOGRAM_GID" kenogram-test; printf 'kenogram:x:%%s:%%s:Kenogram OpenClaw:/workspace:/bin/sh\n' "$KENOGRAM_UID" "$KENOGRAM_GID" >> /etc/passwd
USER node
`, lock.Image)
	mustWrite(t, containerfile, []byte(containerBody), 0o600)
	image := "localhost/kenogram-openclaw-e2e:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	run(t, ctx, tmp, nil, "podman", "build", "--pull=missing", "--build-arg", "KENOGRAM_UID="+strconv.Itoa(os.Getuid()), "--build-arg", "KENOGRAM_GID="+strconv.Itoa(os.Getgid()), "-t", image, "-f", containerfile, ".")
	imageDigest := strings.TrimSpace(run(t, ctx, tmp, nil, "podman", "image", "inspect", "--format", "{{.Digest}}", image))
	if !strings.HasPrefix(imageDigest, "sha256:") {
		t.Fatalf("invalid fixture image digest: %q", imageDigest)
	}
	pinnedImage := image + "@" + imageDigest

	world := "openclaw-e2e-" + strconv.Itoa(os.Getpid())
	stateRoot := filepath.Join(tmp, "state")
	configSource := filepath.Join(tmp, "openclaw.json")
	revisionSource := filepath.Join(tmp, "revision")
	declaration := filepath.Join(tmp, "kenogram.toml")
	kenogram := filepath.Join(tmp, "kenogram")
	run(t, ctx, root, append(os.Environ(), "CGO_ENABLED=0"), "go", "build", "-o", kenogram, "./cmd/kenogram")
	testEnv := append(os.Environ(), "KENOGRAM_STATE_DIR="+stateRoot)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		for generation := 1; generation <= 3; generation++ {
			_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", containerName(world, generation)).Run()
		}
		_ = exec.CommandContext(cleanupCtx, "podman", "rmi", "--force", image).Run()
	})

	writeOpenClawConfig(t, configSource, doorHost, providerPort)
	mustWrite(t, revisionSource, []byte("one\n"), 0o600)
	writeOpenClawDeclaration(t, declaration, world, pinnedImage, configSource, revisionSource, doorHost, providerPort)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	first := containerName(world, 1)
	waitForOpenClaw(t, ctx, tmp, testEnv, first)
	assertOpenClawVersion(t, ctx, tmp, testEnv, first, lock.Version)
	assertOpenClawIsolation(t, ctx, tmp, testEnv, first, doorHost, providerPort)
	runOpenClawTUI(t, ctx, tmp, testEnv, first, "proof-one")
	provider.assertObserved(t)
	run(t, ctx, tmp, testEnv, "podman", "exec", first, "/bin/sh", "-c", "printf carried > /workspace/openclaw-carry")
	assertSecretAbsentOutsideWorkspace(t, filepath.Join(stateRoot, world), openClawSecretCanary)

	// An unchanged declaration must adopt the running generation rather than
	// replacing it. This proves the idempotent path before forcing a successor.
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", first); err != nil {
		t.Fatalf("OpenClaw generation was not adopted: %v", err)
	}

	mustWrite(t, revisionSource, []byte("two\n"), 0o600)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	second := containerName(world, 2)
	waitForOpenClaw(t, ctx, tmp, testEnv, second)
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/workspace/openclaw-carry")); got != "carried" {
		t.Fatalf("OpenClaw carried state = %q", got)
	}
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/etc/openclaw-revision")); got != "two" {
		t.Fatalf("OpenClaw regenerated revision = %q", got)
	}
	runOpenClawTUI(t, ctx, tmp, testEnv, second, "proof-two")
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", first); err == nil {
		t.Fatal("OpenClaw predecessor survived replacement")
	}

	run(t, ctx, tmp, testEnv, kenogram, "down", world)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	waitForOpenClaw(t, ctx, tmp, testEnv, second)
	runOpenClawTUI(t, ctx, tmp, testEnv, second, "proof-restart")
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	assertDestroyedHistory(t, stateRoot, world)
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), openClawSecretCanary)
}

type observedProvider struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
	bodies   []string
}

func newOpenAIProvider(t *testing.T, host string) *observedProvider {
	t.Helper()
	provider := &observedProvider{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			response.WriteHeader(http.StatusOK)
			return
		}
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(response, request)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			http.Error(response, "read request", http.StatusBadRequest)
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		provider.mu.Lock()
		provider.requests++
		provider.bodies = append(provider.bodies, string(raw))
		provider.mu.Unlock()
		proof := openClawProof + "\n[engram:upstream] " + openClawSignalID + " " + openClawTelegramProof
		if body.Stream {
			response.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(response, "data: {\"id\":\"proof\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", proof)
			fmt.Fprint(response, "data: {\"id\":\"proof\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(response, "{\"id\":\"proof\",\"object\":\"chat.completion\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}", proof)
	}))
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	provider.Server = server
	return provider
}

func (p *observedProvider) assertObserved(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.requests == 0 {
		t.Fatal("OpenClaw never reached the declared provider")
	}
}

func (p *observedProvider) waitObserved(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		requests := p.requests
		p.mu.Unlock()
		if requests > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("OpenClaw never reached the declared provider")
}

func (p *observedProvider) waitObservedContaining(t *testing.T, timeout time.Duration, fragment string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, body := range p.bodies {
			if strings.Contains(body, fragment) {
				p.mu.Unlock()
				return
			}
		}
		p.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("OpenClaw provider request did not contain canary marker %q", fragment)
}

func readOpenClawLock(t *testing.T) openClawLock {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "openclaw-2026.6.11.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock openClawLock
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	return lock
}

func verifyOpenClawArtifact(t *testing.T, ctx context.Context, tmp string, lock openClawLock) {
	t.Helper()
	archive := filepath.Join(tmp, lock.NPMAsset)
	if supplied := os.Getenv("KENOGRAM_OPENCLAW_ARCHIVE"); supplied != "" {
		copyRegularFile(t, supplied, archive, 0o600)
	} else {
		download(t, ctx, lock.NPMURL, archive)
	}
	verifyFileDigest(t, archive, lock.NPMSHA256)
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha512.New()
	if _, err := io.Copy(hash, file); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lock.NPMIntegrity, "sha512-"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash.Sum(nil), want) {
		t.Fatal("OpenClaw archive does not match its locked npm integrity")
	}
	if len(lock.NPMSHA256) != sha256.Size*2 {
		t.Fatalf("OpenClaw SHA-256 length = %d", len(lock.NPMSHA256))
	}
}

func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	parts := strings.Split(rawURL, ":")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func writeOpenClawConfig(t *testing.T, path, providerHost string, providerPort int) {
	t.Helper()
	body := fmt.Sprintf(`{
  "gateway": {"mode":"local","bind":"loopback","auth":{"mode":"token","token":%q}},
  "proxy": {"enabled":true,"proxyUrl":"http://127.0.0.1:3128","loopbackMode":"gateway-only"},
  "models": {"mode":"merge","providers":{"kenogram-proof":{"baseUrl":"http://%s:%d/v1","apiKey":%q,"api":"openai-completions","models":[{"id":"proof","name":"Kenogram proof","reasoning":false,"input":["text"],"contextWindow":32768,"maxTokens":256}]}}},
  "agents": {"defaults":{"workspace":"/workspace/openclaw","model":{"primary":"kenogram-proof/proof"},"models":{"kenogram-proof/proof":{"params":{"stream":true}}},"memorySearch":{"enabled":false},"sandbox":{"mode":"off"}}}
}
`, openClawGatewayToken, providerHost, providerPort, openClawSecretCanary)
	mustWrite(t, path, []byte(body), 0o600)
}

func writeOpenClawDeclaration(t *testing.T, path, world, image, configSource, revisionSource, providerHost string, providerPort int) {
	t.Helper()
	service := []string{"/usr/bin/env", "HOME=/workspace/home", "OPENCLAW_HOME=/workspace/home", "OPENCLAW_STATE_DIR=/workspace/.openclaw", "OPENCLAW_CONFIG_PATH=/etc/openclaw.json", "OPENCLAW_WORKSPACE_DIR=/workspace/openclaw", "OPENCLAW_GATEWAY_TOKEN=" + openClawGatewayToken, "OPENCLAW_PROXY_URL=http://127.0.0.1:3128", "OPENCLAW_DISABLE_BONJOUR=1", "/usr/local/bin/openclaw", "gateway", "--port", "18789", "--verbose"}
	commandJSON, err := json.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`version = 1
name = %q
[world]
hostname = "openclaw-e2e"
base = %q
workdir = "/workspace"
user = %q
[resources]
cpus = 2
memory_bytes = 2147483648
pids = 512
[workspace]
paths = ["/workspace"]
[[copies]]
source = %q
target = "/etc/openclaw.json"
mode = "0600"
secret = true
[[copies]]
source = %q
target = "/etc/openclaw-revision"
mode = "0644"
[[network.allow]]
host = %q
port = %d
[[services]]
name = "openclaw-gateway"
command = %s
autostart = true
restart = "never"
`, world, image, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), configSource, revisionSource, providerHost, providerPort, commandJSON)
	mustWrite(t, path, []byte(body), 0o600)
}

func openClawEnvCommand(args ...string) []string {
	command := []string{"/usr/bin/env", "HOME=/workspace/home", "OPENCLAW_HOME=/workspace/home", "OPENCLAW_STATE_DIR=/workspace/.openclaw", "OPENCLAW_CONFIG_PATH=/etc/openclaw.json", "OPENCLAW_WORKSPACE_DIR=/workspace/openclaw", "OPENCLAW_GATEWAY_TOKEN=" + openClawGatewayToken, "OPENCLAW_PROXY_URL=http://127.0.0.1:3128", "OPENCLAW_DISABLE_BONJOUR=1"}
	return append(command, args...)
}

func waitForOpenClaw(t *testing.T, ctx context.Context, dir string, env []string, container string) {
	t.Helper()
	waitFor(t, 30*time.Second, func() (bool, string) {
		out, err := runResult(ctx, dir, env, "podman", "exec", container, "curl", "--fail", "--silent", "http://127.0.0.1:18789/readyz")
		return err == nil, out
	})
}

func assertOpenClawVersion(t *testing.T, ctx context.Context, dir string, env []string, container, version string) {
	t.Helper()
	out := strings.TrimSpace(run(t, ctx, dir, env, "podman", "exec", container, "/usr/local/bin/openclaw", "--version"))
	if !strings.Contains(out, version) {
		t.Fatalf("in-world OpenClaw version = %q, want %q", out, version)
	}
}

func assertOpenClawIsolation(t *testing.T, ctx context.Context, dir string, env []string, container, providerHost string, providerPort int) {
	t.Helper()
	if out, err := runResult(ctx, dir, env, "podman", "exec", container, "/usr/bin/env", "-u", "HTTP_PROXY", "-u", "HTTPS_PROXY", "-u", "ALL_PROXY", "-u", "http_proxy", "-u", "https_proxy", "-u", "all_proxy", "curl", "--fail", "--max-time", "2", fmt.Sprintf("http://%s:%d/healthz", providerHost, providerPort)); err == nil {
		t.Fatalf("provider was directly reachable without Kenogram door: %s", out)
	}
	if out, err := runResult(ctx, dir, env, "podman", "exec", container, "test", "!", "-e", "/run/podman/podman.sock"); err != nil {
		t.Fatalf("runtime socket visible: %s", out)
	}
}

func hostDoorIPv4(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ipv4 := ip.To4(); ipv4 != nil && !ipv4.IsLinkLocalUnicast() {
				return ipv4.String()
			}
		}
	}
	t.Fatal("no non-loopback IPv4 address is available for the host-side proof fixtures")
	return ""
}

func runOpenClawTUI(t *testing.T, ctx context.Context, dir string, env []string, container, session string) {
	t.Helper()
	_, _ = runResult(ctx, dir, env, "podman", "exec", container, "tmux", "kill-session", "-t", session)
	command := openClawEnvCommand("/usr/local/bin/openclaw", "tui", "--url", "ws://127.0.0.1:18789", "--token", openClawGatewayToken, "--session", session, "--message", "Reply with the proof marker.", "--timeout-ms", "20000")
	args := append([]string{"exec", container, "tmux", "new-session", "-d", "-x", "120", "-y", "40", "-s", session}, command...)
	run(t, ctx, dir, env, "podman", args...)
	waitFor(t, 30*time.Second, func() (bool, string) {
		out, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", session)
		return err == nil && strings.Contains(out, openClawProof), out
	})
}

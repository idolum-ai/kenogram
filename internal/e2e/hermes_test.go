//go:build linux

package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const hermesProof = "KENOGRAM_HERMES_PROOF_a47e82d1"
const hermesSecretCanary = "sk-kenogram-hermes-canary-117b4cc2e5354dab"
const hermesNativeTelegramPrompt = "KENOGRAM_HERMES_TELEGRAM_PROMPT_00bff571"
const hermesDeniedTelegramPrompt = "KENOGRAM_HERMES_TELEGRAM_DENIED_d6721e0c"

type hermesLock struct {
	Release      string `json:"release"`
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	SourceAsset  string `json:"source_asset"`
	SourceSHA256 string `json:"source_sha256"`
	SourceURL    string `json:"source_url"`
	Image        string `json:"image"`
}

func TestHermesReleaseLock(t *testing.T) {
	lock := readHermesLock(t)
	if lock.Release != "v2026.7.7.2" || lock.Version != "0.18.2" || lock.Commit != "9de9c25f620ff7f1ce0fd5457d596052d5159596" {
		t.Fatalf("unexpected Hermes identity: %#v", lock)
	}
	if lock.SourceAsset != "hermes-agent-v2026.7.7.2.tar.gz" || len(lock.SourceSHA256) != 64 {
		t.Fatalf("invalid Hermes source lock: %#v", lock)
	}
	if _, err := hex.DecodeString(lock.SourceSHA256); err != nil {
		t.Fatalf("invalid Hermes source SHA-256: %v", err)
	}
	if lock.SourceURL != "https://github.com/NousResearch/hermes-agent/archive/refs/tags/v2026.7.7.2.tar.gz" {
		t.Fatalf("unexpected Hermes source origin: %q", lock.SourceURL)
	}
	if !strings.HasPrefix(lock.Image, "docker.io/nousresearch/hermes-agent@sha256:") {
		t.Fatalf("Hermes image is not official and digest-pinned: %q", lock.Image)
	}
}

func TestHermesInsideKenogram(t *testing.T) {
	if os.Getenv("KENOGRAM_HERMES_E2E") != "1" {
		t.Skip("set KENOGRAM_HERMES_E2E=1 to run the Hermes composition proof")
	}
	requireExecutable(t, "podman")
	requireExecutable(t, "nsenter")
	if runtime.GOOS != "linux" {
		t.Fatalf("Hermes composition requires Linux, got %s", runtime.GOOS)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	doorHost := hostDoorIPv4(t)
	provider := newObservedProvider(t, doorHost, hermesProof)
	defer provider.Close()
	telegram := newTelegramFixture(t, doorHost)
	defer telegram.Close()
	providerPort := mustPort(t, provider.URL)
	telegramPort := mustPort(t, telegram.URL)
	root := repositoryRoot(t)
	tmp := t.TempDir()
	lock := readHermesLock(t)
	t.Logf("evidence hermes_release=%s version=%s commit=%s source_sha256=%s image=%s", lock.Release, lock.Version, lock.Commit, lock.SourceSHA256, lock.Image)
	verifyHermesArtifact(t, ctx, tmp, lock)
	image := lock.Image

	world := "hermes-e2e-" + strconv.Itoa(os.Getpid())
	stateRoot := filepath.Join(tmp, "state")
	configSource := filepath.Join(tmp, "hermes-config.yaml")
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
	})

	writeHermesConfig(t, configSource, doorHost, providerPort, hermesTelegramAPIBase(telegram.URL, doorHost))
	mustWrite(t, revisionSource, []byte("one\n"), 0o600)
	writeHermesDeclaration(t, declaration, world, image, configSource, revisionSource, doorHost, providerPort, doorHost, telegramPort)
	getMeCount, getUpdatesCount := telegram.methodCount("getMe"), telegram.methodCount("getUpdates")
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	first := containerName(world, 1)
	waitForHermesTelegramAfter(t, telegram, 45*time.Second, getMeCount, getUpdatesCount)
	assertHermesVersion(t, ctx, tmp, testEnv, first, lock.Version)
	assertHermesIsolation(t, ctx, tmp, testEnv, first, doorHost, providerPort)
	telegram.enqueueTextFrom(telegramFixtureUser+1, hermesDeniedTelegramPrompt)
	telegram.enqueueText("Reply with the proof marker and preserve this request marker: " + hermesNativeTelegramPrompt)
	provider.waitObservedContaining(t, 45*time.Second, hermesNativeTelegramPrompt)
	provider.assertNotObservedContaining(t, hermesDeniedTelegramPrompt)
	telegram.waitOutbound(t, 45*time.Second, hermesProof)
	runHermesTUI(t, ctx, tmp, testEnv, first, provider, "hermes-proof-one", "HERMES_TUI_PROOF_ONE")
	run(t, ctx, tmp, testEnv, "podman", "exec", first, "/bin/sh", "-c", "printf carried > /workspace/hermes-carry")
	assertSecretAbsentOutsideWorkspace(t, filepath.Join(stateRoot, world), hermesSecretCanary)
	assertSecretAbsentOutsideWorkspace(t, filepath.Join(stateRoot, world), telegramFixtureToken)

	// An unchanged declaration adopts the live generation.
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", first); err != nil {
		t.Fatalf("Hermes generation was not adopted: %v", err)
	}

	mustWrite(t, revisionSource, []byte("two\n"), 0o600)
	getMeCount, getUpdatesCount = telegram.methodCount("getMe"), telegram.methodCount("getUpdates")
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	second := containerName(world, 2)
	waitForHermesTelegramAfter(t, telegram, 45*time.Second, getMeCount, getUpdatesCount)
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/workspace/hermes-carry")); got != "carried" {
		t.Fatalf("Hermes carried state = %q", got)
	}
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/etc/hermes-revision")); got != "two" {
		t.Fatalf("Hermes regenerated revision = %q", got)
	}
	runHermesTUI(t, ctx, tmp, testEnv, second, provider, "hermes-proof-two", "HERMES_TUI_PROOF_TWO")
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", first); err == nil {
		t.Fatal("Hermes predecessor survived replacement")
	}

	run(t, ctx, tmp, testEnv, kenogram, "down", world)
	getMeCount, getUpdatesCount = telegram.methodCount("getMe"), telegram.methodCount("getUpdates")
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	waitForHermesTelegramAfter(t, telegram, 45*time.Second, getMeCount, getUpdatesCount)
	runHermesTUI(t, ctx, tmp, testEnv, second, provider, "hermes-proof-restart", "HERMES_TUI_PROOF_RESTART")
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	assertDestroyedHistory(t, stateRoot, world)
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), hermesSecretCanary)
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), telegramFixtureToken)
}

func readHermesLock(t *testing.T) hermesLock {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hermes-agent-v2026.7.7.2.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock hermesLock
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	return lock
}

func verifyHermesArtifact(t *testing.T, ctx context.Context, tmp string, lock hermesLock) {
	t.Helper()
	archive := filepath.Join(tmp, lock.SourceAsset)
	if supplied := os.Getenv("KENOGRAM_HERMES_ARCHIVE"); supplied != "" {
		copyRegularFile(t, supplied, archive, 0o600)
	} else {
		download(t, ctx, lock.SourceURL, archive)
	}
	verifyFileDigest(t, archive, lock.SourceSHA256)
}

func writeHermesConfig(t *testing.T, path, providerHost string, providerPort int, telegramBase string) {
	t.Helper()
	telegramConfig := ""
	if telegramBase != "" {
		telegramConfig = fmt.Sprintf(`platforms:
  telegram:
    enabled: true
    allow_from:
      - %q
    dm_policy: allowlist
    extra:
      base_url: %q
      base_file_url: %q
`, strconv.FormatInt(telegramFixtureUser, 10), telegramBase+"/bot", telegramBase+"/file/bot")
	}
	body := fmt.Sprintf(`model:
  provider: custom
  model: proof
  default: proof
  base_url: http://%s:%d/v1
  api_key: %q
  api_mode: chat_completions
display:
  interface: tui
%s`, providerHost, providerPort, hermesSecretCanary, telegramConfig)
	mustWrite(t, path, []byte(body), 0o600)
}

func writeHermesDeclaration(t *testing.T, path, world, image, configSource, revisionSource, providerHost string, providerPort int, telegramHost string, telegramPort int) {
	t.Helper()
	tmuxCopies := hermesTmuxCopies(t)
	gatewayArgs := hermesEnvCommand("/opt/hermes/.venv/bin/hermes", "gateway", "run", "--no-supervise", "--quiet")
	gatewayShell := "mkdir -p /workspace/.hermes && cp /etc/hermes-config.yaml /workspace/.hermes/config.yaml && chmod 0600 /workspace/.hermes/config.yaml && exec " + shellJoin(gatewayArgs)
	gateway, err := json.Marshal([]string{"/bin/sh", "-c", gatewayShell})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`version = 1
name = %q
[world]
hostname = "hermes-e2e"
base = %q
workdir = "/workspace"
user = %q
[resources]
cpus = 2
memory_bytes = 3221225472
pids = 768
[workspace]
paths = ["/workspace"]
%s
[[copies]]
source = %q
target = "/etc/hermes-config.yaml"
mode = "0600"
secret = true
[[copies]]
source = %q
target = "/etc/hermes-revision"
mode = "0644"
[[network.allow]]
host = %q
port = %d
[[network.allow]]
host = %q
port = %d
[[services]]
name = "hermes-gateway"
command = %s
autostart = true
restart = "never"
`, world, image, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), tmuxCopies, configSource, revisionSource, providerHost, providerPort, telegramHost, telegramPort, gateway)
	mustWrite(t, path, []byte(body), 0o600)
}

func hermesTmuxCopies(t *testing.T) string {
	t.Helper()
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	resolvedTmux, err := filepath.EvalSymlinks(tmux)
	if err != nil {
		t.Fatal(err)
	}
	blocks := []string{fmt.Sprintf("[[copies]]\nsource = %q\ntarget = \"/usr/local/bin/tmux\"\nmode = \"0755\"", resolvedTmux)}
	out, err := exec.Command("ldd", resolvedTmux).Output()
	if err != nil {
		t.Fatalf("inspect tmux shared libraries: %v", err)
	}
	wanted := map[string]bool{"libevent_core": false, "libtinfo": false, "libutempter": false}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "=>" || !filepath.IsAbs(fields[2]) {
			continue
		}
		for prefix := range wanted {
			if !strings.HasPrefix(fields[0], prefix) {
				continue
			}
			resolved, err := filepath.EvalSymlinks(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			target := fields[2]
			blocks = append(blocks, fmt.Sprintf("[[copies]]\nsource = %q\ntarget = %q\nmode = \"0644\"", resolved, target))
			wanted[prefix] = true
		}
	}
	for library, found := range wanted {
		if !found {
			t.Fatalf("tmux dependency %s was not resolved by ldd", library)
		}
	}
	return strings.Join(blocks, "\n")
}

func shellJoin(command []string) string {
	quoted := make([]string, len(command))
	for i, argument := range command {
		quoted[i] = shellQuote(argument)
	}
	return strings.Join(quoted, " ")
}

func hermesEnvCommand(args ...string) []string {
	command := []string{"/usr/bin/env", "HOME=/workspace/.hermes", "HERMES_HOME=/workspace/.hermes", "HERMES_WRITE_SAFE_ROOT=/workspace", "HERMES_DISABLE_LAZY_INSTALLS=1", "HERMES_TELEGRAM_DISABLE_FALLBACK_IPS=1", "HERMES_YOLO_MODE=1", "HERMES_ACCEPT_HOOKS=1", "TELEGRAM_BOT_TOKEN=" + telegramFixtureToken, "TELEGRAM_ALLOWED_USERS=" + strconv.FormatInt(telegramFixtureUser, 10), "TERM=xterm-256color"}
	return append(command, args...)
}

func waitForHermesTelegramAfter(t *testing.T, fixture *telegramFixture, timeout time.Duration, getMeCount, getUpdatesCount int) {
	t.Helper()
	fixture.waitForMethodAfter(t, timeout, "getMe", getMeCount)
	fixture.waitForMethodAfter(t, timeout, "getUpdates", getUpdatesCount)
}

func assertHermesVersion(t *testing.T, ctx context.Context, dir string, env []string, container, version string) {
	t.Helper()
	command := append([]string{"exec", container}, hermesEnvCommand("/opt/hermes/.venv/bin/hermes", "--version")...)
	out := strings.TrimSpace(run(t, ctx, dir, env, "podman", command...))
	if !strings.Contains(out, "Hermes Agent v"+version) {
		t.Fatalf("in-world Hermes version = %q, want %q", out, version)
	}
}

func assertHermesIsolation(t *testing.T, ctx context.Context, dir string, env []string, container, providerHost string, providerPort int) {
	t.Helper()
	if out, err := runResult(ctx, dir, env, "podman", "exec", container, "/usr/bin/env", "-u", "HTTP_PROXY", "-u", "HTTPS_PROXY", "-u", "ALL_PROXY", "-u", "http_proxy", "-u", "https_proxy", "-u", "all_proxy", "curl", "--fail", "--max-time", "2", fmt.Sprintf("http://%s:%d/healthz", providerHost, providerPort)); err == nil {
		t.Fatalf("provider was directly reachable without Kenogram door: %s", out)
	}
	if out, err := runResult(ctx, dir, env, "podman", "exec", container, "test", "!", "-e", "/run/podman/podman.sock"); err != nil {
		t.Fatalf("runtime socket visible: %s", out)
	}
}

func runHermesTUI(t *testing.T, ctx context.Context, dir string, env []string, container string, provider *observedProvider, session, marker string) {
	t.Helper()
	_, _ = runResult(ctx, dir, env, "podman", "exec", container, "tmux", "kill-session", "-t", session)
	command := hermesEnvCommand("/opt/hermes/.venv/bin/hermes", "--tui", "--ignore-rules", "--provider", "custom", "--model", "proof")
	args := append([]string{"exec", container, "tmux", "new-session", "-d", "-x", "120", "-y", "40", "-s", session}, command...)
	run(t, ctx, dir, env, "podman", args...)
	waitForHermesTUIReady(t, ctx, dir, env, container, session)
	prompt := "Reply with the proof marker and preserve this request marker: " + marker
	run(t, ctx, dir, env, "podman", "exec", container, "tmux", "send-keys", "-t", session, "-l", prompt)
	run(t, ctx, dir, env, "podman", "exec", container, "tmux", "send-keys", "-t", session, "Enter")
	// The fake provider records the exact request, proving terminal input crossed
	// Hermes's TUI and embedded gateway before the rendered response is asserted.
	provider.waitObservedContaining(t, 45*time.Second, marker)
	waitFor(t, 45*time.Second, func() (bool, string) {
		out, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", session)
		return err == nil && strings.Contains(out, hermesProof), out
	})
}

func waitForHermesTUIReady(t *testing.T, ctx context.Context, dir string, env []string, container, target string) {
	t.Helper()
	waitFor(t, 60*time.Second, func() (bool, string) {
		pane, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "capture-pane", "-p", "-t", target)
		if err != nil {
			return false, pane
		}
		paneTTY, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "display-message", "-p", "-t", target, "#{pane_tty}")
		if err != nil {
			return false, paneTTY
		}
		modes, err := runResult(ctx, dir, env, "podman", "exec", container, "stty", "-F", strings.TrimSpace(paneTTY), "-a")
		if err != nil {
			return false, modes
		}
		ready := strings.Contains(pane, "ready") && strings.Contains(modes, "-icanon")
		return ready, fmt.Sprintf("terminal modes:\n%s\npane:\n%s", modes, pane)
	})
}

func hermesTelegramAPIBase(rawURL, host string) string {
	return telegramAPIBase(rawURL, host)
}

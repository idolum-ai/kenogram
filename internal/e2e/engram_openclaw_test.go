//go:build linux

package e2e

import (
	"context"
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

func TestEngramControlsOpenClawInsideKenogram(t *testing.T) {
	if os.Getenv("KENOGRAM_ENGRAM_OPENCLAW_E2E") != "1" {
		t.Skip("set KENOGRAM_ENGRAM_OPENCLAW_E2E=1 to run the Engram/OpenClaw composition proof")
	}
	if runtime.GOARCH != "amd64" {
		t.Fatalf("locked Engram fixture requires linux/amd64, got linux/%s", runtime.GOARCH)
	}
	requireExecutable(t, "podman")
	requireExecutable(t, "nsenter")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	doorHost := hostDoorIPv4(t)
	provider := newOpenAIProvider(t, doorHost)
	defer provider.Close()
	telegram := newTelegramFixture(t, doorHost)
	defer telegram.Close()
	providerPort := mustPort(t, provider.URL)
	telegramPort := mustPort(t, telegram.URL)
	root := repositoryRoot(t)
	tmp := t.TempDir()
	openClaw := readOpenClawLock(t)
	engramLock := readReleaseLock(t)
	t.Logf("evidence engram=%s@%s openclaw=%s image=%s", engramLock.Version, engramLock.Commit, openClaw.Version, openClaw.Image)
	engram := materializeEngramRelease(t, ctx, tmp, engramLock)

	containerfile := filepath.Join(tmp, "Containerfile")
	containerBody := fmt.Sprintf(`FROM %s
USER root
RUN apt-get update && apt-get install --no-install-recommends -y tmux curl procps && rm -rf /var/lib/apt/lists/*
ARG KENOGRAM_UID
ARG KENOGRAM_GID
RUN getent group "$KENOGRAM_GID" >/dev/null || groupadd --gid "$KENOGRAM_GID" kenogram-test; printf 'kenogram:x:%%s:%%s:Kenogram composition:/workspace:/bin/sh\n' "$KENOGRAM_UID" "$KENOGRAM_GID" >> /etc/passwd
USER node
`, openClaw.Image)
	mustWrite(t, containerfile, []byte(containerBody), 0o600)
	image := "localhost/kenogram-engram-openclaw-e2e:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	run(t, ctx, tmp, nil, "podman", "build", "--pull=missing", "--build-arg", "KENOGRAM_UID="+strconv.Itoa(os.Getuid()), "--build-arg", "KENOGRAM_GID="+strconv.Itoa(os.Getgid()), "-t", image, "-f", containerfile, ".")
	imageDigest := strings.TrimSpace(run(t, ctx, tmp, nil, "podman", "image", "inspect", "--format", "{{.Digest}}", image))
	if !strings.HasPrefix(imageDigest, "sha256:") {
		t.Fatalf("invalid composition image digest: %q", imageDigest)
	}
	pinnedImage := image + "@" + imageDigest

	world := "engram-openclaw-e2e-" + strconv.Itoa(os.Getpid())
	stateRoot := filepath.Join(tmp, "state")
	openClawConfig := filepath.Join(tmp, "openclaw.json")
	engramEnv := filepath.Join(tmp, "engram.env")
	declaration := filepath.Join(tmp, "kenogram.toml")
	kenogram := filepath.Join(tmp, "kenogram")
	run(t, ctx, root, append(os.Environ(), "CGO_ENABLED=0"), "go", "build", "-buildvcs=false", "-o", kenogram, "./cmd/kenogram")
	testEnv := append(os.Environ(), "KENOGRAM_STATE_DIR="+stateRoot)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", containerName(world, 1)).Run()
		_ = exec.CommandContext(cleanupCtx, "podman", "rmi", "--force", image).Run()
	})

	writeOpenClawConfig(t, openClawConfig, doorHost, providerPort, "")
	writeEngramCompositionEnv(t, engramEnv, telegramFixtureToken, telegramAPIBase(telegram.URL, doorHost), telegramFixtureUser, telegramFixtureUser)
	writeEngramOpenClawDeclaration(t, declaration, world, pinnedImage, engram, openClawConfig, engramEnv, doorHost, providerPort, doorHost, telegramPort)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	container := containerName(world, 1)
	waitForOpenClaw(t, ctx, tmp, testEnv, container)
	assertOpenClawVersion(t, ctx, tmp, testEnv, container, openClaw.Version)
	waitForTmuxTarget(t, ctx, tmp, testEnv, container, "main:openclaw")
	waitForOpenClawTUIReady(t, ctx, tmp, testEnv, container, "main:openclaw")
	waitForEngramPolling(t, telegram, 20*time.Second)
	assertOpenClawCompositionVersions(t, ctx, tmp, testEnv, container, openClaw.Version)

	telegram.enqueueText("/attach main:openclaw")
	telegram.waitOutbound(t, 20*time.Second, "attached existing tmux target")
	telegram.enqueueText("/send 1 Reply with the proof marker.")
	if !provider.observedWithin(30 * time.Second) {
		pane, _ := runResult(ctx, tmp, testEnv, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", "main:openclaw")
		audit, _ := runResult(ctx, tmp, testEnv, "podman", "exec", container, "cat", "/workspace/.engram/audit.jsonl")
		state, _ := runResult(ctx, tmp, testEnv, "podman", "exec", container, "cat", "/workspace/.engram/state.json")
		diagnostics := openClawTerminalDiagnostics(ctx, tmp, testEnv, container)
		telegram.mu.Lock()
		outbound := append([]telegramOutbound(nil), telegram.outbound...)
		telegram.mu.Unlock()
		t.Fatalf("OpenClaw never reached the declared provider\npane:\n%s\nEngram state:\n%s\nEngram audit:\n%s\nTerminal boundary:\n%s\nTelegram outbound: %#v", pane, state, audit, diagnostics, outbound)
	}
	waitFor(t, 30*time.Second, func() (bool, string) {
		out, err := runResult(ctx, tmp, testEnv, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", "main:openclaw")
		return err == nil && strings.Contains(out, openClawProof), out
	})
	telegram.waitOutbound(t, 30*time.Second, openClawTelegramProof)

	telegram.enqueueDocument()
	telegram.waitForFileRequest(t, 20*time.Second)
	telegram.waitOutbound(t, 20*time.Second, "attachment saved")
	attachment := findAttachmentPath(t, ctx, tmp, testEnv, container)
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", container, "cat", attachment)); got != telegramFixtureFile {
		t.Fatalf("downloaded Telegram fixture = %q", got)
	}

	assertSecretAbsentOutsideWorkspace(t, filepath.Join(stateRoot, world), openClawSecretCanary)
	assertSecretAbsent(t, filepath.Join(stateRoot, world), secretCanary)
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	assertDestroyedOutcomes(t, stateRoot, world, "applied", "destroyed")
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), openClawSecretCanary)
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), secretCanary)
}

func materializeEngramRelease(t *testing.T, ctx context.Context, tmp string, lock releaseLock) string {
	t.Helper()
	archive := filepath.Join(tmp, lock.Asset)
	if supplied := os.Getenv("KENOGRAM_ENGRAM_ARCHIVE"); supplied != "" {
		copyRegularFile(t, supplied, archive, 0o600)
		if os.Getenv("KENOGRAM_VERIFY_UPSTREAM") == "1" {
			verifyPublishedChecksum(t, ctx, lock)
		}
	} else {
		verifyPublishedChecksum(t, ctx, lock)
		download(t, ctx, lock.AssetURL, archive)
	}
	verifyFileDigest(t, archive, lock.SHA256)
	engram := extractEngram(t, archive, tmp)
	version := run(t, ctx, tmp, nil, engram, "version")
	if want := "engram " + lock.Version + " commit=" + lock.Commit; !strings.Contains(version, want) {
		t.Fatalf("embedded release identity = %q, want %q", strings.TrimSpace(version), want)
	}
	return engram
}

func writeEngramCompositionEnv(t *testing.T, path, token, apiBase string, allowedUserID, chatID int64) {
	t.Helper()
	body := fmt.Sprintf("TELEGRAM_BOT_TOKEN=%s\nTELEGRAM_API_BASE=%s\nTELEGRAM_ALLOWED_USER_ID=%d\nTELEGRAM_CHAT_ID=%d\nTELEGRAM_POLL_TIMEOUT_SECONDS=1\nLLM_PROVIDER=anthropic\nANTHROPIC_API_KEY=%s\nANTHROPIC_MODEL=claude-haiku-4-5-20251001\nENGRAM_HOME=/workspace/.engram\nENGRAM_WORKDIR=/workspace\nENGRAM_TMUX_SESSION=main\nENGRAM_ANCHOR_MODE=guide\n", token, apiBase, allowedUserID, chatID, secretCanary)
	mustWrite(t, path, []byte(body), 0o600)
}

func writeEngramOpenClawDeclaration(t *testing.T, path, world, image, engram, openClawConfig, engramEnv, providerHost string, providerPort int, telegramHost string, telegramPort int) {
	t.Helper()
	gateway := openClawEnvCommand("/usr/local/bin/openclaw", "gateway", "--port", "18789", "--verbose")
	gatewayJSON, err := json.Marshal(gateway)
	if err != nil {
		t.Fatal(err)
	}
	tuiShell := strings.Join(openClawEnvCommand("/usr/local/bin/openclaw", "tui", "--url", "ws://127.0.0.1:18789", "--token", openClawGatewayToken, "--session", "engram-composition", "--timeout-ms", "20000"), " ")
	tmuxCommand := []string{"/bin/sh", "-c", "until /usr/bin/curl --fail --silent http://127.0.0.1:18789/readyz >/dev/null; do sleep 0.1; done; cd /workspace && /usr/bin/tmux new-session -d -x 120 -y 40 -s main -n openclaw " + shellQuote(tuiShell) + " && exec /usr/bin/tmux wait-for kenogram-service-stop"}
	tmuxJSON, err := json.Marshal(tmuxCommand)
	if err != nil {
		t.Fatal(err)
	}
	engramCommand, err := json.Marshal([]string{"/usr/local/bin/engram", "run", "--env", "/etc/engram.env"})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`version = 1
name = %q
[world]
hostname = "engram-openclaw-e2e"
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
target = "/usr/local/bin/engram"
mode = "0755"
[[copies]]
source = %q
target = "/etc/openclaw.json"
mode = "0600"
secret = true
[[copies]]
source = %q
target = "/etc/engram.env"
mode = "0600"
secret = true
[[network.allow]]
host = %q
port = %d
[[network.allow]]
host = %q
port = %d
[[services]]
name = "openclaw-gateway"
command = %s
autostart = true
restart = "never"
[[services]]
name = "openclaw-tui"
command = %s
autostart = true
restart = "never"
[[services]]
name = "engram"
command = %s
autostart = true
restart = "never"
`, world, image, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), engram, openClawConfig, engramEnv, providerHost, providerPort, telegramHost, telegramPort, gatewayJSON, tmuxJSON, engramCommand)
	mustWrite(t, path, []byte(body), 0o600)
}

func waitForTmuxTarget(t *testing.T, ctx context.Context, dir string, env []string, container, target string) {
	t.Helper()
	waitFor(t, 30*time.Second, func() (bool, string) {
		out, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "display-message", "-p", "-t", target, "#{pane_id}")
		return err == nil && strings.HasPrefix(strings.TrimSpace(out), "%"), out
	})
}

func waitForOpenClawTUIReady(t *testing.T, ctx context.Context, dir string, env []string, container, target string) {
	t.Helper()
	waitFor(t, 45*time.Second, func() (bool, string) {
		pane, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "capture-pane", "-p", "-t", target)
		if err != nil {
			return false, pane
		}
		paneTTY, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "display-message", "-p", "-t", target, "#{pane_tty}")
		if err != nil {
			return false, paneTTY
		}
		paneTTY = strings.TrimSpace(paneTTY)
		modes, err := runResult(ctx, dir, env, "podman", "exec", container, "stty", "-F", paneTTY, "-a")
		if err != nil {
			return false, modes
		}
		ready := strings.Contains(pane, "gateway connected | idle") &&
			strings.Contains(modes, "-icanon") &&
			strings.Contains(modes, "-icrnl")
		return ready, fmt.Sprintf("pane tty: %s\nterminal modes:\n%s\npane:\n%s", paneTTY, modes, pane)
	})
}

func assertOpenClawCompositionVersions(t *testing.T, ctx context.Context, dir string, env []string, container, openClawVersion string) {
	t.Helper()
	command := `printf 'tmux='; tmux -V; printf 'openclaw='; /usr/local/bin/openclaw --version; printf 'pi-tui='; node -p "require('/app/node_modules/@earendil-works/pi-tui/package.json').version"`
	out := run(t, ctx, dir, env, "podman", "exec", container, "/bin/sh", "-c", command)
	if !strings.Contains(out, "openclaw=OpenClaw "+openClawVersion) || !strings.Contains(out, "pi-tui=0.78.0") {
		t.Fatalf("unexpected composition versions:\n%s", out)
	}
	t.Logf("composition versions:\n%s", strings.TrimSpace(out))
}

func openClawTerminalDiagnostics(ctx context.Context, dir string, env []string, container string) string {
	command := `
printf '%s\n' '--- versions ---'
tmux -V
/usr/local/bin/openclaw --version
printf 'pi-tui '
node -p "require('/app/node_modules/@earendil-works/pi-tui/package.json').version"
printf '%s\n' '--- tmux options ---'
for option in default-terminal extended-keys extended-keys-format terminal-features terminal-overrides; do
  value=$(tmux show-options -Agv "$option" 2>/dev/null || tmux show-options -Asgv "$option" 2>/dev/null || true)
  printf '%s=%s\n' "$option" "$value"
done
tmux display-message -p -t main:openclaw 'pane=#{pane_id} tty=#{pane_tty} term=#{client_termname} features=#{client_termfeatures}' || true
printf '%s\n' '--- TUI process terminal state ---'
for pid in $(pgrep -f 'openclaw.*tui'); do
  tty=$(readlink "/proc/$pid/fd/0" 2>/dev/null || true)
  case "$tty" in
    /dev/pts/*)
      printf 'pid=%s stdin=%s\n' "$pid" "$tty"
      tr '\000' '\n' < "/proc/$pid/environ" | grep -E '^(TERM|COLORTERM|TERM_PROGRAM|KITTY_WINDOW_ID|TMUX)=' || true
      stty -a < "/proc/$pid/fd/0" || true
      for fd in "/proc/$pid/fd"/*; do
        target=$(readlink "$fd" 2>/dev/null || true)
        case "$target" in /dev/pts/*) printf 'tty-fd=%s target=%s\n' "${fd##*/}" "$target";; esac
      done
      ;;
  esac
done
`
	out, err := runResult(ctx, dir, env, "podman", "exec", container, "/bin/sh", "-c", command)
	if err != nil {
		return fmt.Sprintf("diagnostic command failed: %v\n%s", err, out)
	}
	return out
}

func waitForEngramPolling(t *testing.T, fixture *telegramFixture, timeout time.Duration) {
	t.Helper()
	fixture.waitForMethod(t, timeout, "setMyCommands")
	fixture.waitForMethod(t, timeout, "getUpdates")
}

func findAttachmentPath(t *testing.T, ctx context.Context, dir string, env []string, container string) string {
	t.Helper()
	out := strings.Fields(run(t, ctx, dir, env, "podman", "exec", container, "find", "/tmp", "-path", "*/engram-*/attachments/*proof.txt", "-type", "f", "-print"))
	if len(out) != 1 {
		t.Fatalf("attachment files = %q", out)
	}
	return out[0]
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

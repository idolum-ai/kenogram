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

const hermesEngramProof = "KENOGRAM_ENGRAM_HERMES_PROOF_27fddad8"
const hermesEngramSignalID = "76543210fedcba9876543210fedcba98"

func TestEngramControlsHermesInsideKenogram(t *testing.T) {
	if os.Getenv("KENOGRAM_ENGRAM_HERMES_E2E") != "1" {
		t.Skip("set KENOGRAM_ENGRAM_HERMES_E2E=1 to run the Engram/Hermes composition proof")
	}
	if runtime.GOARCH != "amd64" {
		t.Fatalf("locked Engram fixture requires linux/amd64, got linux/%s", runtime.GOARCH)
	}
	requireExecutable(t, "podman")
	requireExecutable(t, "nsenter")

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	doorHost := hostDoorIPv4(t)
	providerProof := hermesProof + "\n[engram:upstream] " + hermesEngramSignalID + " " + hermesEngramProof
	provider := newObservedProvider(t, doorHost, providerProof)
	defer provider.Close()
	telegram := newTelegramFixture(t, doorHost)
	defer telegram.Close()
	providerPort := mustPort(t, provider.URL)
	telegramPort := mustPort(t, telegram.URL)
	root := repositoryRoot(t)
	tmp := t.TempDir()
	hermes := readHermesLock(t)
	verifyHermesArtifact(t, ctx, tmp, hermes)
	image := hermes.Image
	engramLock := readReleaseLock(t)
	engram := materializeEngramRelease(t, ctx, tmp, engramLock)

	world := "engram-hermes-e2e-" + strconv.Itoa(os.Getpid())
	stateRoot := filepath.Join(tmp, "state")
	hermesConfig := filepath.Join(tmp, "hermes-config.yaml")
	engramEnv := filepath.Join(tmp, "engram.env")
	declaration := filepath.Join(tmp, "kenogram.toml")
	kenogram := filepath.Join(tmp, "kenogram")
	run(t, ctx, root, append(os.Environ(), "CGO_ENABLED=0"), "go", "build", "-o", kenogram, "./cmd/kenogram")
	testEnv := append(os.Environ(), "KENOGRAM_STATE_DIR="+stateRoot)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", containerName(world, 1)).Run()
	})

	writeHermesConfig(t, hermesConfig, doorHost, providerPort, "")
	writeEngramCompositionEnv(t, engramEnv, telegramFixtureToken, telegramAPIBase(telegram.URL, doorHost), telegramFixtureUser, telegramFixtureUser)
	writeEngramHermesDeclaration(t, declaration, world, image, engram, hermesConfig, engramEnv, doorHost, providerPort, doorHost, telegramPort)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	container := containerName(world, 1)
	waitForTmuxTarget(t, ctx, tmp, testEnv, container, "main:hermes")
	waitForHermesTUIReady(t, ctx, tmp, testEnv, container, "main:hermes")
	waitForEngramPolling(t, telegram, 30*time.Second)
	assertHermesCompositionVersions(t, ctx, tmp, testEnv, container, hermes.Version)

	telegram.enqueueText("/attach main:hermes")
	telegram.waitOutbound(t, 30*time.Second, "attached existing tmux target")
	promptMarker := "HERMES_ENGRAM_TUI_INPUT_cbcf9742"
	telegram.enqueueText("/send 1 Reply with the proof marker and preserve this request marker: " + promptMarker)
	if !provider.observedContainingWithin(45*time.Second, promptMarker) {
		pane, _ := runResult(ctx, tmp, testEnv, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", "main:hermes")
		audit, _ := runResult(ctx, tmp, testEnv, "podman", "exec", container, "cat", "/workspace/.engram/audit.jsonl")
		state, _ := runResult(ctx, tmp, testEnv, "podman", "exec", container, "cat", "/workspace/.engram/state.json")
		t.Fatalf("Hermes never received Engram's terminal input\npane:\n%s\nEngram state:\n%s\nEngram audit:\n%s", pane, state, audit)
	}
	waitFor(t, 45*time.Second, func() (bool, string) {
		out, err := runResult(ctx, tmp, testEnv, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", "main:hermes")
		return err == nil && strings.Contains(out, hermesProof), out
	})
	telegram.waitOutbound(t, 45*time.Second, hermesEngramProof)

	telegram.enqueueDocument()
	telegram.waitForFileRequest(t, 30*time.Second)
	telegram.waitOutbound(t, 30*time.Second, "attachment saved")
	attachment := findAttachmentPath(t, ctx, tmp, testEnv, container)
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", container, "cat", attachment)); got != telegramFixtureFile {
		t.Fatalf("downloaded Telegram fixture = %q", got)
	}

	assertSecretAbsentOutsideWorkspace(t, filepath.Join(stateRoot, world), hermesSecretCanary)
	assertSecretAbsent(t, filepath.Join(stateRoot, world), secretCanary)
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	assertDestroyedOutcomes(t, stateRoot, world, "applied", "destroyed")
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), hermesSecretCanary)
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), secretCanary)
}

func writeEngramHermesDeclaration(t *testing.T, path, world, image, engram, hermesConfig, engramEnv, providerHost string, providerPort int, telegramHost string, telegramPort int) {
	t.Helper()
	tmuxCopies := hermesTmuxCopies(t)
	tuiShell := shellJoin(hermesEnvCommand("/opt/hermes/.venv/bin/hermes", "--tui", "--ignore-rules"))
	bootstrap := "mkdir -p /workspace/.hermes && cp /etc/hermes-config.yaml /workspace/.hermes/config.yaml && chmod 0600 /workspace/.hermes/config.yaml"
	tmuxCommand := []string{"/bin/sh", "-c", bootstrap + " && cd /workspace && /usr/local/bin/tmux new-session -d -x 120 -y 40 -s main -n hermes " + shellQuote(tuiShell) + " && exec /usr/local/bin/tmux wait-for kenogram-service-stop"}
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
hostname = "engram-hermes-e2e"
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
target = "/usr/local/bin/engram"
mode = "0755"
[[copies]]
source = %q
target = "/etc/hermes-config.yaml"
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
name = "hermes-tui"
command = %s
autostart = true
restart = "never"
[[services]]
name = "engram"
command = %s
autostart = true
restart = "never"
`, world, image, fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), tmuxCopies, engram, hermesConfig, engramEnv, providerHost, providerPort, telegramHost, telegramPort, tmuxJSON, engramCommand)
	mustWrite(t, path, []byte(body), 0o600)
}

func assertHermesCompositionVersions(t *testing.T, ctx context.Context, dir string, env []string, container, hermesVersion string) {
	t.Helper()
	command := `printf 'tmux='; tmux -V; printf 'hermes='; HOME=/workspace/.hermes HERMES_HOME=/workspace/.hermes /opt/hermes/.venv/bin/hermes --version; printf 'node='; node --version`
	out := run(t, ctx, dir, env, "podman", "exec", container, "/bin/sh", "-c", command)
	if !strings.Contains(out, "hermes=Hermes Agent v"+hermesVersion) || !strings.Contains(out, "node=v22.") {
		t.Fatalf("unexpected composition versions:\n%s", out)
	}
	t.Logf("composition versions:\n%s", strings.TrimSpace(out))
}

func (p *observedProvider) observedContainingWithin(timeout time.Duration, fragment string) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, body := range p.bodies {
			if strings.Contains(body, fragment) {
				p.mu.Unlock()
				return true
			}
		}
		p.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

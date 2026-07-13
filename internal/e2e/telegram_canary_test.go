//go:build linux

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLiveTelegramOpenClawCanary(t *testing.T) {
	if os.Getenv("KENOGRAM_LIVE_TELEGRAM") != "1" {
		t.Skip("set KENOGRAM_LIVE_TELEGRAM=1 to run the operator-assisted Telegram canary")
	}
	if runtime.GOARCH != "amd64" {
		t.Fatalf("locked Engram fixture requires linux/amd64, got linux/%s", runtime.GOARCH)
	}
	token := requiredCanaryEnv(t, "KENOGRAM_TELEGRAM_BOT_TOKEN")
	allowedUserID := requiredCanaryInt64(t, "KENOGRAM_TELEGRAM_ALLOWED_USER_ID")
	chatID := allowedUserID
	if value := strings.TrimSpace(os.Getenv("KENOGRAM_TELEGRAM_CHAT_ID")); value != "" {
		var err error
		chatID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || chatID == 0 {
			t.Fatalf("KENOGRAM_TELEGRAM_CHAT_ID must be a nonzero integer")
		}
	}
	nonce := requiredCanaryEnv(t, "KENOGRAM_TELEGRAM_CANARY_NONCE")
	requireExecutable(t, "podman")
	requireExecutable(t, "nsenter")

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	doorHost := hostDoorIPv4(t)
	provider := newOpenAIProvider(t, doorHost)
	defer provider.Close()
	providerPort := mustPort(t, provider.URL)
	root := repositoryRoot(t)
	tmp := t.TempDir()
	openClaw := readOpenClawLock(t)
	engramLock := readReleaseLock(t)
	t.Logf("evidence engram=%s@%s openclaw=%s image=%s", engramLock.Version, engramLock.Commit, openClaw.Version, openClaw.Image)
	engram := materializeEngramRelease(t, ctx, tmp, engramLock)

	containerfile := filepath.Join(tmp, "Containerfile")
	containerBody := fmt.Sprintf(`FROM %s
USER root
RUN apt-get update && apt-get install --no-install-recommends -y tmux curl procps ca-certificates && rm -rf /var/lib/apt/lists/*
ARG KENOGRAM_UID
ARG KENOGRAM_GID
RUN getent group "$KENOGRAM_GID" >/dev/null || groupadd --gid "$KENOGRAM_GID" kenogram-test; printf 'kenogram:x:%%s:%%s:Kenogram live canary:/workspace:/bin/sh\n' "$KENOGRAM_UID" "$KENOGRAM_GID" >> /etc/passwd
USER node
`, openClaw.Image)
	mustWrite(t, containerfile, []byte(containerBody), 0o600)
	image := "localhost/kenogram-telegram-canary:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	run(t, ctx, tmp, nil, "podman", "build", "--pull=missing", "--build-arg", "KENOGRAM_UID="+strconv.Itoa(os.Getuid()), "--build-arg", "KENOGRAM_GID="+strconv.Itoa(os.Getgid()), "-t", image, "-f", containerfile, ".")
	imageDigest := strings.TrimSpace(run(t, ctx, tmp, nil, "podman", "image", "inspect", "--format", "{{.Digest}}", image))
	if !strings.HasPrefix(imageDigest, "sha256:") {
		t.Fatalf("invalid live-canary image digest: %q", imageDigest)
	}

	world := "telegram-canary-" + strconv.Itoa(os.Getpid())
	stateRoot := filepath.Join(tmp, "state")
	openClawConfig := filepath.Join(tmp, "openclaw.json")
	engramEnv := filepath.Join(tmp, "engram.env")
	declaration := filepath.Join(tmp, "kenogram.toml")
	kenogram := filepath.Join(tmp, "kenogram")
	run(t, ctx, root, append(os.Environ(), "CGO_ENABLED=0"), "go", "build", "-o", kenogram, "./cmd/kenogram")
	testEnv := append(os.Environ(), "KENOGRAM_STATE_DIR="+stateRoot)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", containerName(world, 1)).Run()
		_ = exec.CommandContext(cleanupCtx, "podman", "rmi", "--force", image).Run()
	})

	writeOpenClawConfig(t, openClawConfig, doorHost, providerPort)
	writeEngramCompositionEnv(t, engramEnv, token, "https://api.telegram.org", allowedUserID, chatID)
	writeEngramOpenClawDeclaration(t, declaration, world, image+"@"+imageDigest, engram, openClawConfig, engramEnv, doorHost, providerPort, "api.telegram.org", 443)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	container := containerName(world, 1)
	waitForOpenClaw(t, ctx, tmp, testEnv, container)
	assertOpenClawVersion(t, ctx, tmp, testEnv, container, openClaw.Version)
	waitForTmuxTarget(t, ctx, tmp, testEnv, container, "main:openclaw")

	instructions := fmt.Sprintf("Kenogram canary ready. Within 3 minutes send:\n/attach main:openclaw\n/text 1 Reply with %s\n/key 1 KPEnter", nonce)
	notifyCanaryOperator(t, ctx, token, chatID, instructions)
	t.Log(instructions)
	provider.waitObservedContaining(t, 3*time.Minute, nonce)
	waitFor(t, 45*time.Second, func() (bool, string) {
		out, err := runResult(ctx, tmp, testEnv, "podman", "exec", container, "tmux", "capture-pane", "-p", "-e", "-t", "main:openclaw")
		return err == nil && strings.Contains(out, openClawProof), out
	})
	waitFor(t, 45*time.Second, func() (bool, string) {
		out, err := runResult(ctx, tmp, testEnv, "podman", "exec", container, "cat", "/workspace/.engram/audit.jsonl")
		return err == nil && strings.Contains(out, "terminal.upstream_signal") && strings.Contains(out, "delivered") && strings.Contains(out, openClawTelegramProof), out
	})
	assertSecretAbsent(t, filepath.Join(stateRoot, world), token)
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	assertDestroyedOutcomes(t, stateRoot, world, "applied", "destroyed")
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), token)
}

func requiredCanaryEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required when KENOGRAM_LIVE_TELEGRAM=1", name)
	}
	return value
}

func requiredCanaryInt64(t *testing.T, name string) int64 {
	t.Helper()
	value := requiredCanaryEnv(t, name)
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed == 0 {
		t.Fatalf("%s must be a nonzero integer", name)
	}
	return parsed
}

func notifyCanaryOperator(t *testing.T, ctx context.Context, token string, chatID int64, text string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		t.Fatal("create Telegram canary notification request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("notify Telegram canary operator: transport request failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("notify Telegram canary operator: HTTP %d", response.StatusCode)
	}
}

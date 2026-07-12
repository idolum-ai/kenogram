//go:build linux

package e2e

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const debianBookwormAMD64 = "docker.io/library/debian@sha256:1def178129dfb5f24db43afbf2fcac04530012e3264ba4ff81c71184e17a9ee4"
const secretCanary = "kenogram-canary-7f51c210b2d946e4a83572fd94d8f567"

type releaseLock struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	Asset        string `json:"asset"`
	SHA256       string `json:"sha256"`
	ReleaseURL   string `json:"release_url"`
	AssetURL     string `json:"asset_url"`
	ChecksumsURL string `json:"checksums_url"`
}

func TestEngramReleaseLock(t *testing.T) {
	lock := readReleaseLock(t)
	if lock.Version != "v0.1.0" || lock.Commit != "296d8e36f367" {
		t.Fatalf("unexpected Engram identity: %#v", lock)
	}
	if len(lock.SHA256) != sha256.Size*2 {
		t.Fatalf("invalid SHA-256 length: %q", lock.SHA256)
	}
	if _, err := hex.DecodeString(lock.SHA256); err != nil {
		t.Fatalf("invalid SHA-256: %v", err)
	}
	if !strings.Contains(lock.Asset, lock.Version) || !strings.HasSuffix(lock.Asset, "-linux-amd64.tar.gz") {
		t.Fatalf("asset does not match locked release: %q", lock.Asset)
	}
	for name, value := range map[string]string{"release_url": lock.ReleaseURL, "asset_url": lock.AssetURL, "checksums_url": lock.ChecksumsURL} {
		if !strings.HasPrefix(value, "https://github.com/idolum-ai/engram/releases/") {
			t.Fatalf("%s leaves the Engram release boundary: %q", name, value)
		}
	}
}

func TestEngramReleaseInsideKenogram(t *testing.T) {
	if os.Getenv("KENOGRAM_E2E") != "1" {
		t.Skip("set KENOGRAM_E2E=1 to run the Engram release acceptance test")
	}
	if runtime.GOARCH != "amd64" {
		t.Fatalf("locked Engram fixture requires linux/amd64, got linux/%s", runtime.GOARCH)
	}
	requireExecutable(t, "podman")
	requireExecutable(t, "nsenter")

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	root := repositoryRoot(t)
	tmp := t.TempDir()
	lock := readReleaseLock(t)
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
	wantVersion := "engram " + lock.Version + " commit=" + lock.Commit
	if !strings.Contains(version, wantVersion) {
		t.Fatalf("embedded release identity = %q, want %q", strings.TrimSpace(version), wantVersion)
	}

	containerfile := filepath.Join(tmp, "Containerfile")
	containerBody := "FROM " + debianBookwormAMD64 + "\n" +
		"RUN apt-get update && apt-get install --no-install-recommends -y tmux=3.3a-3 && rm -rf /var/lib/apt/lists/*\n" +
		fmt.Sprintf("RUN printf 'kenogram:x:%d:%d:Kenogram E2E:/workspace:/bin/sh\\n' >> /etc/passwd && printf 'kenogram:x:%d:\\n' >> /etc/group\n", os.Getuid(), os.Getgid(), os.Getgid())
	mustWrite(t, containerfile, []byte(containerBody), 0o600)
	image := "localhost/kenogram-engram-e2e:" + strconv.FormatInt(time.Now().UnixNano(), 10)
	run(t, ctx, tmp, nil, "podman", "build", "--pull=missing", "-t", image, "-f", containerfile, ".")
	imageDigest := strings.TrimSpace(run(t, ctx, tmp, nil, "podman", "image", "inspect", "--format", "{{.Digest}}", image))
	if !strings.HasPrefix(imageDigest, "sha256:") || len(imageDigest) != len("sha256:")+sha256.Size*2 {
		t.Fatalf("invalid built image digest: %q", imageDigest)
	}
	pinnedImage := image + "@" + imageDigest

	world := "engram-e2e-" + strconv.Itoa(os.Getpid())
	stateRoot := filepath.Join(tmp, "state")
	declaration := filepath.Join(tmp, "kenogram.toml")
	envSource := filepath.Join(tmp, "engram.env")
	revisionSource := filepath.Join(tmp, "revision")
	kenogram := filepath.Join(tmp, "kenogram")
	buildEnv := append(os.Environ(), "CGO_ENABLED=0")
	run(t, ctx, root, buildEnv, "go", "build", "-o", kenogram, "./cmd/kenogram")
	testEnv := append(os.Environ(), "KENOGRAM_STATE_DIR="+stateRoot)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		for generation := 1; generation <= 3; generation++ {
			_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", containerName(world, generation)).Run()
		}
		_ = exec.CommandContext(cleanupCtx, "podman", "rmi", "--force", image).Run()
	})

	writeEngramEnv(t, envSource, 123)
	mustWrite(t, revisionSource, []byte("one\n"), 0o600)
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	writeDeclaration(t, declaration, world, pinnedImage, user, engram, envSource, revisionSource)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	first := containerName(world, 1)
	waitForTmux(t, ctx, tmp, testEnv, first)
	assertGeneratedContract(t, ctx, tmp, testEnv, first)
	assertSecretAbsent(t, filepath.Join(stateRoot, world), secretCanary)

	assertEngramOffline(t, ctx, tmp, testEnv, first, lock)
	firstStateDigest := inWorldSHA256(t, ctx, tmp, testEnv, first, "/workspace/.engram/state.json")
	run(t, ctx, tmp, testEnv, "podman", "exec", first, "tmux", "new-window", "-d", "-t", "main", "-n", "signal", "/usr/local/bin/engram signal fixture-ready; sleep 30")
	waitFor(t, 10*time.Second, func() (bool, string) {
		out, err := runResult(ctx, tmp, testEnv, "podman", "exec", first, "tmux", "capture-pane", "-p", "-t", "main:signal")
		return err == nil && strings.Contains(out, "[engram:upstream]") && strings.Contains(out, "fixture-ready"), out
	})
	run(t, ctx, tmp, testEnv, "podman", "exec", first, "/bin/sh", "-c", "printf carried > /workspace/sentinel")

	writeEngramEnv(t, envSource, 456)
	mustWrite(t, revisionSource, []byte("two\n"), 0o600)
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	second := containerName(world, 2)
	assertGeneratedContract(t, ctx, tmp, testEnv, second)
	assertSecretAbsent(t, filepath.Join(stateRoot, world), secretCanary)
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/workspace/sentinel")); got != "carried" {
		t.Fatalf("workspace sentinel after replacement = %q", got)
	}
	if got := inWorldSHA256(t, ctx, tmp, testEnv, second, "/workspace/.engram/state.json"); got != firstStateDigest {
		t.Fatalf("Engram state changed across replacement: %s -> %s", firstStateDigest, got)
	}
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/etc/engram-revision")); got != "two" {
		t.Fatalf("regenerated configuration = %q", got)
	}
	preflight := run(t, ctx, tmp, testEnv, "podman", "exec", second, "/usr/local/bin/engram", "preflight", "--env", "/etc/engram.env")
	if !strings.Contains(preflight, "telegram user: 456") || !strings.Contains(preflight, "status: ok") {
		t.Fatalf("replacement Engram preflight:\n%s", preflight)
	}
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", first); err == nil {
		t.Fatal("predecessor container survived successful replacement")
	}

	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	assertGeneration(t, filepath.Join(stateRoot, world, "state.json"), 2, "running")
	run(t, ctx, tmp, testEnv, kenogram, "down", world)
	assertGeneration(t, filepath.Join(stateRoot, world, "state.json"), 2, "down")
	run(t, ctx, tmp, testEnv, kenogram, "up", "--yes", declaration)
	assertGeneration(t, filepath.Join(stateRoot, world, "state.json"), 2, "running")
	waitForTmux(t, ctx, tmp, testEnv, second)
	if got := strings.TrimSpace(run(t, ctx, tmp, testEnv, "podman", "exec", second, "cat", "/workspace/sentinel")); got != "carried" {
		t.Fatalf("workspace sentinel after restart = %q", got)
	}
	run(t, ctx, tmp, testEnv, kenogram, "status", world)
	run(t, ctx, tmp, testEnv, kenogram, "destroy", "--yes", world)
	if _, err := runResult(ctx, tmp, testEnv, "podman", "inspect", second); err == nil {
		t.Fatal("container survived destroy")
	}
	assertDestroyedHistory(t, stateRoot, world)
	assertSecretAbsent(t, filepath.Join(stateRoot, ".destroyed"), secretCanary)
}

func readReleaseLock(t *testing.T) releaseLock {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "engram-v0.1.0.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock releaseLock
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	return lock
}

func verifyPublishedChecksum(t *testing.T, ctx context.Context, lock releaseLock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checksums.txt")
	download(t, ctx, lock.ChecksumsURL, path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := lock.SHA256 + "  " + lock.Asset
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("published checksums do not bind %s to %s", lock.Asset, lock.SHA256)
	}
}

func download(t *testing.T, ctx context.Context, url, path string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download %s: %s", url, response.Status)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(file, io.LimitReader(response.Body, 32<<20)); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func verifyFileDigest(t *testing.T, path, want string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		t.Fatalf("archive digest = %s, want %s", got, want)
	}
}

func extractEngram(t *testing.T, archive, root string) string {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	want := []string{"LICENSE", "README.md", "engram"}
	seen := []string{}
	engram := filepath.Join(root, "engram-release")
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != header.Name {
			t.Fatalf("unsafe release archive entry: %q type=%d", header.Name, header.Typeflag)
		}
		seen = append(seen, header.Name)
		if header.Name != "engram" {
			continue
		}
		out, err := os.OpenFile(engram, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, io.LimitReader(tarReader, 32<<20)); err != nil {
			out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(seen)
	sort.Strings(want)
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("release archive entries = %q, want %q", seen, want)
	}
	return engram
}

func writeEngramEnv(t *testing.T, path string, user int) {
	t.Helper()
	body := fmt.Sprintf("TELEGRAM_BOT_TOKEN=offline-fixture-token\nTELEGRAM_ALLOWED_USER_ID=%d\nLLM_PROVIDER=anthropic\nANTHROPIC_API_KEY=%s\nANTHROPIC_MODEL=claude-haiku-4-5-20251001\nENGRAM_HOME=/workspace/.engram\nENGRAM_WORKDIR=/workspace\nENGRAM_TMUX_SESSION=main\nENGRAM_ANCHOR_MODE=guide\n", user, secretCanary)
	mustWrite(t, path, []byte(body), 0o600)
}

func writeDeclaration(t *testing.T, path, world, image, user, engram, envSource, revisionSource string) {
	t.Helper()
	body := fmt.Sprintf(`version = 1
name = %q
[world]
hostname = "engram-e2e"
base = %q
workdir = "/workspace"
user = %q
[resources]
cpus = 1
memory_bytes = 536870912
pids = 256
[workspace]
paths = ["/workspace"]
[[copies]]
source = %q
target = "/usr/local/bin/engram"
mode = "0755"
[[copies]]
source = %q
target = "/etc/engram.env"
mode = "0600"
secret = true
[[copies]]
source = %q
target = "/etc/engram-revision"
mode = "0644"
[[services]]
name = "tmux"
command = ["/bin/sh", "-c", "/usr/bin/tmux new-session -d -s main && exec /usr/bin/tmux wait-for kenogram-service-stop"]
autostart = true
restart = "never"
`, world, image, user, engram, envSource, revisionSource)
	mustWrite(t, path, []byte(body), 0o600)
}

func assertEngramOffline(t *testing.T, ctx context.Context, dir string, env []string, container string, lock releaseLock) {
	t.Helper()
	version := run(t, ctx, dir, env, "podman", "exec", container, "/usr/local/bin/engram", "version")
	if !strings.Contains(version, "engram "+lock.Version+" commit="+lock.Commit) {
		t.Fatalf("in-world Engram version: %s", version)
	}
	for _, command := range []string{"preflight", "dry-start"} {
		out := run(t, ctx, dir, env, "podman", "exec", container, "/usr/local/bin/engram", command, "--env", "/etc/engram.env")
		for _, want := range []string{"telegram_api: not_called", "anthropic_api: not_called", "polling: not_started", "status: ok"} {
			if !strings.Contains(out, want) {
				t.Fatalf("engram %s missing %q:\n%s", command, want, out)
			}
		}
	}
	if got := strings.TrimSpace(run(t, ctx, dir, env, "podman", "exec", container, "stat", "-c", "%a", "/workspace/.engram/state.json")); got != "600" {
		t.Fatalf("Engram state mode = %q", got)
	}
	if out, err := runResult(ctx, dir, env, "podman", "exec", container, "test", "!", "-e", "/run/podman/podman.sock"); err != nil {
		t.Fatalf("runtime socket is visible: %s", out)
	}
}

func assertGeneratedContract(t *testing.T, ctx context.Context, dir string, env []string, container string) {
	t.Helper()
	for _, path := range []string{"/KENOGRAM.md", "/etc/kenogram/world.json", "/etc/kenogram/services/tmux.sh"} {
		if out, err := runResult(ctx, dir, env, "podman", "exec", container, "test", "-f", path); err != nil {
			t.Fatalf("generated contract path %s missing: %s", path, out)
		}
	}
	world := run(t, ctx, dir, env, "podman", "exec", container, "cat", "/etc/kenogram/world.json")
	if !strings.Contains(world, `"name": "engram-e2e-`) || !strings.Contains(world, `"generation":`) {
		t.Fatalf("world projection is incomplete:\n%s", world)
	}
	status := strings.TrimSpace(run(t, ctx, dir, env, "podman", "exec", container, "cat", "/run/kenogram/services/tmux"))
	if !strings.HasPrefix(status, "running ") {
		t.Fatalf("service evidence = %q", status)
	}
}

func assertSecretAbsent(t *testing.T, root, canary string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), canary) {
			return fmt.Errorf("secret canary leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func inWorldSHA256(t *testing.T, ctx context.Context, dir string, env []string, container, path string) string {
	t.Helper()
	out := strings.Fields(run(t, ctx, dir, env, "podman", "exec", container, "sha256sum", path))
	if len(out) != 2 || len(out[0]) != sha256.Size*2 {
		t.Fatalf("invalid in-world SHA-256 output for %s: %q", path, out)
	}
	return out[0]
}

func assertGeneration(t *testing.T, path string, generation int64, status string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Generation int64  `json:"generation"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Generation != generation || state.Status != status {
		t.Fatalf("state = %#v, want generation=%d status=%s", state, generation, status)
	}
}

func assertDestroyedHistory(t *testing.T, stateRoot, world string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(stateRoot, ".destroyed", world+"-*", "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("destroyed history files = %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{`"outcome":"applied"`, `"outcome":"adopted"`, `"outcome":"stopped"`, `"outcome":"restarted"`, `"outcome":"destroyed"`} {
		if !strings.Contains(string(raw), outcome) {
			t.Fatalf("destroyed history lacks %s:\n%s", outcome, raw)
		}
	}
}

func requireExecutable(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required E2E executable %s is unavailable: %v", name, err)
	}
}

func repositoryRoot(t *testing.T) string {
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
			t.Fatal("repository root not found")
		}
		wd = parent
	}
}

func containerName(world string, generation int) string {
	return fmt.Sprintf("kenogram-%s-g%d", world, generation)
}

func waitFor(t *testing.T, timeout time.Duration, probe func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		if ok, evidence := probe(); ok {
			return
		} else {
			last = evidence
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s; last evidence:\n%s", timeout, last)
}

func waitForTmux(t *testing.T, ctx context.Context, dir string, env []string, container string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		out, err := runResult(ctx, dir, env, "podman", "exec", container, "tmux", "has-session", "-t", "main")
		if err == nil {
			return
		}
		last = out
		time.Sleep(50 * time.Millisecond)
	}
	commands := [][]string{
		{"id"},
		{"tmux", "-V"},
		{"cat", "/etc/kenogram/services/tmux.sh"},
		{"ls", "-ld", "/tmp", "/workspace", "/etc/kenogram/services/tmux.sh"},
		{"find", "/tmp", "-maxdepth", "3", "-type", "s", "-o", "-type", "d"},
		{"ps", "-ef"},
		{"timeout", "3", "/bin/sh", "-x", "/etc/kenogram/services/tmux.sh"},
	}
	var diagnostics strings.Builder
	for _, command := range commands {
		out, err := runResult(ctx, dir, env, "podman", append([]string{"exec", container}, command...)...)
		fmt.Fprintf(&diagnostics, "$ %s\nerror: %v\n%s\n", strings.Join(command, " "), err, out)
	}
	t.Fatalf("tmux service unavailable; last probe:\n%s\ndiagnostics:\n%s", last, diagnostics.String())
}

func run(t *testing.T, ctx context.Context, dir string, env []string, name string, args ...string) string {
	t.Helper()
	out, err := runResult(ctx, dir, env, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}

func runResult(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	if env != nil {
		command.Env = env
	}
	out, err := command.CombinedOutput()
	return string(out), err
}

func mustWrite(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func copyRegularFile(t *testing.T, source, target string, mode os.FileMode) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("fixture is not a regular file: %s", source)
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, io.LimitReader(input, 32<<20)); err != nil {
		output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

//go:build linux

package integration

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/proxy"
)

// TestGitHTTPSPrivateCAWorkflow is a capability-shaped fixture: a real Git
// smart-HTTP server, a private CA, the exact-destination door, and an offline Go
// workload. Nothing in the fixture requires a public network or credentials.
func TestGitHTTPSPrivateCAWorkflow(t *testing.T) {
	if os.Getenv("KENOGRAM_INTEGRATION") != "1" {
		t.Skip("set KENOGRAM_INTEGRATION=1 to run integration proofs")
	}
	git := requirePath(t, "git")
	root := t.TempDir()
	execPathCommand := exec.Command(git, "--exec-path")
	execPathRaw, err := execPathCommand.Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	gitBackend := filepath.Join(strings.TrimSpace(string(execPathRaw)), "git-http-backend")
	if info, err := os.Stat(gitBackend); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("required git-http-backend is unavailable at %s: %v", gitBackend, err)
	}
	projects := filepath.Join(root, "projects")
	mustMkdir(t, projects)
	seed := filepath.Join(root, "seed")
	mustMkdir(t, seed)
	gitRun(t, seed, nil, git, "init", "--initial-branch=main")
	mustWriteFile(t, filepath.Join(seed, "go.mod"), "module fixture.example/proof\n\ngo 1.22\n")
	mustWriteFile(t, filepath.Join(seed, "proof.go"), "package proof\n\nfunc Value() string { return \"provable\" }\n")
	mustWriteFile(t, filepath.Join(seed, "proof_test.go"), "package proof\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != \"provable\" { t.Fatal(Value()) } }\n")
	identity := []string{"GIT_AUTHOR_NAME=Kenogram Proof", "GIT_AUTHOR_EMAIL=proof@kenogram.invalid", "GIT_COMMITTER_NAME=Kenogram Proof", "GIT_COMMITTER_EMAIL=proof@kenogram.invalid"}
	gitRun(t, seed, identity, git, "add", ".")
	gitRun(t, seed, identity, git, "commit", "-m", "seed proof")
	bare := filepath.Join(projects, "proof.git")
	gitRun(t, root, nil, git, "init", "--bare", bare)
	gitRun(t, bare, nil, git, "config", "http.receivepack", "true")
	gitRun(t, seed, nil, git, "remote", "add", "origin", bare)
	gitRun(t, seed, nil, git, "push", "origin", "main")
	gitRun(t, bare, nil, git, "symbolic-ref", "HEAD", "refs/heads/main")

	handler := &cgi.Handler{Path: gitBackend, Root: "/", Env: []string{"GIT_PROJECT_ROOT=" + projects, "GIT_HTTP_EXPORT_ALL=1"}}
	tlsListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: tlsListener, Config: &http.Server{Handler: handler}}
	server.StartTLS()
	defer server.Close()
	certificate := server.Certificate()
	caPath := filepath.Join(root, "private-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	serverHost, serverPortText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	serverPort, err := strconv.Atoi(serverPortText)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	door := proxy.New([]proxy.Destination{{Host: serverHost, Port: serverPort}}, proxy.Options{})
	go func() { _ = door.Serve(listener) }()
	defer listener.Close()

	proxyURL := "http://" + listener.Addr().String()
	env := append(identity,
		"HTTPS_PROXY="+proxyURL,
		"HTTP_PROXY="+proxyURL,
		"NO_PROXY=",
		"no_proxy=",
		"GIT_SSL_CAINFO="+caPath,
		"GIT_TERMINAL_PROMPT=0",
		"GOTOOLCHAIN=local",
		"GOPROXY=off",
	)
	clone := filepath.Join(root, "clone")
	gitRun(t, root, env, git, "clone", server.URL+"/proof.git", clone)
	commandRun(t, clone, env, "go", "test", "./...")
	mustWriteFile(t, filepath.Join(clone, "pushed.txt"), "through the door\n")
	gitRun(t, clone, env, git, "add", "pushed.txt")
	gitRun(t, clone, env, git, "commit", "-m", "prove push")
	gitRun(t, clone, env, git, "push", "origin", "main")
	verify := filepath.Join(root, "verify")
	gitRun(t, root, env, git, "clone", server.URL+"/proof.git", verify)
	if raw, err := os.ReadFile(filepath.Join(verify, "pushed.txt")); err != nil || string(raw) != "through the door\n" {
		t.Fatalf("pushed content = %q, err=%v", raw, err)
	}
}

func requirePath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("required integration executable %s is unavailable: %v", name, err)
	}
	return path
}

func gitRun(t *testing.T, dir string, extraEnv []string, git string, args ...string) {
	t.Helper()
	commandRun(t, dir, extraEnv, git, args...)
}

func commandRun(t *testing.T, dir string, extraEnv []string, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), extraEnv...)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

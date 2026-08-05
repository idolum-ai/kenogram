//go:build linux

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

func TestGovernedJobDirectPodmanEvidence(t *testing.T) {
	if os.Getenv("KENOGRAM_INTEGRATION") != "1" {
		t.Skip("set KENOGRAM_INTEGRATION=1")
	}
	require(t, "podman")
	root := repoRoot(t)
	tmp := t.TempDir()
	fixture := filepath.Join(tmp, "job-fixture")
	buildEnv := append(os.Environ(), "CGO_ENABLED=0")
	run(t, root, buildEnv, "go", "build", "-buildvcs=false", "-o", fixture, "./internal/integration/testdata/jobfixture")
	containerfile := filepath.Join(tmp, "Containerfile")
	if err := os.WriteFile(containerfile, []byte("FROM scratch\nCOPY job-fixture /usr/local/bin/job-target\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	imageTag := "localhost/kenogram-job-integration:" + fmt.Sprint(time.Now().UnixNano())
	run(t, tmp, nil, "podman", "build", "-t", imageTag, "-f", containerfile, ".")
	imageID, err := canonicalPodmanImageID(strings.TrimSpace(run(t, tmp, nil, "podman", "image", "inspect", "--format", "{{.Id}}", imageTag)))
	if err != nil {
		t.Fatalf("image identity: %v", err)
	}
	bin := filepath.Join(tmp, "kenogram")
	run(t, root, buildEnv, "go", "build", "-buildvcs=false", "-o", bin, "./cmd/kenogram")
	t.Cleanup(func() { exec.Command("podman", "rmi", "--force", imageTag).Run() })

	t.Run("success and nonzero with governed policy", func(t *testing.T) {
		jobID := "direct-provider-proof"
		cleanupJobContainers(t, jobID)
		mountSource := filepath.Join(t.TempDir(), "input")
		if err := os.Mkdir(mountSource, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mountSource, "read-only.txt"), []byte("mounted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		secret := []byte("integrated-secret\nline")
		secretSource := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(secretSource, secret, 0o600); err != nil {
			t.Fatal(err)
		}
		declarationPath, declarationRaw := writeJobDeclaration(t, tmp, imageID, fmt.Sprintf(`
[[mounts]]
source = %q
target = "/input"
mode = "ro"

[[copies]]
source = %q
target = "/usr/local/bin/secret-token"
mode = "0600"
secret = true
`, mountSource, secretSource))
		public := "explicit"
		request := governedRequest(jobID, declarationPath, declarationRaw, []string{"/usr/local/bin/job-target", "--proof"})
		request.Command.Environment = []jobcontract.EnvironmentItem{
			{Name: "KENOGRAM_RETAINED", PublicValue: &public},
			{Name: "KENOGRAM_SECRET", SecretFile: "/usr/local/bin/secret-token"},
		}
		request.Artifacts = &jobcontract.ArtifactRequest{ContainerRoot: "/artifacts", MaxEntries: 2, MaxBytes: 4096}
		result, evidenceDir := runGovernedJob(t, tmp, bin, request, false)
		if result.Status != "complete" || result.Target.Kind != "exited" || result.Target.ExitStatus == nil || *result.Target.ExitStatus != 7 {
			t.Fatalf("result=%#v", result)
		}
		assertVerifiedJob(t, tmp, bin, evidenceDir, "complete")
		for path, wanted := range map[string]string{
			"stdout.bin":                   "governed stdout\n",
			"stderr.bin":                   "governed stderr\n",
			"target-artifacts/report.json": "{\"proof\":true}\n",
		} {
			raw, err := os.ReadFile(filepath.Join(evidenceDir, path))
			if err != nil || string(raw) != wanted {
				t.Fatalf("%s=%q err=%v", path, raw, err)
			}
		}
		assertSecretAbsentFromEvidence(t, evidenceDir, secret)
		assertNoOwnedContainers(t, tmp, jobID)
	})

	t.Run("zero exit remains complete", func(t *testing.T) {
		jobID := "direct-provider-success"
		cleanupJobContainers(t, jobID)
		declarationPath, declarationRaw := writeJobDeclaration(t, tmp, imageID, "")
		request := governedRequest(jobID, declarationPath, declarationRaw, []string{"/usr/local/bin/job-target", "--success"})
		result, evidenceDir := runGovernedJob(t, tmp, bin, request, false)
		if result.Status != "complete" || result.Target.ExitStatus == nil || *result.Target.ExitStatus != 0 {
			t.Fatalf("result=%#v", result)
		}
		assertVerifiedJob(t, tmp, bin, evidenceDir, "complete")
		assertNoOwnedContainers(t, tmp, jobID)
	})

	t.Run("timeout kills orphan and seals unknown", func(t *testing.T) {
		jobID := "direct-provider-timeout"
		cleanupJobContainers(t, jobID)
		declarationPath, declarationRaw := writeJobDeclaration(t, tmp, imageID, "")
		request := governedRequest(jobID, declarationPath, declarationRaw, []string{"/usr/local/bin/job-target", "--hang-orphan"})
		request.Limits.TimeoutNS = int64(750 * time.Millisecond)
		result, evidenceDir := runGovernedJob(t, tmp, bin, request, true)
		if result.Status != "incomplete" || result.Target.Kind != "unknown" || result.Cleanup.Status != "complete" || !result.Cleanup.ProcessGroupEmpty {
			t.Fatalf("result=%#v", result)
		}
		assertVerifiedJob(t, tmp, bin, evidenceDir, "incomplete")
		assertNoOwnedContainers(t, tmp, jobID)
	})
}

func writeJobDeclaration(t *testing.T, dir, imageID, extra string) (string, []byte) {
	t.Helper()
	path := filepath.Join(dir, "kenogram-"+fmt.Sprint(time.Now().UnixNano())+".toml")
	raw := []byte(fmt.Sprintf(`version = 1
name = "job-integration"
[world]
hostname = "job-integration"
base = %q
workdir = "/workspace"
user = "0"
[resources]
cpus = 1
memory_bytes = 268435456
pids = 64
[workspace]
paths = ["/workspace"]
%s`, imageID, extra))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func governedRequest(jobID, declarationPath string, declarationRaw []byte, argv []string) jobcontract.Request {
	return jobcontract.Request{
		Schema: jobcontract.RequestSchema, JobID: jobID,
		Declaration: jobcontract.DeclarationBinding{Path: declarationPath, SHA256: sha256Digest(declarationRaw)},
		Command:     jobcontract.Command{Argv: argv, WorkingDirectory: "/workspace", Environment: []jobcontract.EnvironmentItem{}},
		Limits:      jobcontract.Limits{TimeoutNS: int64(20 * time.Second), FinalizeNS: int64(10 * time.Second), StdoutMaxBytes: 4096, StderrMaxBytes: 4096},
	}
}

func runGovernedJob(t *testing.T, dir, bin string, request jobcontract.Request, expectFailure bool) (jobcontract.Result, string) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, request.JobID+"-request.json")
	if err := os.WriteFile(requestPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(dir, request.JobID+"-evidence")
	command := exec.Command(bin, "job", "--request", requestPath, "--evidence-dir", evidenceDir)
	stdout, stderr := new(strings.Builder), new(strings.Builder)
	command.Stdout, command.Stderr = stdout, stderr
	err = command.Run()
	if expectFailure {
		if err == nil {
			t.Fatal("incomplete governed job returned success")
		}
	} else if err != nil {
		t.Fatalf("governed job: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	var result jobcontract.Result
	if parseErr := json.Unmarshal([]byte(stdout.String()), &result); parseErr != nil {
		t.Fatalf("parse result: %v\nstdout=%s\nstderr=%s", parseErr, stdout, stderr)
	}
	return result, evidenceDir
}

func assertVerifiedJob(t *testing.T, dir, bin, evidenceDir, status string) {
	t.Helper()
	verification := run(t, dir, nil, bin, "verify-job", "--evidence-dir", evidenceDir)
	var observed struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(verification), &observed); err != nil || observed.Status != status {
		t.Fatalf("verification=%s", verification)
	}
}

func assertSecretAbsentFromEvidence(t *testing.T, evidenceDir string, secret []byte) {
	t.Helper()
	if err := filepath.Walk(evidenceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(raw), string(secret)) {
			return fmt.Errorf("secret retained in %s", filepath.Base(path))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func cleanupJobContainers(t *testing.T, jobID string) {
	t.Helper()
	t.Cleanup(func() {
		containers, _ := exec.Command("podman", "ps", "--all", "--filter", "label=io.kenogram.job-id="+jobID, "--format", "{{.Names}}").Output()
		for _, name := range strings.Fields(string(containers)) {
			exec.Command("podman", "rm", "--force", name).Run()
		}
	})
}

func assertNoOwnedContainers(t *testing.T, dir, jobID string) {
	t.Helper()
	remaining := strings.TrimSpace(run(t, dir, nil, "podman", "ps", "--all", "--filter", "label=io.kenogram.job-id="+jobID, "--format", "{{.Names}}"))
	if remaining != "" {
		t.Fatalf("owned container survived cleanup: %q", remaining)
	}
}

func sha256Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

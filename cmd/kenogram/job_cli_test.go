package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/job"
	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

type unusedJobRuntime struct{}

func (unusedJobRuntime) Start(context.Context, job.Invocation, io.Writer, io.Writer) (job.Process, error) {
	return nil, fmt.Errorf("unexpected provider start")
}
func (unusedJobRuntime) Cleanup(context.Context, job.Invocation) jobcontract.CleanupResult {
	return jobcontract.CleanupResult{Status: "complete", ContainerAbsent: true, ProxyAbsent: true, ProcessGroupEmpty: true, Reasons: []string{}}
}

func TestVersionJSONReportsValidatedExecutableProvenance(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var provenance jobcontract.Provenance
	if err := json.Unmarshal(stdout.Bytes(), &provenance); err != nil {
		t.Fatal(err)
	}
	if err := jobcontract.ValidateProvenance(provenance); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(provenance.ExecutableSHA256, "sha256:") {
		t.Fatalf("provenance=%#v", provenance)
	}
}

func TestGovernedJobCLIProviderBoundaryFailsClosed(t *testing.T) {
	prior := governedJobRuntime
	governedJobRuntime = nil
	defer func() { governedJobRuntime = prior }()
	dir := t.TempDir()
	declaration := []byte("version = 1\nname = \"job\"\n[world]\nhostname = \"job\"\nbase = \"example.invalid/job@sha256:" + strings.Repeat("a", 64) + "\"\nworkdir = \"/workspace\"\nuser = \"agent\"\n[resources]\ncpus = 1\nmemory_bytes = 1024\npids = 8\n[workspace]\npaths = [\"/workspace\"]\n")
	declarationPath := filepath.Join(dir, "kenogram.toml")
	if err := os.WriteFile(declarationPath, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(declaration)
	request := jobcontract.Request{
		Schema: jobcontract.RequestSchema, JobID: "job-1",
		Declaration: jobcontract.DeclarationBinding{Path: declarationPath, SHA256: fmt.Sprintf("sha256:%x", sum)},
		Command:     jobcontract.Command{Argv: []string{"/bin/true"}, WorkingDirectory: "/workspace", Environment: []jobcontract.EnvironmentItem{}},
		Limits:      jobcontract.Limits{TimeoutNS: int64(time.Second), FinalizeNS: int64(time.Second), StdoutMaxBytes: 1, StderrMaxBytes: 1},
	}
	raw, _ := json.Marshal(request)
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runJob(context.Background(), []string{"--request", requestPath, "--evidence-dir", filepath.Join(dir, "evidence")}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "provider is not configured") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestGovernedJobCLIRejectsInvalidRequestBeforeProviderSelection(t *testing.T) {
	prior := governedJobRuntime
	governedJobRuntime = nil
	defer func() { governedJobRuntime = prior }()
	request := t.TempDir() + "/request.json"
	if err := os.WriteFile(request, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runJob(context.Background(), []string{"--request", request, "--evidence-dir", t.TempDir() + "/evidence"}, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "job request") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestGovernedJobCLIReturnsOneForPostIdentityPublicationFailure(t *testing.T) {
	prior := governedJobRuntime
	governedJobRuntime = func() job.Runtime { return unusedJobRuntime{} }
	defer func() { governedJobRuntime = prior }()
	dir := t.TempDir()
	declaration := []byte("version = 1\nname = \"job\"\n[world]\nhostname = \"job\"\nbase = \"example.invalid/job@sha256:" + strings.Repeat("a", 64) + "\"\nworkdir = \"/workspace\"\nuser = \"agent\"\n[resources]\ncpus = 1\nmemory_bytes = 1024\npids = 8\n[workspace]\npaths = [\"/workspace\"]\n")
	declarationPath := filepath.Join(dir, "kenogram.toml")
	if err := os.WriteFile(declarationPath, declaration, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(declaration)
	request := jobcontract.Request{
		Schema: jobcontract.RequestSchema, JobID: "job-1",
		Declaration: jobcontract.DeclarationBinding{Path: declarationPath, SHA256: fmt.Sprintf("sha256:%x", sum)},
		Command:     jobcontract.Command{Argv: []string{"/bin/true"}, WorkingDirectory: "/workspace", Environment: []jobcontract.EnvironmentItem{}},
		Limits:      jobcontract.Limits{TimeoutNS: int64(time.Second), FinalizeNS: int64(time.Second), StdoutMaxBytes: 1, StderrMaxBytes: 1},
	}
	raw, _ := json.Marshal(request)
	requestPath := filepath.Join(dir, "request.json")
	if err := os.WriteFile(requestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "evidence")
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runJob(context.Background(), []string{"--request", requestPath, "--evidence-dir", evidence}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "without replacement") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/app"
	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/doctor"
	"github.com/idolum-ai/kenogram/internal/history"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

func TestStatusPayloadPreservesAliasesAndReportsTransitionSources(t *testing.T) {
	observation := &app.GenerationObservation{
		State:    worldfs.State{Name: "w", Generation: 2, Container: "kenogram-w-g2"},
		Exists:   true,
		Evidence: &backend.Evidence{Name: "kenogram-w-g2", Running: true},
	}
	payload := newStatusPayload(app.StatusResult{Authoritative: observation, RecoveryPhase: "commit"})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"state":{"name":"w"`, `"runtime_evidence"`, `"runtime_exists":true`, `"authoritative"`, `"recovery_phase":"commit"`, `"declared":"transition.json"`, `"recorded":"transition.json"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("payload missing %s: %s", want, raw)
		}
	}
}

func TestSettledStatusSourcesNameAppliedArtifacts(t *testing.T) {
	payload := newStatusPayload(app.StatusResult{})
	if payload.Sources["declared"] != "applied.toml" || payload.Sources["recorded"] != "state.json" {
		t.Fatalf("sources = %v", payload.Sources)
	}
}

func TestStatusCommandJSONAndTextSurface(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteState(worldfs.State{Name: "w", Generation: 1, Status: "stopped", PlanDigest: "plan", DeclarationDigest: "declaration"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.History, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := newApp
	newApp = func(io.Writer) (*app.App, error) { return &app.App{BaseDir: base}, nil }
	t.Cleanup(func() { newApp = previous })

	var stdout, stderr bytes.Buffer
	if code := runStatus(context.Background(), []string{"--json", "w"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{`"state"`, `"runtime_exists": false`, `"status"`, `"authoritative"`, `"declared": "applied.toml"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("JSON missing %q: %s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runStatus(context.Background(), []string{"w"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"world: w", "status: stopped", "authoritative generation: g1", "authoritative runtime exists: false"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("text missing %q: %s", want, stdout.String())
		}
	}
}

func TestDryRunExample(t *testing.T) {
	root := repoRoot(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", "--dry-run", filepath.Join(root, "kenogram.example.toml")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Kenogram plan") || !strings.Contains(stdout.String(), "plan digest:") {
		t.Fatalf("stdout=%s", stdout.String())
	}
}

func TestUpWithoutConfirmationIsHonest(t *testing.T) {
	root := repoRoot(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", filepath.Join(root, "kenogram.example.toml")}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "without --yes") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestJSONDryRun(t *testing.T) {
	root := repoRoot(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", "--dry-run", "--json", filepath.Join(root, "kenogram.example.toml")}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"plan_digest"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestJSONDryRunOfExistingWorldIsExactlyOneObject(t *testing.T) {
	for _, test := range []struct {
		name        string
		changePlan  bool
		wantChanges bool
	}{
		{name: "semantic and workspace changes", changePlan: true, wantChanges: true},
		{name: "workspace-only drift"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			declaration := filepath.Join(t.TempDir(), "world.toml")
			priorRaw := comparisonDeclaration("reviewed")
			if err := os.WriteFile(declaration, []byte(priorRaw), 0o600); err != nil {
				t.Fatal(err)
			}
			prior, err := app.Prepare(declaration)
			if err != nil {
				t.Fatal(err)
			}
			writeComparisonWorld(t, base, prior, declaration)
			if err := os.WriteFile(filepath.Join(worldfs.For(base, "reviewed").Workspace, "drift.txt"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.changePlan {
				if err := os.WriteFile(declaration, []byte(strings.Replace(priorRaw, "cpus = 1", "cpus = 2", 1)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			previous := newApp
			newApp = func(io.Writer) (*app.App, error) { return &app.App{BaseDir: base}, nil }
			t.Cleanup(func() { newApp = previous })
			var stdout, stderr bytes.Buffer
			if code := run([]string{"up", "--dry-run", "--json", declaration}, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var payload struct {
				Changes   []map[string]any `json:"changes"`
				Workspace string           `json:"workspace"`
			}
			decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
			if err := decoder.Decode(&payload); err != nil {
				t.Fatalf("decode stdout: %v: %q", err, stdout.String())
			}
			if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
				t.Fatalf("stdout contains more than one JSON value: %v: %q", err, stdout.String())
			}
			if (len(payload.Changes) > 0) != test.wantChanges || !strings.Contains(payload.Workspace, "1 files changed") || stderr.Len() != 0 {
				t.Fatalf("payload=%#v stderr=%q", payload, stderr.String())
			}
		})
	}
}

func TestUpComparisonFailuresPrecedeOutputAndConfirmation(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*testing.T, worldfs.Layout, app.Prepared)
		want string
	}{
		{
			name: "corrupt state",
			set: func(t *testing.T, layout worldfs.Layout, _ app.Prepared) {
				if err := os.WriteFile(layout.State, []byte("not-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "read predecessor state: decode state",
		},
		{
			name: "applied declaration without state",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if err := layout.WriteApplied(prepared.Raw); err != nil {
					t.Fatal(err)
				}
			},
			want: "state is missing while applied declaration exists",
		},
		{
			name: "applied plan without state",
			set: func(t *testing.T, layout worldfs.Layout, _ app.Prepared) {
				if err := layout.WriteAppliedPlan([]byte("{}")); err != nil {
					t.Fatal(err)
				}
			},
			want: "state is missing while applied plan exists",
		},
		{
			name: "recorded workspace digest without state",
			set: func(t *testing.T, layout worldfs.Layout, _ app.Prepared) {
				if _, err := layout.WriteDigest(1, worldfs.DigestTree{Root: "orphan"}); err != nil {
					t.Fatal(err)
				}
			},
			want: "state is missing while recorded workspace digests exist",
		},
		{
			name: "carried workspace without state",
			set: func(t *testing.T, layout worldfs.Layout, _ app.Prepared) {
				if err := os.WriteFile(filepath.Join(layout.Workspace, "orphan"), []byte("data"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "state is missing while carried workspace entries exist",
		},
		{
			name: "proxy identity without state",
			set: func(t *testing.T, layout worldfs.Layout, _ app.Prepared) {
				if err := os.WriteFile(layout.ProxyPID, []byte("123 1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "state is missing while proxy identity exists",
		},
		{
			name: "authoritative history without state",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if _, err := history.Append(layout.History, history.Record{Action: "up", PlanDigest: prepared.Result.PlanDigest, Outcome: "applied"}, time.Unix(1, 0)); err != nil {
					t.Fatal(err)
				}
			},
			want: "state is missing while authoritative history exists",
		},
		{
			name: "missing applied declaration",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if err := layout.WriteState(comparisonState(prepared, "missing.toml")); err != nil {
					t.Fatal(err)
				}
			},
			want: "prepare predecessor:",
		},
		{
			name: "invalid applied declaration",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if err := layout.WriteState(comparisonState(prepared, layout.Applied)); err != nil {
					t.Fatal(err)
				}
				if err := layout.WriteApplied([]byte("not-toml")); err != nil {
					t.Fatal(err)
				}
			},
			want: "prepare predecessor:",
		},
		{
			name: "applied declaration digest mismatch",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				state := comparisonState(prepared, layout.Applied)
				state.DeclarationDigest = strings.Repeat("0", 64)
				if err := layout.WriteState(state); err != nil {
					t.Fatal(err)
				}
				if err := layout.WriteApplied(prepared.Raw); err != nil {
					t.Fatal(err)
				}
			},
			want: "applied predecessor declaration",
		},
		{
			name: "invalid applied recovery plan",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if err := layout.WriteState(comparisonState(prepared, layout.Applied)); err != nil {
					t.Fatal(err)
				}
				if err := layout.WriteApplied(prepared.Raw); err != nil {
					t.Fatal(err)
				}
				if err := layout.WriteAppliedPlan([]byte("not-json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "decode applied plan",
		},
		{
			name: "missing predecessor workspace digest",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if err := layout.WriteState(comparisonState(prepared, layout.Applied)); err != nil {
					t.Fatal(err)
				}
				if err := layout.WriteApplied(prepared.Raw); err != nil {
					t.Fatal(err)
				}
			},
			want: "read predecessor workspace digest:",
		},
		{
			name: "missing current workspace",
			set: func(t *testing.T, layout worldfs.Layout, prepared app.Prepared) {
				if err := layout.WriteState(comparisonState(prepared, layout.Applied)); err != nil {
					t.Fatal(err)
				}
				if err := layout.WriteApplied(prepared.Raw); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(layout.Workspace); err != nil {
					t.Fatal(err)
				}
			},
			want: "digest current workspace:",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			declaration := filepath.Join(t.TempDir(), "world.toml")
			if err := os.WriteFile(declaration, []byte(comparisonDeclaration("reviewed")), 0o600); err != nil {
				t.Fatal(err)
			}
			prepared, err := app.Prepare(declaration)
			if err != nil {
				t.Fatal(err)
			}
			layout := worldfs.For(base, "reviewed")
			if err := layout.Ensure(); err != nil {
				t.Fatal(err)
			}
			test.set(t, layout, prepared)
			previous := newApp
			newApp = func(io.Writer) (*app.App, error) { return &app.App{BaseDir: base}, nil }
			t.Cleanup(func() { newApp = previous })
			var stdout, stderr bytes.Buffer
			code := run([]string{"up", "--dry-run", "--json", declaration}, &stdout, &stderr)
			if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want %q", code, stdout.String(), stderr.String(), test.want)
			}
		})
	}
}

func comparisonDeclaration(name string) string {
	return `version = 1
name = "` + name + `"
[world]
hostname = "` + name + `"
base = "example@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "agent"
[resources]
cpus = 1
memory_bytes = 268435456
pids = 32
[workspace]
paths = ["/workspace"]
`
}

func comparisonState(prepared app.Prepared, declarationPath string) worldfs.State {
	return worldfs.State{Name: prepared.Result.Plan.Name, Generation: 1, Container: "kenogram-" + prepared.Result.Plan.Name + "-g1", PlanDigest: prepared.Result.PlanDigest, DeclarationDigest: prepared.Result.DeclarationDigest, DeclarationPath: declarationPath, Status: "running"}
}

func writeComparisonWorld(t *testing.T, base string, prepared app.Prepared, declarationPath string) {
	t.Helper()
	layout := worldfs.For(base, prepared.Result.Plan.Name)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteState(comparisonState(prepared, declarationPath)); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteApplied(prepared.Raw); err != nil {
		t.Fatal(err)
	}
	digest, err := worldfs.Digest(layout.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.WriteDigest(1, digest); err != nil {
		t.Fatal(err)
	}
}

func TestSubcommandHelpIsSuccessful(t *testing.T) {
	for _, command := range []string{"up", "down", "destroy", "enter", "connect", "status", "allow", "revoke", "repair-history", "worlds", "doctor"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run([]string{command, "--help"}, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if code != 0 || !strings.Contains(output, "usage: kenogram "+command) {
				t.Fatalf("code=%d output=%q", code, output)
			}
			if command == "down" && strings.Contains(output, "--yes") {
				t.Fatalf("down help advertises destroy-only confirmation: %q", output)
			}
		})
	}
}

func TestRelayConnectionIsAByteTransparentFullDuplexStream(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	wantUpload := []byte{0, 's', 's', 'h', '\r', '\n', 255}
	wantDownload := []byte{'o', 'k', 0, 254}
	serverDone := make(chan error, 1)
	go func() {
		got := make([]byte, len(wantUpload))
		if _, err := io.ReadFull(server, got); err != nil {
			serverDone <- err
			return
		}
		if !bytes.Equal(got, wantUpload) {
			serverDone <- errors.New("uploaded bytes changed")
			return
		}
		_, err := server.Write(wantDownload)
		serverDone <- err
		server.Close()
	}()
	var output bytes.Buffer
	if err := relayConnection(context.Background(), bytes.NewReader(wantUpload), &output, client); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), wantDownload) {
		t.Fatalf("downloaded bytes = %v, want %v", output.Bytes(), wantDownload)
	}
}

func TestRelayConnectionPreservesUploadAfterServerHalfClosesOutput(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("sandbox does not permit loopback sockets")
		}
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	wantUpload := []byte("staged-after-response")
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		tcp := connection.(*net.TCPConn)
		if _, writeErr := tcp.Write([]byte("ready")); writeErr != nil {
			serverDone <- writeErr
			return
		}
		if closeErr := tcp.CloseWrite(); closeErr != nil {
			serverDone <- closeErr
			return
		}
		got, readErr := io.ReadAll(tcp)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if !bytes.Equal(got, wantUpload) {
			serverDone <- errors.New("upload stopped after server half-close")
			return
		}
		serverDone <- nil
	}()

	inputReader, inputWriter := io.Pipe()
	var output bytes.Buffer
	outputReady := make(chan struct{})
	notifyingOutput := &notifyingWriter{writer: &output, ready: outputReady}
	relayDone := make(chan error, 1)
	go func() { relayDone <- relayConnection(context.Background(), inputReader, notifyingOutput, client) }()
	select {
	case <-outputReady:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for server output")
	}
	if output.String() != "ready" {
		t.Fatalf("server output = %q", output.String())
	}
	if _, err := inputWriter.Write(wantUpload); err != nil {
		t.Fatal(err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-relayDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type notifyingWriter struct {
	writer io.Writer
	ready  chan struct{}
	once   sync.Once
}

func (w *notifyingWriter) Write(payload []byte) (int, error) {
	n, err := w.writer.Write(payload)
	if n > 0 {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func TestDoctorReportsEveryObservationInTextAndJSON(t *testing.T) {
	previous := inspectHost
	wantState := t.TempDir()
	var observedState string
	inspectHost = func(_ context.Context, stateDir string) doctor.Report {
		observedState = stateDir
		return doctor.Report{Ready: false, Checks: []doctor.Check{
			{Name: "podman_rootless", Status: "pass", Observed: "rootless=true"},
			{Name: "nsenter_executable", Status: "fail", Observed: "not found\nretry", Remediation: "install util-linux"},
			{Name: "normal_entry_surface", Status: "info", Observed: "needs tmux"},
		}}
	}
	t.Cleanup(func() { inspectHost = previous })
	t.Setenv("KENOGRAM_STATE_DIR", wantState)

	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), nil, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if observedState != wantState {
		t.Fatalf("doctor inspected state %q, want %q", observedState, wantState)
	}
	for _, want := range []string{"PASS\tpodman_rootless", "FAIL\tnsenter_executable\tnot found" + `\nretry`, "remedy: install util-linux", "INFO\tnormal_entry_surface", "ready: false"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("text output missing %q: %s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runDoctor(context.Background(), []string{"--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var report doctor.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ready || len(report.Checks) != 3 || report.Checks[1].Remediation != "install util-linux" {
		t.Fatalf("report = %#v", report)
	}
}

func TestSubcommandUsageErrorsExplainTheFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing status world", args: []string{"status"}, want: "usage: kenogram status"},
		{name: "invalid status world", args: []string{"status", "INVALID!"}, want: "invalid world name"},
		{name: "missing enter world", args: []string{"enter"}, want: "usage: kenogram enter"},
		{name: "extra worlds argument", args: []string{"worlds", "extra"}, want: "usage: kenogram worlds"},
		{name: "down rejects destroy flag", args: []string{"down", "--yes", "world"}, want: "flag provided but not defined"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(test.args, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestAppleMachineBridgeRestoresExactArguments(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the decoded Apple machine operation runs inside Linux")
	}
	args := []string{"status", "INVALID! $HOME 'quoted' $(false) && echo", ""}
	encoded := backend.EncodeAppleMachineArguments(args)
	var stdout, stderr bytes.Buffer
	code := run(append([]string{backend.AppleMachineBridgeCommand}, encoded...), &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "usage: kenogram status") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAppleMachineBridgeRejectsMalformedEnvelope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the decoded Apple machine operation runs inside Linux")
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{backend.AppleMachineBridgeCommand, "$HOME"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "decode Apple machine arguments") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestLauncherExitStatusAndTransportFailureRemainDistinct(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string
	}{
		{name: "inner usage", err: &backend.RemoteExitError{Code: 2}, wantCode: 2},
		{name: "inner interrupt", err: &backend.RemoteExitError{Code: 130}, wantCode: 130},
		{name: "transport", err: errors.New("machine unavailable"), wantCode: 1, wantStderr: "runtime: machine unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := reportLauncherError(test.err, &stderr); code != test.wantCode || !strings.Contains(stderr.String(), test.wantStderr) {
				t.Fatalf("code=%d stderr=%q", code, stderr.String())
			}
		})
	}
}

func TestDryRunRejectsMountContainingStateRoot(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	t.Setenv("KENOGRAM_STATE_DIR", state)
	declaration := filepath.Join(root, "dangerous.toml")
	raw := `version = 1
name = "dry-run-mount"
[world]
hostname = "dry-run-mount"
base = "example@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "0"
[resources]
cpus = 1
memory_bytes = 268435456
pids = 32
[workspace]
paths = ["/workspace"]
[[mounts]]
source = "."
target = "/host"
mode = "ro"
`
	if err := os.WriteFile(declaration, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"up", "--dry-run", declaration}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "protected host path") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func repoRoot(t *testing.T) string {
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

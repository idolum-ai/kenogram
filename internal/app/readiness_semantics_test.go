package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

// This file is an executable design fixture, not a shipped readiness
// implementation. It inserts today's polling-wrapper technique at App's real
// services-started checkpoint, then leaves App to perform its existing runtime
// verification, rollback/commit transition, and recovery behavior. Because no
// readiness gate exists yet, a negative result cannot directly return through
// App.Up: the fixture fails the next real successor verification to prove the
// selected pre-commit placement and ordinary rollback cleanup, not result-to-
// error production wiring.
const (
	referenceReadinessTimeout = 150 * time.Millisecond
	referenceRetryCadence     = 5 * time.Millisecond
	referenceMaxAttempts      = 5
	referenceMaxOutputBytes   = 192
)

type readinessActionSpec struct {
	Command        []string
	Destination    string
	Timeout        time.Duration
	RetryCadence   time.Duration
	MaxAttempts    int
	MaxOutputBytes int
}

type readinessObservation struct {
	Command    []string `json:"command"`
	Status     string   `json:"status"`
	Attempts   int      `json:"attempts"`
	Diagnostic string   `json:"diagnostic,omitempty"`
	Truncated  bool     `json:"diagnostic_truncated"`
}

type readinessAttempt func(context.Context, *readinessDoor) (bool, string, error)

type attemptResult struct {
	ready  bool
	output string
	err    error
}

func runReadinessWrapper(parent context.Context, spec readinessActionSpec, door *readinessDoor, attempt readinessAttempt) readinessObservation {
	observation := readinessObservation{Command: append([]string(nil), spec.Command...), Status: "invalid"}
	if len(spec.Command) == 0 || spec.Timeout <= 0 || spec.RetryCadence <= 0 || spec.MaxAttempts <= 0 || spec.MaxOutputBytes <= 0 || attempt == nil {
		return observation
	}
	ctx, cancel := context.WithTimeout(parent, spec.Timeout)
	defer cancel()
	for number := 1; number <= spec.MaxAttempts; number++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			observation.Status = readinessContextStatus(ctxErr)
			return observation
		}
		result := runBoundedAttempt(ctx, attempt, door)
		observation.Attempts = number
		line := fmt.Sprintf("attempt %d: %s", number, strings.TrimSpace(result.output))
		if result.err != nil {
			line += ": " + result.err.Error()
		}
		appendBoundedDiagnostic(&observation, line+"\n", spec.MaxOutputBytes)
		// A result and the context deadline may become observable together.
		// Deadline/cancellation wins so a late success can never authorize
		// readiness nondeterministically.
		if ctxErr := ctx.Err(); ctxErr != nil {
			observation.Status = readinessContextStatus(ctxErr)
			return observation
		}
		if result.err == nil && result.ready {
			observation.Status = "ready"
			return observation
		}
		if number == spec.MaxAttempts {
			observation.Status = "attempts_exhausted"
			return observation
		}
		timer := time.NewTimer(spec.RetryCadence)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			observation.Status = readinessContextStatus(ctx.Err())
			return observation
		case <-timer.C:
		}
	}
	panic("unreachable")
}

func readinessContextStatus(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "canceled"
}

func runBoundedAttempt(ctx context.Context, attempt readinessAttempt, door *readinessDoor) attemptResult {
	result := make(chan attemptResult, 1)
	go func() {
		ready, output, err := attempt(ctx, door)
		result <- attemptResult{ready: ready, output: output, err: err}
	}()
	select {
	case observed := <-result:
		return observed
	case <-ctx.Done():
		return attemptResult{err: ctx.Err()}
	}
}

func appendBoundedDiagnostic(observation *readinessObservation, text string, limit int) {
	remaining := limit - len(observation.Diagnostic)
	if remaining <= 0 {
		observation.Truncated = true
		return
	}
	if len(text) > remaining {
		observation.Diagnostic += text[:remaining]
		observation.Truncated = true
		return
	}
	observation.Diagnostic += text
}

// readinessDoor is a declaration-derived policy analogue, not traffic through
// Kenogram's proxy. The action can ask it to invoke a destination, but cannot
// alter the allowance set derived from the candidate plan.
type readinessDoor struct {
	allowed   map[string]struct{}
	attempted []string
	delivered []string
}

func readinessDoorFromPlan(allows []plan.NetworkAllow) *readinessDoor {
	allowed := make(map[string]struct{}, len(allows))
	for _, entry := range allows {
		destination := net.JoinHostPort(entry.Host, strconv.FormatInt(entry.Port, 10))
		allowed[destination] = struct{}{}
	}
	return &readinessDoor{allowed: allowed}
}

func (d *readinessDoor) invoke(destination string, operation func()) error {
	d.attempted = append(d.attempted, destination)
	if _, ok := d.allowed[destination]; !ok {
		return fmt.Errorf("destination %s is not declared", destination)
	}
	d.delivered = append(d.delivered, destination)
	operation()
	return nil
}

type readinessProvider struct {
	readyAfter int
	calls      int
	output     string
}

func (p *readinessProvider) action(destination string) readinessAttempt {
	return func(_ context.Context, door *readinessDoor) (bool, string, error) {
		ready := false
		err := door.invoke(destination, func() {
			// Provider-facing checks are actions: even this sanitized fixture
			// increments provider state rather than pretending to be passive.
			p.calls++
			ready = p.readyAfter > 0 && p.calls >= p.readyAfter
		})
		return ready, p.output, err
	}
}

func referenceActionSpec(destination string) readinessActionSpec {
	return readinessActionSpec{
		Command:        []string{"channel-status", "--require", "connected"},
		Destination:    destination,
		Timeout:        referenceReadinessTimeout,
		RetryCadence:   referenceRetryCadence,
		MaxAttempts:    referenceMaxAttempts,
		MaxOutputBytes: referenceMaxOutputBytes,
	}
}

func TestReadinessWrapperSemanticReference(t *testing.T) {
	t.Run("delayed action precedes successor verification and commit", func(t *testing.T) {
		base, layout, prior, successor, runner, application := newReadinessApp(t)
		defer func() { _ = application.stopProxy(layout) }()
		applyReadinessPredecessor(t, application, prior)
		providerDestination := declaredReadinessDestination(t, successor.Result.Plan.NetworkAllow)
		door := readinessDoorFromPlan(successor.Result.Plan.NetworkAllow)
		provider := &readinessProvider{readyAfter: 3, output: "adapter state observed"}
		spec := referenceActionSpec(providerDestination)
		var observation readinessObservation
		var events []string
		application.lifecycleCheckpoint = func(name string) {
			events = append(events, name)
			if name == "services-started" {
				observation = runReadinessWrapper(context.Background(), spec, door, provider.action(spec.Destination))
				if observation.Status != "ready" {
					t.Fatal("delayed action did not become ready")
				}
				events = append(events, "readiness-succeeded")
			}
		}
		if err := application.Up(context.Background(), successor); err != nil {
			t.Fatal(err)
		}
		if observation.Attempts != 3 || provider.calls != 3 || !reflect.DeepEqual(observation.Command, spec.Command) {
			t.Fatalf("observation = %#v, provider calls = %d", observation, provider.calls)
		}
		assertEventOrder(t, events, "services-started", "readiness-succeeded", "successor-verified", "commit-recorded")
		assertReadinessAuthority(t, base, runner, application, 2, successor.Result.PlanDigest)
	})

	t.Run("never ready occupies pre-commit rollback boundary", func(t *testing.T) {
		base, layout, prior, successor, runner, application := newReadinessApp(t)
		defer func() { _ = application.stopProxy(layout) }()
		applyReadinessPredecessor(t, application, prior)
		providerDestination := declaredReadinessDestination(t, successor.Result.Plan.NetworkAllow)
		door := readinessDoorFromPlan(successor.Result.Plan.NetworkAllow)
		provider := &readinessProvider{output: strings.Repeat("diagnostic", 200)}
		spec := referenceActionSpec(providerDestination)
		var observation readinessObservation
		application.lifecycleCheckpoint = func(name string) {
			if name != "services-started" {
				return
			}
			observation = runReadinessWrapper(context.Background(), spec, door, provider.action(spec.Destination))
			if observation.Status != "ready" {
				// There is intentionally no production readiness-to-Up error
				// wiring yet. Fail the next real successor inspection so this proof
				// establishes placement and App's ordinary rollback cleanup only.
				runner.failPrefix = "inspect kenogram-w-g2"
			}
		}
		err := application.Up(context.Background(), successor)
		if err == nil || !strings.Contains(err.Error(), "verify successor") {
			t.Fatalf("apply error = %v", err)
		}
		if strings.Contains(err.Error(), "readiness") {
			t.Fatalf("fixture invented production readiness error wiring: %v", err)
		}
		if observation.Status != "attempts_exhausted" || observation.Attempts != referenceMaxAttempts {
			t.Fatalf("observation = %#v", observation)
		}
		if len(observation.Diagnostic) != referenceMaxOutputBytes || !observation.Truncated {
			t.Fatalf("diagnostic bytes = %d, truncated = %v", len(observation.Diagnostic), observation.Truncated)
		}
		raw, marshalErr := json.Marshal(observation)
		if marshalErr != nil || !json.Valid(raw) || strings.Count(string(raw), "\n") != 0 {
			t.Fatalf("observation JSON = %q, %v", raw, marshalErr)
		}
		assertReadinessAuthority(t, base, runner, application, 1, prior.Result.PlanDigest)
		if _, exists := runner.containers["kenogram-w-g2"]; exists {
			t.Fatal("failed successor survived rollback")
		}
	})

	t.Run("action cannot broaden inherited egress", func(t *testing.T) {
		const undeclared = "undeclared.fixture:443"
		base, layout, prior, successor, runner, application := newReadinessApp(t)
		defer func() { _ = application.stopProxy(layout) }()
		applyReadinessPredecessor(t, application, prior)
		door := readinessDoorFromPlan(successor.Result.Plan.NetworkAllow)
		provider := &readinessProvider{readyAfter: 1}
		spec := referenceActionSpec(undeclared)
		application.lifecycleCheckpoint = func(name string) {
			if name == "services-started" {
				observation := runReadinessWrapper(context.Background(), spec, door, provider.action(spec.Destination))
				if observation.Status != "ready" {
					runner.failPrefix = "inspect kenogram-w-g2"
				}
			}
		}
		if err := application.Up(context.Background(), successor); err == nil {
			t.Fatal("undeclared action unexpectedly committed")
		}
		if provider.calls != 0 || len(door.delivered) != 0 {
			t.Fatalf("undeclared provider was reached: calls=%d delivered=%v", provider.calls, door.delivered)
		}
		if _, broadened := door.allowed[undeclared]; broadened || len(door.allowed) != 1 {
			t.Fatalf("readiness action changed authority: %#v", door.allowed)
		}
		assertReadinessAuthority(t, base, runner, application, 1, prior.Result.PlanDigest)
	})

	t.Run("timeout interrupts one hanging attempt", func(t *testing.T) {
		spec := referenceActionSpec("not-invoked.fixture:443")
		spec.Timeout = 25 * time.Millisecond
		spec.MaxAttempts = 100
		observation := runReadinessWrapper(context.Background(), spec, nil, func(ctx context.Context, _ *readinessDoor) (bool, string, error) {
			<-ctx.Done()
			return false, strings.Repeat("late", 100), ctx.Err()
		})
		if observation.Status != "timeout" || observation.Attempts != 1 || len(observation.Diagnostic) > spec.MaxOutputBytes {
			t.Fatalf("timeout observation = %#v", observation)
		}
	})

	t.Run("deadline outranks final-attempt exhaustion", func(t *testing.T) {
		spec := referenceActionSpec("not-invoked.fixture:443")
		spec.Timeout = 25 * time.Millisecond
		spec.MaxAttempts = 1
		observation := runReadinessWrapper(context.Background(), spec, nil, func(ctx context.Context, _ *readinessDoor) (bool, string, error) {
			<-ctx.Done()
			return false, "late failure", ctx.Err()
		})
		if observation.Status != "timeout" || observation.Attempts != 1 {
			t.Fatalf("deadline observation = %#v", observation)
		}
	})

	t.Run("parent cancellation prevents a late success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attempted := false
		observation := runReadinessWrapper(ctx, referenceActionSpec("not-invoked.fixture:443"), nil, func(context.Context, *readinessDoor) (bool, string, error) {
			attempted = true
			return true, "too late", nil
		})
		if observation.Status != "canceled" || observation.Attempts != 0 || attempted {
			t.Fatalf("canceled observation = %#v, attempted=%t", observation, attempted)
		}
	})

	t.Run("absence preserves real acknowledgement-only path", func(t *testing.T) {
		base, layout, prior, successor, runner, application := newReadinessApp(t)
		defer func() { _ = application.stopProxy(layout) }()
		applyReadinessPredecessor(t, application, prior)
		var events []string
		application.lifecycleCheckpoint = func(name string) { events = append(events, name) }
		if err := application.Up(context.Background(), successor); err != nil {
			t.Fatal(err)
		}
		assertEventOrder(t, events, "services-started", "successor-verified", "commit-recorded")
		for _, event := range events {
			if strings.HasPrefix(event, "readiness-") {
				t.Fatalf("legacy apply invented readiness event: %v", events)
			}
		}
		assertReadinessAuthority(t, base, runner, application, 2, successor.Result.PlanDigest)
	})
}

func TestReadinessSemanticSIGKILLHelper(t *testing.T) {
	base := os.Getenv("KENOGRAM_TEST_READINESS_CRASH_STATE")
	if base == "" {
		return
	}
	_, layout, prior, successor, _, application := newReadinessAppAt(t, base)
	applyReadinessPredecessor(t, application, prior)
	destination := declaredReadinessDestination(t, successor.Result.Plan.NetworkAllow)
	spec := referenceActionSpec(destination)
	application.lifecycleCheckpoint = func(name string) {
		if name != "services-started" {
			return
		}
		observation := runReadinessWrapper(context.Background(), spec, readinessDoorFromPlan(successor.Result.Plan.NetworkAllow), persistentReadinessAction(filepath.Join(base, "readiness-action-count"), spec.Destination))
		if observation.Status != "ready" {
			t.Fatalf("crash action = %#v", observation)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	if err := application.Up(context.Background(), successor); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("readiness crash checkpoint was not reached; transition=%s", layout.Transition)
}

func TestReadinessSuccessBeforeCommitRecoversThenExplicitlyReruns(t *testing.T) {
	base := t.TempDir()
	crashAfterReadiness(t, base)
	layout := worldfs.For(base, "w")
	transition, err := layout.ReadTransition()
	if err != nil || transition.Phase != "rollback" || transition.Prior == nil || transition.Prior.Generation != 1 || transition.Successor.Generation != 2 {
		t.Fatalf("pre-commit crash transition = %#v, %v", transition, err)
	}
	if got := readActionCount(t, filepath.Join(base, "readiness-action-count")); got != 1 {
		t.Fatalf("action count after crash = %d", got)
	}

	prior, successor := crashPrepared("readiness-prior", 3), crashPrepared("readiness-successor", 4)
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
	application := readinessApp(base, runner)
	defer func() { _ = application.stopProxy(layout) }()
	if err := application.recoverTransition(context.Background(), layout); err != nil {
		t.Fatal(err)
	}
	if got := readActionCount(t, filepath.Join(base, "readiness-action-count")); got != 1 {
		t.Fatalf("recovery reran readiness action: count = %d", got)
	}
	assertReadinessAuthority(t, base, runner, application, 1, prior.Result.PlanDigest)

	destination := declaredReadinessDestination(t, successor.Result.Plan.NetworkAllow)
	spec := referenceActionSpec(destination)
	application.lifecycleCheckpoint = func(name string) {
		if name != "services-started" {
			return
		}
		observation := runReadinessWrapper(context.Background(), spec, readinessDoorFromPlan(successor.Result.Plan.NetworkAllow), persistentReadinessAction(filepath.Join(base, "readiness-action-count"), spec.Destination))
		if observation.Status != "ready" {
			t.Fatalf("explicit rerun = %#v", observation)
		}
	}
	if err := application.Up(context.Background(), successor); err != nil {
		t.Fatal(err)
	}
	if got := readActionCount(t, filepath.Join(base, "readiness-action-count")); got != 2 {
		t.Fatalf("explicit apply action count = %d", got)
	}
	assertReadinessAuthority(t, base, runner, application, 2, successor.Result.PlanDigest)
}

func newReadinessApp(t *testing.T) (string, worldfs.Layout, Prepared, Prepared, *crashRunner, *App) {
	t.Helper()
	return newReadinessAppAt(t, t.TempDir())
}

func newReadinessAppAt(t *testing.T, base string) (string, worldfs.Layout, Prepared, Prepared, *crashRunner, *App) {
	t.Helper()
	layout := worldfs.For(base, "w")
	prior, successor := crashPrepared("readiness-prior", 3), crashPrepared("readiness-successor", 4)
	runner := newCrashRunner(layout.WorkspacePath("/workspace"), filepath.Join(base, "fake-runtime.json"), prior.Result, successor.Result)
	return base, layout, prior, successor, runner, readinessApp(base, runner)
}

func readinessApp(base string, runner *crashRunner) *App {
	return &App{
		Backend: crashBackend(runner), BaseDir: base, Out: &bytes.Buffer{},
		Now:        func() time.Time { return time.Unix(1, 0) },
		Executable: filepath.Join(base, "fake-proxy.sh"), proxyReady: crashProxyReady,
	}
}

func applyReadinessPredecessor(t *testing.T, application *App, prior Prepared) {
	t.Helper()
	// crashProxyExecutable materializes the existing proxy fixture at the path
	// already selected by readinessApp.
	application.Executable = crashProxyExecutable(t, application.BaseDir)
	if err := application.Up(context.Background(), prior); err != nil {
		t.Fatal(err)
	}
	application.Now = func() time.Time { return time.Unix(2, 0) }
}

func assertReadinessAuthority(t *testing.T, base string, runner *crashRunner, application *App, generation int64, digest string) {
	t.Helper()
	layout := worldfs.For(base, "w")
	state, err := layout.ReadState()
	if err != nil || state.Generation != generation || state.PlanDigest != digest || state.Status != "running" {
		t.Fatalf("state = %#v, %v; want running g%d %s", state, err, generation, digest)
	}
	if _, err := os.Stat(layout.Transition); !os.IsNotExist(err) {
		t.Fatalf("transition remains after settlement: %v", err)
	}
	container := runner.containers[state.Container]
	if len(runner.containers) != 1 || container == nil || !container.Running || !container.Services["worker"] {
		t.Fatalf("runtime authority = %#v", runner.containers)
	}
	identity, alive := application.liveProxyIdentity(layout)
	if !alive || state.ProxyPID != identity.PID {
		t.Fatalf("authoritative network door identity = %#v alive=%t; state PID=%d", identity, alive, state.ProxyPID)
	}
}

func assertEventOrder(t *testing.T, events []string, ordered ...string) {
	t.Helper()
	next := 0
	for _, event := range events {
		if next < len(ordered) && event == ordered[next] {
			next++
		}
	}
	if next != len(ordered) {
		t.Fatalf("events %v do not contain order %v", events, ordered)
	}
}

func declaredReadinessDestination(t *testing.T, allows []plan.NetworkAllow) string {
	t.Helper()
	if len(allows) == 0 {
		t.Fatal("readiness fixture candidate has no declared destination")
	}
	return net.JoinHostPort(allows[0].Host, strconv.FormatInt(allows[0].Port, 10))
}

func persistentReadinessAction(path, destination string) readinessAttempt {
	return func(_ context.Context, door *readinessDoor) (bool, string, error) {
		var count int
		err := door.invoke(destination, func() {
			raw, readErr := os.ReadFile(path)
			if readErr == nil {
				count, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			}
			count++
			if writeErr := os.WriteFile(path, []byte(strconv.Itoa(count)+"\n"), 0o600); writeErr != nil {
				panic(writeErr)
			}
		})
		return err == nil, "provider action invoked", err
	}
}

func readActionCount(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func crashAfterReadiness(t *testing.T, base string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadinessSemanticSIGKILLHelper$")
	command.Env = append(os.Environ(), "KENOGRAM_TEST_READINESS_CRASH_STATE="+base)
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("readiness crash helper timed out: %v", ctx.Err())
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("readiness crash helper error = %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("readiness crash helper status = %v", exit.Sys())
	}
}

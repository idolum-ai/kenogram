// Package jobpodman implements the direct one-shot Podman adapter for the
// provider-independent governed-job core.
package jobpodman

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/job"
	"github.com/idolum-ai/kenogram/internal/jobcontract"
	"github.com/idolum-ai/kenogram/internal/jobenv"
	"github.com/idolum-ai/kenogram/internal/joblifecycle"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/sourcetree"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

const generation = int64(1)

const jobHelperPath = "/etc/kenogram/job-exec"
const jobLifecyclePath = "/etc/kenogram/job-lifecycle"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var ownerTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type attachedStarter interface {
	Start(string, []string, io.Reader, io.Writer, io.Writer) (attachedProcess, error)
}

type attachedProcess interface {
	Wait() error
	Kill() error
}

type lifecycleBoundStarter interface {
	BindLifecycle(string, []byte)
}

type execStarter struct{}

func (execStarter) Start(binary string, args []string, stdin io.Reader, stdout, stderr io.Writer) (attachedProcess, error) {
	command := exec.Command(binary, args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcess(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &execProcess{command: command, killGroup: syscall.Kill}, nil
}

type execProcess struct {
	command   *exec.Cmd
	killGroup func(int, syscall.Signal) error
}

func (p *execProcess) Wait() error { return p.command.Wait() }
func (p *execProcess) Kill() error {
	if p.command.Process == nil {
		return nil
	}
	if err := p.killGroup(-p.command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return errors.Join(err, p.command.Process.Kill())
	}
	return nil
}

type Runtime struct {
	Podman     *backend.Podman
	starter    attachedStarter
	tempDir    func(string, string) (string, error)
	now        func() time.Time
	goos       string
	token      func() (string, error)
	executable func() (string, error)

	mu            sync.Mutex
	name          string
	containerID   string
	ownerToken    string
	scratch       string
	helperSource  string
	mayOwn        bool
	process       *process
	mounts        []backend.Mount
	artifactRoots []*os.Root
	mountFacts    []jobcontract.RuntimeMountObservation
	lifecycleKey  []byte
	lifecycleFile string
	forced        bool
	started       bool
}

func New(podman *backend.Podman) *Runtime {
	if podman == nil {
		podman = backend.New(nil)
	}
	return &Runtime{Podman: podman, starter: execStarter{}, tempDir: os.MkdirTemp, now: time.Now, goos: runtime.GOOS, token: randomToken, executable: runningExecutable}
}

func (r *Runtime) Start(ctx context.Context, invocation job.Invocation, stdout, stderr io.Writer) (job.Process, error) {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil, errors.New("direct governed job runtime is one-shot")
	}
	r.started = true
	r.mu.Unlock()
	if r.goos != "linux" {
		return nil, fmt.Errorf("direct governed jobs require Linux, not %s", r.goos)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(invocation.Prepared.Result.Plan.NetworkAllow) != 0 {
		return nil, errors.New("direct governed jobs do not yet implement a network proxy")
	}
	if !pathIsAbsoluteCommand(invocation.Request.Command.Argv[0]) {
		return nil, errors.New("direct governed jobs require an absolute target command")
	}
	if invocation.Request.Command.WorkingDirectory != invocation.Prepared.Result.Plan.World.Workdir {
		return nil, errors.New("direct governed jobs require the declared world working directory")
	}
	if err := validateRuntimeMountCount(len(invocation.Prepared.Result.Plan.Workspace), len(invocation.Prepared.Result.Plan.Mounts)); err != nil {
		return nil, err
	}
	if err := validateRuntimeMountPaths(invocation.Prepared.Result.Plan); err != nil {
		return nil, err
	}
	if err := validateReadOnlyWritableAliases(invocation.Prepared.Result.Plan.Mounts); err != nil {
		return nil, err
	}
	if err := validateWritablePlanMounts(ctx, invocation.Prepared.Result.Plan.Mounts); err != nil {
		return nil, err
	}
	ownerToken, err := r.token()
	if err != nil {
		return nil, fmt.Errorf("create job owner token: %w", err)
	}
	name := jobContainerName(invocation.Request.JobID, ownerToken)
	scratch, err := r.tempDir("", "kenogram-job-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		os.RemoveAll(scratch)
		return nil, err
	}
	r.mu.Lock()
	r.name, r.ownerToken, r.scratch, r.mayOwn = name, ownerToken, scratch, false
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	layout := worldfs.For(scratch, "ephemeral")
	if err := layout.Ensure(); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	lifecycleHost := filepath.Join(scratch, "job-lifecycle")
	if err := os.Mkdir(lifecycleHost, 0o700); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	lifecycleKey := make([]byte, jobenv.LifecycleKeyBytes)
	if _, err := rand.Read(lifecycleKey); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	environmentRaw, err := targetEnvironment(invocation, lifecycleKey)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	readOnlySnapshots, err := snapshotReadOnlyMounts(ctx, scratch, invocation.Prepared.Result.Plan.Mounts)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	helperSource, helperFact, err := stageHelper(ctx, r.executable, scratch)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	if invocation.Provenance.ExecutableSHA256 == "" || helperFact.SHA256 != invocation.Provenance.ExecutableSHA256 {
		return r.refuseBeforeAdmission(errors.New("staged helper does not match retained executable provenance"))
	}
	r.mu.Lock()
	r.helperSource = helperSource
	r.mu.Unlock()
	mounts, err := jobMounts(layout, invocation.Prepared.Result, readOnlySnapshots)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	mounts = append(mounts, backend.Mount{Source: helperSource, Target: jobHelperPath, Mode: "ro"}, backend.Mount{Source: lifecycleHost, Target: jobLifecyclePath, Mode: "rw", NoExec: true})
	if err := validateBackendMountPaths(mounts); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	if err := validateWritableBackendMounts(ctx, mounts); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	mountFacts, err := captureMountFacts(ctx, mounts, invocation.Prepared.Result)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	// stageHelper already captured this exact file. Binding the two observations
	// makes the mounted helper's inode and digest part of executable provenance.
	for index := range mountFacts {
		if mountFacts[index].Target == jobHelperPath && (mountFacts[index].Device != helperFact.Device || mountFacts[index].Inode != helperFact.Inode || mountFacts[index].SHA256 != helperFact.SHA256) {
			return r.refuseBeforeAdmission(errors.New("staged helper identity changed before admission"))
		}
	}
	if err := r.Podman.Preflight(ctx); err != nil {
		return r.refuseBeforeAdmission(fmt.Errorf("runtime preflight: %w", err))
	}
	exists, err := r.Podman.Exists(ctx, name)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	if exists {
		return r.refuseBeforeAdmission(fmt.Errorf("ephemeral container name %q already exists", name))
	}
	r.mu.Lock()
	r.mountFacts, r.mounts, r.lifecycleKey, r.lifecycleFile = mountFacts, append([]backend.Mount{}, mounts...), append([]byte{}, lifecycleKey...), filepath.Join(lifecycleHost, joblifecycle.FileName)
	r.mu.Unlock()
	ownerLabels := map[string]string{"io.kenogram.job-owner": ownerToken, "io.kenogram.job-id": invocation.Request.JobID}
	providerPlan := publicProviderPlan(invocation.Prepared.Result)
	createdID, err := r.Podman.CreateGovernedJob(ctx, name, providerPlan, generation, mounts, ownerLabels, jobHelperPath)
	if err != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if evidence, inspectErr := r.Podman.Inspect(reconcileCtx, name); inspectErr == nil && containerIDPattern.MatchString(evidence.ID) && evidence.Name == name && evidence.Labels["io.kenogram.job-owner"] == ownerToken {
			r.recordOwnership(evidence.ID)
			return r.admittedFailure(invocation, err)
		}
		return nil, err
	}
	if !containerIDPattern.MatchString(createdID) {
		return r.admittedFailure(invocation, errors.New("created container ID is invalid"))
	}
	r.recordOwnership(createdID)
	created, err := r.Podman.Inspect(ctx, createdID)
	if err != nil || !containerIDPattern.MatchString(created.ID) || created.Name != name || created.Labels["io.kenogram.job-owner"] != ownerToken {
		return r.admittedFailure(invocation, errors.New("created container identity is unproved"))
	}
	if err := materializeCopies(ctx, r.Podman, layout, created.ID, invocation.Prepared.Result); err != nil {
		return r.admittedFailure(invocation, err)
	}
	if err := r.Podman.Start(ctx, created.ID); err != nil {
		return r.admittedFailure(invocation, err)
	}
	evidence, err := r.Podman.Inspect(ctx, created.ID)
	if err != nil {
		return r.admittedFailure(invocation, err)
	}
	if evidence.ID != created.ID {
		return r.admittedFailure(invocation, errors.New("container identity changed before target admission"))
	}
	if err := backend.VerifyNamed(evidence, providerPlan, generation, name, mounts); err != nil {
		return r.admittedFailure(invocation, fmt.Errorf("verify job runtime before target: %w", err))
	}
	imageDigest, err := validateObservedImage(invocation.Prepared.Result.Plan.World.Base, evidence)
	if err != nil {
		return r.admittedFailure(invocation, err)
	}
	if err := verifyMountFacts(ctx, mountFacts, mounts); err != nil {
		return r.admittedFailure(invocation, err)
	}
	if err := validateWritableBackendMounts(ctx, mounts); err != nil {
		return r.admittedFailure(invocation, fmt.Errorf("reinspect runtime-writable mounts before target admission: %w", err))
	}
	before, err := runtimeEvidence("before", r.now().UTC(), evidence, invocation, imageDigest, mountFacts)
	if err != nil {
		return r.admittedFailure(invocation, err)
	}
	args := []string{"exec", "--interactive", "--workdir", invocation.Request.Command.WorkingDirectory, created.ID, jobHelperPath, "_job-exec"}
	args = append(args, invocation.Request.Command.Argv...)
	if binder, ok := r.starter.(lifecycleBoundStarter); ok {
		binder.BindLifecycle(r.lifecycleFile, r.lifecycleKey)
	}
	attached, err := r.starter.Start(r.Podman.Binary, args, bytes.NewReader(environmentRaw), stdout, stderr)
	if err != nil {
		return r.admittedFailure(invocation, err)
	}
	process := &process{runtime: r, attached: attached, invocation: invocation, identity: job.RuntimeIdentity{
		Provider: "podman-cli", Generation: generation, ImageReference: invocation.Prepared.Result.Plan.World.Base,
		ImageDigest: imageDigest, Before: before,
	}, done: make(chan error, 1), clientDone: make(chan struct{})}
	r.mu.Lock()
	r.mounts, r.process = mounts, process
	r.mu.Unlock()
	go func() {
		process.done <- attached.Wait()
		close(process.clientDone)
	}()
	return process, nil
}

func (r *Runtime) refuseBeforeAdmission(cause error) (job.Process, error) {
	r.mu.Lock()
	scratch := r.scratch
	r.name, r.ownerToken, r.scratch, r.helperSource = "", "", "", ""
	r.mu.Unlock()
	if scratch != "" {
		if err := os.RemoveAll(scratch); err != nil {
			return nil, errors.Join(cause, fmt.Errorf("remove refused-job scratch: %w", err))
		}
	}
	return nil, cause
}

func (r *Runtime) recordOwnership(containerID string) {
	r.mu.Lock()
	r.containerID, r.mayOwn = containerID, true
	r.mu.Unlock()
}

func (r *Runtime) proveOwned(ctx context.Context) (backend.Evidence, error) {
	r.mu.Lock()
	id, owner := r.containerID, r.ownerToken
	r.mu.Unlock()
	if !containerIDPattern.MatchString(id) || !ownerTokenPattern.MatchString(owner) {
		return backend.Evidence{}, errors.New("container ownership authority is incomplete")
	}
	evidence, err := r.Podman.Inspect(ctx, id)
	if err != nil || evidence.ID != id || evidence.Labels["io.kenogram.job-owner"] != owner {
		return backend.Evidence{}, errors.New("container ownership is unproved")
	}
	return evidence, nil
}

func (r *Runtime) admittedFailure(invocation job.Invocation, cause error) (job.Process, error) {
	done := make(chan struct{})
	close(done)
	process := &process{runtime: r, invocation: invocation, waited: true, waitErr: cause, clientDone: done}
	r.mu.Lock()
	r.process = process
	r.mu.Unlock()
	return process, cause
}

func (r *Runtime) Cleanup(ctx context.Context, _ job.Invocation) jobcontract.CleanupResult {
	started := time.Now()
	r.mu.Lock()
	name, containerID, ownerToken, scratch, mayOwn, process := r.name, r.containerID, r.ownerToken, r.scratch, r.mayOwn, r.process
	artifactRoots := append([]*os.Root{}, r.artifactRoots...)
	forced := r.forced
	r.mu.Unlock()
	reasons := []string{}
	if process != nil && process.attached != nil && !process.finished() {
		forced = true
		if err := process.attached.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			reasons = append(reasons, "CLIENT_PROCESS_PRESENT")
		}
	}
	if process != nil && process.clientDone != nil {
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-process.clientDone:
			timer.Stop()
		case <-timer.C:
			reasons = append(reasons, "CLIENT_PROCESS_PRESENT")
		case <-ctx.Done():
			timer.Stop()
			reasons = append(reasons, "CLIENT_PROCESS_PRESENT")
		}
	}
	for _, root := range artifactRoots {
		if err := root.Close(); err != nil {
			reasons = append(reasons, "ARTIFACT_DESCRIPTOR_CLOSE_FAILED")
		}
	}
	if !mayOwn && name != "" && ownerTokenPattern.MatchString(ownerToken) {
		// A create response can be lost after the provider commits the object.
		// Reconcile with fresh authority during cleanup rather than abandoning a
		// potentially owned container forever.
		if evidence, inspectErr := r.Podman.Inspect(ctx, name); inspectErr == nil && containerIDPattern.MatchString(evidence.ID) && evidence.Name == name && evidence.Labels["io.kenogram.job-owner"] == ownerToken {
			containerID, mayOwn = evidence.ID, true
			r.recordOwnership(containerID)
		}
	}
	if mayOwn && containerID != "" {
		exists, existsErr := r.Podman.ExistsID(ctx, containerID)
		switch {
		case existsErr != nil:
			reasons = append(reasons, "CONTAINER_OWNERSHIP_UNPROVED")
		case !exists:
		case exists:
			evidence, inspectErr := r.Podman.Inspect(ctx, containerID)
			if inspectErr != nil || containerID == "" || evidence.ID != containerID || evidence.Labels["io.kenogram.job-owner"] != ownerToken {
				reasons = append(reasons, "CONTAINER_OWNERSHIP_UNPROVED")
			} else if err := r.Podman.Destroy(ctx, containerID); err != nil {
				reasons = append(reasons, "CONTAINER_REMOVE_FAILED")
			}
		}
	}
	containerAbsent := name == ""
	if containerID != "" {
		exists, err := r.Podman.ExistsID(ctx, containerID)
		if err != nil {
			reasons = append(reasons, "CONTAINER_ABSENCE_UNPROVED")
			containerAbsent = false
		} else {
			containerAbsent = !exists
			if exists {
				reasons = append(reasons, "CONTAINER_PRESENT")
			}
		}
	} else if name != "" {
		reasons = append(reasons, "CONTAINER_ABSENCE_UNPROVED")
		containerAbsent = false
	}
	if scratch != "" && containerAbsent {
		if err := os.RemoveAll(scratch); err != nil {
			reasons = append(reasons, "SCRATCH_REMOVE_FAILED")
		}
	} else if scratch != "" {
		reasons = append(reasons, "SCRATCH_RETAINED_FOR_UNPROVED_CONTAINER")
	}
	status := "complete"
	if len(reasons) != 0 || !containerAbsent {
		status = "incomplete"
	}
	sort.Strings(reasons)
	return jobcontract.CleanupResult{Status: status, ContainerAbsent: containerAbsent, ProxyAbsent: true, ProcessGroupEmpty: containerAbsent, Forced: forced, DurationNS: int64(time.Since(started)), Reasons: reasons}
}

type process struct {
	runtime    *Runtime
	attached   attachedProcess
	invocation job.Invocation
	identity   job.RuntimeIdentity
	done       chan error
	clientDone chan struct{}

	mu      sync.Mutex
	waited  bool
	waitErr error
}

func (p *process) Identity(ctx context.Context) (job.RuntimeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return job.RuntimeIdentity{}, err
	}
	return p.identity, nil
}

func (p *process) Wait(ctx context.Context) (jobcontract.TargetResult, error) {
	select {
	case err := <-p.done:
		return p.recordTerminal(err)
	case <-ctx.Done():
	}
	controlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	_, ownershipErr := p.runtime.proveOwned(controlCtx)
	stopErr := ownershipErr
	if stopErr == nil {
		stopErr = p.runtime.Podman.StopWithin(controlCtx, p.runtime.containerID, 1)
	}
	if stopErr != nil {
		if _, proofErr := p.runtime.proveOwned(controlCtx); proofErr == nil {
			_ = p.runtime.Podman.Kill(controlCtx, p.runtime.containerID)
		}
	}
	cancel()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-p.done:
		_, _ = p.recordTerminal(err)
	case <-timer.C:
		_ = p.attached.Kill()
	}
	p.mu.Lock()
	p.waited, p.waitErr = true, ctx.Err()
	p.mu.Unlock()
	return jobcontract.TargetResult{Kind: "unknown"}, ctx.Err()
}

func (p *process) recordTerminal(err error) (jobcontract.TargetResult, error) {
	if err != nil {
		p.mu.Lock()
		p.waited, p.waitErr = true, err
		p.mu.Unlock()
		return jobcontract.TargetResult{Kind: "unknown"}, err
	}
	record, readErr := joblifecycle.Read(p.runtime.lifecycleFile, p.runtime.lifecycleKey)
	if readErr != nil {
		p.mu.Lock()
		p.waited, p.waitErr = true, readErr
		p.mu.Unlock()
		return jobcontract.TargetResult{Kind: "unknown"}, fmt.Errorf("target-local lifecycle is unproved: %w", readErr)
	}
	result := jobcontract.TargetResult{Kind: "exited", ExitStatus: record.ExitStatus, Signal: record.Signal, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, DurationNS: &record.DurationNS}
	if record.Signal != nil {
		result.Kind = "signaled"
	}
	p.mu.Lock()
	p.waited, p.waitErr = true, nil
	p.mu.Unlock()
	return result, nil
}

func (p *process) finished() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waited
}

func (p *process) Finalize(ctx context.Context) (job.RuntimeFinalization, error) {
	if !p.finished() {
		return job.RuntimeFinalization{}, errors.New("target terminal observation is absent")
	}
	if _, err := p.runtime.proveOwned(ctx); err != nil {
		return job.RuntimeFinalization{}, err
	}
	if err := p.runtime.Podman.StopWithin(ctx, p.runtime.containerID, 1); err != nil {
		if _, proofErr := p.runtime.proveOwned(ctx); proofErr != nil {
			return job.RuntimeFinalization{}, errors.Join(err, proofErr)
		}
		if killErr := p.runtime.Podman.Kill(ctx, p.runtime.containerID); killErr != nil {
			return job.RuntimeFinalization{}, errors.Join(err, killErr)
		}
		p.runtime.mu.Lock()
		p.runtime.forced = true
		p.runtime.mu.Unlock()
	}
	evidence, err := p.runtime.Podman.Inspect(ctx, p.runtime.containerID)
	if err != nil {
		return job.RuntimeFinalization{}, err
	}
	if evidence.Running {
		return job.RuntimeFinalization{}, errors.New("runtime remains running after finalization stop")
	}
	if err := verifyStoppedEvidence(evidence, publicProviderPlan(p.invocation.Prepared.Result), p.runtime.name, p.runtime.containerID, p.runtime.ownerToken, p.identity.ImageDigest); err != nil {
		return job.RuntimeFinalization{}, fmt.Errorf("verify stopped job runtime: %w", err)
	}
	if err := verifyMountFacts(ctx, p.runtime.mountFacts, p.runtime.mounts); err != nil {
		return job.RuntimeFinalization{}, err
	}
	after, err := runtimeEvidence("after", p.runtime.now().UTC(), evidence, p.invocation, p.identity.ImageDigest, p.runtime.mountFacts)
	if err != nil {
		return job.RuntimeFinalization{}, err
	}
	artifacts, err := p.extractArtifacts(ctx)
	if err != nil {
		return job.RuntimeFinalization{After: after}, err
	}
	return job.RuntimeFinalization{After: after, Artifacts: artifacts}, nil
}

func verifyStoppedEvidence(evidence backend.Evidence, result plan.Result, name, containerID, ownerToken, imageDigest string) error {
	if containerID == "" || evidence.ID != containerID || evidence.Name != name || evidence.Labels["io.kenogram.job-owner"] != ownerToken ||
		evidence.Labels["io.kenogram.plan-digest"] != result.PlanDigest ||
		evidence.Labels["io.kenogram.declaration-digest"] != result.DeclarationDigest {
		return errors.New("stopped runtime identity or ownership labels disagree")
	}
	if !evidenceHasImageDigest(evidence, imageDigest) {
		return errors.New("stopped runtime image identity is absent or changed")
	}
	return nil
}

func targetEnvironment(invocation job.Invocation, lifecycleKey []byte) ([]byte, error) {
	items := make([]jobenv.Item, 0, len(invocation.Request.Command.Environment))
	for _, requested := range invocation.Request.Command.Environment {
		if requested.PublicValue != nil {
			items = append(items, jobenv.Item{Name: requested.Name, Value: []byte(*requested.PublicValue)})
			continue
		}
		var selected *plan.Copy
		for index := range invocation.Prepared.Result.Plan.Copies {
			copy := &invocation.Prepared.Result.Plan.Copies[index]
			if copy.Secret && copy.Target == requested.SecretFile {
				if selected != nil {
					return nil, errors.New("secret environment binding is ambiguous")
				}
				selected = copy
			}
		}
		if selected == nil {
			return nil, errors.New("secret environment binding is absent")
		}
		value, err := readSecretSource(*selected)
		if err != nil {
			return nil, err
		}
		items = append(items, jobenv.Item{Name: requested.Name, Value: value})
	}
	return jobenv.EncodeLaunch(items, lifecycleKey)
}

func readSecretSource(copy plan.Copy) ([]byte, error) {
	return readSecretSourceWithHook(copy, nil)
}

func readSecretSourceWithHook(copy plan.Copy, afterOpen func()) ([]byte, error) {
	file, err := os.Open(copy.Source)
	if err != nil {
		return nil, errors.New("open secret environment source")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > jobenv.MaximumValueBytes {
		return nil, errors.New("opened secret environment source is not a bounded regular file")
	}
	if afterOpen != nil {
		afterOpen()
	}
	raw, err := io.ReadAll(io.LimitReader(file, jobenv.MaximumValueBytes+1))
	if err != nil || len(raw) > jobenv.MaximumValueBytes || bytes.IndexByte(raw, 0) >= 0 {
		return nil, errors.New("secret environment source is unreadable, oversized, or contains NUL")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("secret environment source changed during read")
	}
	if observed := plan.DigestRegularCopyBytes(raw, opened.Mode()); observed != copy.SourceDigest {
		return nil, errors.New("opened secret environment bytes disagree with planned copy identity")
	}
	return raw, nil
}

func (p *process) extractArtifacts(ctx context.Context) ([]job.Artifact, error) {
	request := p.invocation.Request.Artifacts
	if request == nil {
		return nil, nil
	}
	destination := filepath.Join(p.runtime.scratch, "artifacts")
	command := []string{
		p.runtime.helperSource, "_job-collect", p.runtime.containerID, p.runtime.name,
		p.runtime.ownerToken, request.ContainerRoot, destination,
		fmt.Sprint(request.MaxEntries), fmt.Sprint(request.MaxBytes),
	}
	if err := p.runtime.Podman.RunUnshare(ctx, command); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return nil, err
	}
	// The artifact open closures share this descriptor until Cleanup removes the
	// scratch tree. It is intentionally left open for the core's immediate copy.
	artifacts := []job.Artifact{}
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." || entry.IsDir() {
			return nil
		}
		info, err := root.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("target artifact %q is not a regular file", path)
		}
		if int64(len(artifacts)) >= request.MaxEntries || info.Size() < 0 || info.Size() > request.MaxBytes-total {
			return errors.New("target artifact inventory exceeds request bounds")
		}
		total += info.Size()
		rel := filepath.ToSlash(path)
		artifacts = append(artifacts, job.Artifact{Path: rel, Open: func() (io.ReadCloser, error) {
			before, err := root.Lstat(rel)
			if err != nil || !before.Mode().IsRegular() {
				return nil, fmt.Errorf("artifact %q changed before open", rel)
			}
			file, err := root.Open(rel)
			if err != nil {
				return nil, err
			}
			opened, err := file.Stat()
			if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
				file.Close()
				return nil, fmt.Errorf("artifact %q changed during open", rel)
			}
			return file, nil
		}})
		return nil
	})
	if err != nil {
		root.Close()
		return nil, err
	}
	p.runtime.mu.Lock()
	p.runtime.artifactRoots = append(p.runtime.artifactRoots, root)
	p.runtime.mu.Unlock()
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts, nil
}

// CollectArtifacts is the narrow helper entered under `podman unshare`. It
// re-proves the stopped container identity, mounts its root only inside the
// provider user namespace, copies a bounded regular-file tree, and unmounts it.
func CollectArtifacts(ctx context.Context, podman *backend.Podman, containerID, name, ownerToken, containerRoot, destination string, maximumEntries, maximumBytes int64) (retErr error) {
	if podman == nil || name == "" || !containerIDPattern.MatchString(containerID) || !ownerTokenPattern.MatchString(ownerToken) ||
		!filepath.IsAbs(containerRoot) || filepath.Clean(containerRoot) != containerRoot ||
		!filepath.IsAbs(destination) || filepath.Clean(destination) != destination ||
		maximumEntries < 1 || maximumEntries > 10_000 || maximumBytes < 1 || maximumBytes > 1<<30 {
		return errors.New("artifact collector authority is invalid")
	}
	evidence, err := podman.Inspect(ctx, containerID)
	if err != nil || evidence.Running || evidence.ID != containerID || evidence.Labels["io.kenogram.job-owner"] != ownerToken {
		return errors.New("stopped artifact source identity is unproved")
	}
	mounted, err := podman.MountRoot(ctx, containerID)
	if err != nil {
		return err
	}
	defer func() {
		unmountCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		// Unmount is destructive provider state. Re-prove immutable identity and
		// owner immediately before issuing it.
		proof, proofErr := podman.Inspect(unmountCtx, containerID)
		if proofErr != nil || proof.ID != containerID || proof.Labels["io.kenogram.job-owner"] != ownerToken {
			retErr = errors.Join(retErr, errors.New("artifact source ownership changed before unmount"))
			return
		}
		retErr = errors.Join(retErr, podman.Unmount(unmountCtx, containerID))
	}()
	return collectArtifactTreeFromMountedRoot(ctx, mounted, containerRoot, destination, maximumEntries, maximumBytes)
}

func collectArtifactTreeFromMountedRoot(ctx context.Context, mounted, containerRoot, destination string, maximumEntries, maximumBytes int64) error {
	root, err := os.OpenRoot(mounted)
	if err != nil {
		return err
	}
	defer root.Close()
	relative := filepath.FromSlash(strings.TrimPrefix(containerRoot, "/"))
	current := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		info, err := current.Lstat(component)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			if current != root {
				current.Close()
			}
			return errors.New("target artifact root traversal contains a non-directory or symlink")
		}
		next, err := current.OpenRoot(component)
		if current != root {
			current.Close()
		}
		if err != nil {
			return err
		}
		opened, err := next.Stat(".")
		if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
			next.Close()
			return errors.New("target artifact root component changed during descriptor open")
		}
		current = next
	}
	if current != root {
		defer current.Close()
	}
	return collectArtifactRoot(ctx, current, destination, maximumEntries, maximumBytes)
}

func collectArtifactTree(ctx context.Context, source, destination string, maximumEntries, maximumBytes int64) error {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("target artifact root is not a directory")
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	return collectArtifactRoot(ctx, sourceRoot, destination, maximumEntries, maximumBytes)
}

func collectArtifactRoot(ctx context.Context, sourceRoot *os.Root, destination string, maximumEntries, maximumBytes int64) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer destinationRoot.Close()
	var files, nodes, total int64
	return fs.WalkDir(sourceRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		nodes++
		if nodes > 20_000 {
			return errors.New("target artifact traversal exceeds provider work bound")
		}
		if path == "." {
			return nil
		}
		info, err := sourceRoot.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("target artifact %q changed or is a symlink", path)
		}
		if info.IsDir() {
			return destinationRoot.MkdirAll(path, 0o700)
		}
		rel := filepath.ToSlash(path)
		if !info.Mode().IsRegular() || jobcontract.ValidateEvidenceRelativePath(rel) != nil {
			return fmt.Errorf("target artifact %q is not a regular safe path", path)
		}
		files++
		if files > maximumEntries || info.Size() < 0 || info.Size() > maximumBytes-total {
			return errors.New("target artifact inventory exceeds request bounds")
		}
		input, err := sourceRoot.Open(path)
		if err != nil {
			return err
		}
		opened, err := input.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			input.Close()
			return fmt.Errorf("target artifact %q changed during open", path)
		}
		output, err := destinationRoot.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, info.Size()+1))
		after, statErr := input.Stat()
		closeInputErr := input.Close()
		syncErr := output.Sync()
		closeOutputErr := output.Close()
		if copyErr != nil || statErr != nil || closeInputErr != nil || syncErr != nil || closeOutputErr != nil || written != info.Size() || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
			return errors.Join(copyErr, statErr, closeInputErr, syncErr, closeOutputErr, fmt.Errorf("target artifact %q changed during bounded copy", path))
		}
		total += written
		return nil
	})
}

func runtimeEvidence(phase string, observed time.Time, evidence backend.Evidence, invocation job.Invocation, imageDigest string, mounts []jobcontract.RuntimeMountObservation) ([]byte, error) {
	observation := jobcontract.RuntimeObservation{
		Schema: jobcontract.RuntimeObservationSchema, Phase: phase, ObservedAt: observed.Format(time.RFC3339Nano), Provider: "podman-cli",
		ContainerID: evidence.ID, ContainerName: evidence.Name, Running: evidence.Running,
		ImageReference: invocation.Prepared.Result.Plan.World.Base, ImageDigest: imageDigest,
		PlanSHA256: "sha256:" + invocation.Prepared.Result.EvidenceDigest, DeclarationSHA256: "sha256:" + invocation.Prepared.Result.DeclarationDigest, Generation: generation,
		NetworkMode: evidence.NetworkMode, IPCMode: evidence.IPCMode, IPCIsolated: evidence.IPCIsolatedFromHost,
		PIDMode: evidence.PIDMode, UTSMode: evidence.UTSMode, UserNSMode: evidence.UserNSMode, User: evidence.User,
		Hostname: evidence.Hostname, WorkingDirectory: evidence.WorkingDir, BoundingCaps: append([]string{}, evidence.BoundingCaps...),
		NoNewPrivileges: containsFold(evidence.SecurityOpt, "no-new-privileges"), SeccompMode: int64(evidence.SeccompMode), Devices: int64(evidence.Devices),
		UIDIdentity: mapsHostIdentity(evidence.UIDMap, int64(os.Getuid())), GIDIdentity: mapsHostIdentity(evidence.GIDMap, int64(os.Getgid())),
		MemoryBytes: evidence.Memory, NanoCPUs: evidence.NanoCPUs, PIDs: evidence.PIDs, Mounts: append([]jobcontract.RuntimeMountObservation{}, mounts...),
	}
	if phase == "after" {
		// These properties require a live process to inspect. Preserve their
		// absence honestly rather than copying a stale live-process claim.
		observation.IPCIsolated, observation.NoNewPrivileges, observation.UIDIdentity, observation.GIDIdentity = false, false, false, false
		observation.SeccompMode = 0
		observation.BoundingCaps = []string{}
	}
	if err := jobcontract.ValidateRuntimeObservation(observation); err != nil {
		return nil, err
	}
	return json.Marshal(observation)
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) || strings.HasPrefix(strings.ToLower(value), strings.ToLower(wanted)+"=") {
			return true
		}
	}
	return false
}

func mapsHostIdentity(mappings []backend.IDMap, id int64) bool {
	for _, mapping := range mappings {
		if mapping.Size > 0 && id >= mapping.HostID && id < mapping.HostID+mapping.Size && mapping.ContainerID+id-mapping.HostID == id {
			return true
		}
	}
	return false
}

func observedImageDigest(evidence backend.Evidence) string {
	for _, candidate := range []string{evidence.ImageDigest, evidence.ImageReference} {
		if digestPattern.MatchString(candidate) {
			return candidate
		}
	}
	return ""
}

func publicProviderPlan(result plan.Result) plan.Result {
	result.PlanDigest = result.EvidenceDigest
	return result
}

func validateObservedImage(reference string, evidence backend.Evidence) (string, error) {
	expected := ""
	if strings.HasPrefix(strings.ToLower(reference), "sha256:") {
		expected = strings.ToLower(reference)
	} else if index := strings.LastIndex(strings.ToLower(reference), "@sha256:"); index >= 0 {
		expected = strings.ToLower(reference[index+1:])
	}
	if expected != "" {
		if evidenceHasImageDigest(evidence, expected) {
			return expected, nil
		}
		return "", errors.New("observed image digest disagrees with declared pinned image")
	}
	if observed := observedImageDigest(evidence); observed != "" {
		return observed, nil
	}
	return "", errors.New("runtime did not expose an immutable image digest")
}

func evidenceHasImageDigest(evidence backend.Evidence, wanted string) bool {
	for _, candidate := range []string{evidence.ImageDigest, evidence.ImageReference} {
		if candidate == wanted {
			return true
		}
	}
	return false
}

func jobContainerName(jobID, ownerToken string) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + ownerToken))
	prefix := jobID
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	return "kenogram-job-" + prefix + "-" + hex.EncodeToString(sum[:6])
}

func randomToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func runningExecutable() (string, error) {
	if runtime.GOOS == "linux" {
		return "/proc/self/exe", nil
	}
	return os.Executable()
}

func stageHelper(ctx context.Context, resolve func() (string, error), scratch string) (string, jobcontract.RuntimeMountObservation, error) {
	source, err := resolve()
	if err != nil {
		return "", jobcontract.RuntimeMountObservation{}, fmt.Errorf("resolve governed job helper: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", jobcontract.RuntimeMountObservation{}, errors.New("open governed job helper")
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<30 {
		return "", jobcontract.RuntimeMountObservation{}, errors.New("kernel-resolved governed job helper is not a bounded regular file")
	}
	target := filepath.Join(scratch, "job-exec")
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return "", jobcontract.RuntimeMountObservation{}, err
	}
	written, copyErr := io.Copy(output, &contextReader{ctx: ctx, reader: io.LimitReader(input, (1<<30)+1)})
	afterInput, inputStatErr := input.Stat()
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || inputStatErr != nil || syncErr != nil || closeErr != nil || written != info.Size() || !os.SameFile(info, afterInput) || afterInput.Size() != info.Size() || !afterInput.ModTime().Equal(info.ModTime()) {
		return "", jobcontract.RuntimeMountObservation{}, errors.Join(copyErr, inputStatErr, syncErr, closeErr, errors.New("governed job helper copy is incomplete or changed during copy"))
	}
	if err := os.Chmod(target, 0o555); err != nil {
		return "", jobcontract.RuntimeMountObservation{}, err
	}
	fact, err := captureSourceFact(ctx, target, "", jobHelperPath, "ro", "helper", true)
	if err != nil {
		return "", jobcontract.RuntimeMountObservation{}, err
	}
	return target, fact, nil
}

func validateRuntimeMountCount(workspaces, declared int) error {
	const owned = 2
	if workspaces < 0 || declared < 0 || workspaces > jobcontract.MaxRuntimeMounts-owned || declared > jobcontract.MaxRuntimeMounts-owned-workspaces {
		return fmt.Errorf("runtime mount count exceeds %d", jobcontract.MaxRuntimeMounts)
	}
	return nil
}

func validateRuntimeMountPaths(result plan.Plan) error {
	for _, target := range result.Workspace {
		if err := backend.ValidateMountArgumentPath(target); err != nil {
			return fmt.Errorf("workspace mount target %q: %w", target, err)
		}
	}
	for _, mount := range result.Mounts {
		if err := backend.ValidateMountArgumentPath(mount.Source); err != nil {
			return fmt.Errorf("declared mount source %q: %w", mount.Source, err)
		}
		if err := backend.ValidateMountArgumentPath(mount.Target); err != nil {
			return fmt.Errorf("declared mount target %q: %w", mount.Target, err)
		}
	}
	return nil
}

func validateBackendMountPaths(mounts []backend.Mount) error {
	for _, mount := range mounts {
		if err := backend.ValidateMountArgumentPath(mount.Source); err != nil {
			return fmt.Errorf("runtime mount source %q: %w", mount.Source, err)
		}
		if err := backend.ValidateMountArgumentPath(mount.Target); err != nil {
			return fmt.Errorf("runtime mount target %q: %w", mount.Target, err)
		}
	}
	return nil
}

func validateWritablePlanMounts(ctx context.Context, mounts []plan.Mount) error {
	for _, mount := range mounts {
		if mount.Mode == "rw" {
			if err := inspectWritableSource(ctx, mount.Source); err != nil {
				return fmt.Errorf("inspect writable mount %q: %w", mount.Target, err)
			}
		}
	}
	return nil
}

func validateWritableBackendMounts(ctx context.Context, mounts []backend.Mount) error {
	for _, mount := range mounts {
		if mount.Mode == "rw" {
			if err := inspectWritableSource(ctx, mount.Source); err != nil {
				return fmt.Errorf("inspect runtime-writable mount %q: %w", mount.Target, err)
			}
		}
	}
	return nil
}

func inspectWritableSource(ctx context.Context, source string) error {
	protected := protectedRuntimeEndpoints()
	identities := make([]fs.FileInfo, 0, len(protected))
	for _, endpoint := range protected {
		if info, err := os.Lstat(endpoint); err == nil {
			identities = append(identities, info)
		}
		if info, err := os.Stat(endpoint); err == nil {
			identities = append(identities, info)
		}
	}
	return sourcetree.Inspect(ctx, source, func(entry sourcetree.Entry) error {
		if entry.Info.Mode()&os.ModeSocket != 0 {
			return fmt.Errorf("writable source contains socket node at %s", entry.Relative)
		}
		for _, protectedInfo := range identities {
			if os.SameFile(entry.Info, protectedInfo) {
				return fmt.Errorf("writable source contains a runtime-endpoint identity at %s", entry.Relative)
			}
		}
		return nil
	})
}

func validateReadOnlyWritableAliases(mounts []plan.Mount) error {
	for left := range mounts {
		for right := left + 1; right < len(mounts); right++ {
			if mounts[left].Mode == mounts[right].Mode {
				continue
			}
			leftInfo, leftErr := os.Lstat(mounts[left].Source)
			rightInfo, rightErr := os.Lstat(mounts[right].Source)
			if leftErr != nil || rightErr != nil {
				return errors.Join(leftErr, rightErr)
			}
			same := os.SameFile(leftInfo, rightInfo)
			overlap := hostPathsOverlap(canonicalHostPath(mounts[left].Source), canonicalHostPath(mounts[right].Source))
			if same || overlap {
				return fmt.Errorf("read-only and writable mount sources overlap at %q and %q", mounts[left].Source, mounts[right].Source)
			}
		}
	}
	return nil
}

func snapshotReadOnlyMounts(ctx context.Context, scratch string, mounts []plan.Mount) (map[string]string, error) {
	result := map[string]string{}
	root := filepath.Join(scratch, "read-only-mounts")
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, err
	}
	for index, mount := range mounts {
		if mount.Mode != "ro" {
			continue
		}
		info, err := os.Lstat(mount.Source)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("read-only mount %q source is invalid", mount.Target)
		}
		observedType := "file"
		if info.IsDir() {
			observedType = "directory"
		}
		if observedType != mount.SourceType {
			return nil, fmt.Errorf("read-only mount %q source type changed", mount.Target)
		}
		before, err := boundedSourceContentDigest(ctx, mount.Source, info)
		if err != nil {
			return nil, err
		}
		destination := filepath.Join(root, fmt.Sprintf("%03d", index))
		if err := sourcetree.Copy(ctx, mount.Source, destination); err != nil {
			return nil, err
		}
		afterInfo, err := os.Lstat(mount.Source)
		if err != nil || !os.SameFile(info, afterInfo) {
			return nil, errors.New("read-only mount source changed during snapshot")
		}
		after, err := boundedSourceContentDigest(ctx, mount.Source, afterInfo)
		if err != nil {
			return nil, err
		}
		stagedInfo, err := os.Lstat(destination)
		if err != nil {
			return nil, err
		}
		staged, err := boundedSourceContentDigest(ctx, destination, stagedInfo)
		if err != nil {
			return nil, err
		}
		if before != after || before != staged {
			return nil, fmt.Errorf("read-only mount %q changed during immutable snapshot", mount.Target)
		}
		result[mount.Target] = destination
	}
	return result, nil
}

func captureMountFacts(ctx context.Context, mounts []backend.Mount, result plan.Result) ([]jobcontract.RuntimeMountObservation, error) {
	facts := make([]jobcontract.RuntimeMountObservation, 0, len(mounts))
	for _, mount := range mounts {
		role, authoritySource := "", ""
		switch mount.Target {
		case jobHelperPath:
			role = "helper"
		case jobLifecyclePath:
			role = "lifecycle"
		default:
			for _, target := range result.Plan.Workspace {
				if mount.Target == target {
					role = "workspace"
				}
			}
			for _, declared := range result.Plan.Mounts {
				if mount.Target == declared.Target && mount.Mode == declared.Mode {
					role = "declared"
					authoritySource = declared.Source
				}
			}
		}
		if role == "" {
			return nil, fmt.Errorf("mount %q has no retained authority role", mount.Target)
		}
		fact, err := captureSourceFact(ctx, mount.Source, authoritySource, mount.Target, mount.Mode, role, mount.Mode == "ro")
		if err != nil {
			return nil, fmt.Errorf("capture mount %q identity: %w", mount.Target, err)
		}
		facts = append(facts, fact)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Target < facts[j].Target })
	return facts, nil
}

func captureSourceFact(ctx context.Context, source, authoritySource, target, mode, role string, content bool) (jobcontract.RuntimeMountObservation, error) {
	before, err := os.Lstat(source)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || (!before.IsDir() && !before.Mode().IsRegular()) {
		return jobcontract.RuntimeMountObservation{}, errors.New("source is not a regular non-symlink file or directory")
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return jobcontract.RuntimeMountObservation{}, errors.New("source device and inode are unavailable")
	}
	digest := ""
	if content {
		observed, err := boundedSourceContentDigest(ctx, source, before)
		if err != nil {
			return jobcontract.RuntimeMountObservation{}, err
		}
		digest = "sha256:" + observed
	}
	fileType := "file"
	if before.IsDir() {
		fileType = "directory"
	}
	semanticSource, err := jobcontract.RuntimeMountSource(role, target, mode, authoritySource, digest)
	if err != nil {
		return jobcontract.RuntimeMountObservation{}, err
	}
	return jobcontract.RuntimeMountObservation{Role: role, AuthoritySource: authoritySource, Source: semanticSource, Target: target, Mode: mode, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), FileType: fileType, SHA256: digest, IdentityVerified: true}, nil
}

func boundedSourceContentDigest(ctx context.Context, source string, info fs.FileInfo) (string, error) {
	digest, err := sourcetree.ContentDigest(ctx, source)
	if err != nil {
		return "", err
	}
	after, err := os.Lstat(source)
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return "", errors.New("source changed during bounded content digest")
	}
	return digest, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func verifyMountFacts(ctx context.Context, facts []jobcontract.RuntimeMountObservation, mounts []backend.Mount) error {
	for _, expected := range facts {
		observedSource := ""
		for _, mount := range mounts {
			if mount.Target == expected.Target && mount.Mode == expected.Mode {
				observedSource = mount.Source
			}
		}
		if observedSource == "" {
			return fmt.Errorf("mount %q observed source is unavailable", expected.Target)
		}
		observed, err := captureSourceFact(ctx, observedSource, expected.AuthoritySource, expected.Target, expected.Mode, expected.Role, expected.SHA256 != "")
		if err != nil || observed.Source != expected.Source || observed.Device != expected.Device || observed.Inode != expected.Inode || observed.FileType != expected.FileType || observed.Role != expected.Role || observed.AuthoritySource != expected.AuthoritySource || observed.SHA256 != expected.SHA256 {
			return fmt.Errorf("mount %q source identity changed", expected.Target)
		}
	}
	return nil
}

func jobMounts(layout worldfs.Layout, result plan.Result, readOnlySnapshots map[string]string) ([]backend.Mount, error) {
	mounts := []backend.Mount{}
	targets := []string{jobHelperPath, jobLifecyclePath}
	for _, target := range result.Plan.Workspace {
		if overlappingContainerTarget(target, targets) {
			return nil, fmt.Errorf("workspace target %q overlaps another runtime-owned target", target)
		}
		source, err := layout.EnsureWorkspace(target)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, backend.Mount{Source: source, Target: target, Mode: "rw"})
		targets = append(targets, target)
	}
	for _, mount := range result.Plan.Mounts {
		if overlappingContainerTarget(mount.Target, targets) {
			return nil, fmt.Errorf("mount target %q overlaps another runtime-owned target", mount.Target)
		}
		if err := validateMountSource(mount.Source); err != nil {
			return nil, fmt.Errorf("runtime control socket mount is forbidden: %s", mount.Source)
		}
		observedType, err := runtimeMountSourceType(mount.Source)
		if err != nil || observedType != mount.SourceType {
			return nil, fmt.Errorf("mount %q source type changed after planning", mount.Target)
		}
		source := mount.Source
		if mount.Mode == "ro" {
			var ok bool
			source, ok = readOnlySnapshots[mount.Target]
			if !ok {
				return nil, fmt.Errorf("read-only mount %q lacks an immutable snapshot", mount.Target)
			}
		}
		mounts = append(mounts, backend.Mount{Source: source, Target: mount.Target, Mode: mount.Mode})
		targets = append(targets, mount.Target)
	}
	return mounts, nil
}

func runtimeMountSourceType(source string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("mount source is a symlink")
	}
	if info.IsDir() {
		return "directory", nil
	}
	if info.Mode().IsRegular() {
		return "file", nil
	}
	return "", errors.New("mount source is neither a regular file nor a directory")
}

func overlappingContainerTarget(candidate string, existing []string) bool {
	for _, target := range existing {
		if candidate == target || strings.HasPrefix(candidate, target+"/") || strings.HasPrefix(target, candidate+"/") {
			return true
		}
	}
	return false
}

func pathIsAbsoluteCommand(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validateMountSource(source string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return errors.New("mount source is not a regular non-symlink file or directory")
	}
	canonical := canonicalHostPath(source)
	for _, item := range protectedRuntimeEndpoints() {
		if item != "" && hostPathsOverlap(canonical, canonicalHostPath(item)) {
			return fmt.Errorf("mount source overlaps protected host path %q", item)
		}
	}
	return nil
}

func protectedRuntimeEndpoints() []string {
	uid := fmt.Sprint(os.Getuid())
	protected := []string{
		filepath.Join("/run/user", uid, "podman", "podman.sock"),
		filepath.Join("/run/user", uid, "docker.sock"),
		"/run/podman/podman.sock",
		"/run/docker.sock",
		"/var/run/podman/podman.sock",
		"/var/run/docker.sock",
	}
	if runtimeDir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDir != "" {
		protected = append(protected,
			filepath.Join(runtimeDir, "podman", "podman.sock"),
			filepath.Join(runtimeDir, "docker.sock"),
		)
	}
	for _, variable := range []string{"CONTAINER_HOST", "DOCKER_HOST"} {
		if endpoint := strings.TrimSpace(os.Getenv(variable)); strings.HasPrefix(endpoint, "unix://") {
			protected = append(protected, strings.TrimPrefix(endpoint, "unix://"))
		}
	}
	return protected
}

func canonicalHostPath(value string) string {
	clean := filepath.Clean(value)
	if evaluated, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(evaluated)
	}
	return clean
}

func hostPathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+string(os.PathSeparator)) || strings.HasPrefix(right, left+string(os.PathSeparator))
}

func materializeCopies(ctx context.Context, podman *backend.Podman, layout worldfs.Layout, container string, result plan.Result) error {
	for index, copy := range result.Plan.Copies {
		live, err := plan.DigestSourceContext(ctx, copy.Source)
		if err != nil || live != copy.SourceDigest {
			return fmt.Errorf("copy source %s changed after planning", copy.Source)
		}
		stage, err := layout.StageSourceContext(ctx, generation, index, copy.Source, copy.Mode)
		if err != nil {
			return err
		}
		staged, err := plan.DigestSourceContext(ctx, stage)
		if err != nil || staged != copy.SourceDigest {
			return fmt.Errorf("staging did not preserve copy source %s", copy.Source)
		}
		if err := layout.ApplyStageMode(stage, copy.Mode); err != nil {
			return err
		}
		if err := podman.Copy(ctx, container, stage, copy.Target); err != nil {
			return err
		}
		if err := os.RemoveAll(stage); err != nil {
			return fmt.Errorf("remove materialized copy staging: %w", err)
		}
	}
	return nil
}

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

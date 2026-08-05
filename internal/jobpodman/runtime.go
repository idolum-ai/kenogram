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
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/worldfs"
)

const generation = int64(1)

const jobHelperPath = "/etc/kenogram/job-exec"

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
	environmentRaw, err := targetEnvironment(invocation)
	if err != nil {
		return nil, err
	}
	if err := r.Podman.Preflight(ctx); err != nil {
		return nil, fmt.Errorf("runtime preflight: %w", err)
	}
	ownerToken, err := r.token()
	if err != nil {
		return nil, fmt.Errorf("create job owner token: %w", err)
	}
	name := jobContainerName(invocation.Request.JobID, ownerToken)
	exists, err := r.Podman.Exists(ctx, name)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("ephemeral container name %q already exists", name)
	}
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
	layout := worldfs.For(scratch, "ephemeral")
	if err := layout.Ensure(); err != nil {
		return r.refuseBeforeAdmission(err)
	}
	helperSource, err := stageHelper(r.executable, scratch)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	r.mu.Lock()
	r.helperSource = helperSource
	r.mu.Unlock()
	mounts, err := jobMounts(layout, invocation.Prepared.Result)
	if err != nil {
		return r.refuseBeforeAdmission(err)
	}
	mounts = append(mounts, backend.Mount{Source: helperSource, Target: jobHelperPath, Mode: "ro"})
	ownerLabels := map[string]string{"io.kenogram.job-owner": ownerToken, "io.kenogram.job-id": invocation.Request.JobID}
	providerPlan := publicProviderPlan(invocation.Prepared.Result)
	if _, err := r.Podman.CreateGovernedJob(ctx, name, providerPlan, generation, mounts, ownerLabels, jobHelperPath); err != nil {
		if evidence, inspectErr := r.Podman.Inspect(ctx, name); inspectErr == nil && containerIDPattern.MatchString(evidence.ID) && evidence.Name == name && evidence.Labels["io.kenogram.job-owner"] == ownerToken {
			r.recordOwnership(evidence.ID)
			return r.admittedFailure(invocation, err)
		}
		return nil, err
	}
	created, err := r.Podman.Inspect(ctx, name)
	if err != nil || !containerIDPattern.MatchString(created.ID) || created.Name != name || created.Labels["io.kenogram.job-owner"] != ownerToken {
		return r.admittedFailure(invocation, errors.New("created container identity is unproved"))
	}
	r.recordOwnership(created.ID)
	if err := materializeCopies(ctx, r.Podman, layout, name, invocation.Prepared.Result); err != nil {
		return r.admittedFailure(invocation, err)
	}
	if err := r.Podman.Start(ctx, name); err != nil {
		return r.admittedFailure(invocation, err)
	}
	evidence, err := r.Podman.Inspect(ctx, name)
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
	before, err := runtimeEvidence("before", r.now().UTC(), evidence)
	if err != nil {
		return r.admittedFailure(invocation, err)
	}
	args := []string{"exec", "--interactive", "--workdir", invocation.Request.Command.WorkingDirectory, name, jobHelperPath, "_job-exec"}
	args = append(args, invocation.Request.Command.Argv...)
	startedAt := r.now().UTC()
	monotonic := time.Now()
	attached, err := r.starter.Start(r.Podman.Binary, args, bytes.NewReader(environmentRaw), stdout, stderr)
	if err != nil {
		return r.admittedFailure(invocation, err)
	}
	process := &process{runtime: r, attached: attached, invocation: invocation, identity: job.RuntimeIdentity{
		Generation: generation, ImageReference: invocation.Prepared.Result.Plan.World.Base,
		ImageDigest: imageDigest, Before: before,
	}, startedAt: startedAt, monotonic: monotonic, done: make(chan error, 1), clientDone: make(chan struct{})}
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
	name, containerID, scratch, mayOwn, process := r.name, r.containerID, r.scratch, r.mayOwn, r.process
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
	if mayOwn && name != "" {
		exists, existsErr := r.Podman.Exists(ctx, name)
		switch {
		case existsErr != nil:
			reasons = append(reasons, "CONTAINER_OWNERSHIP_UNPROVED")
		case !exists:
		case exists:
			evidence, inspectErr := r.Podman.Inspect(ctx, name)
			if inspectErr != nil || containerID == "" || evidence.ID != containerID || evidence.Name != name || evidence.Labels["io.kenogram.job-owner"] != r.ownerToken {
				reasons = append(reasons, "CONTAINER_OWNERSHIP_UNPROVED")
			} else if err := r.Podman.Destroy(ctx, name); err != nil {
				reasons = append(reasons, "CONTAINER_REMOVE_FAILED")
			}
		}
	}
	containerAbsent := true
	if name != "" {
		exists, err := r.Podman.Exists(ctx, name)
		if err != nil {
			reasons = append(reasons, "CONTAINER_ABSENCE_UNPROVED")
			containerAbsent = false
		} else {
			containerAbsent = !exists
			if exists {
				reasons = append(reasons, "CONTAINER_PRESENT")
			}
		}
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
	startedAt  time.Time
	monotonic  time.Time
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
	stopErr := p.runtime.Podman.StopWithin(controlCtx, p.runtime.name, 1)
	if stopErr != nil {
		_ = p.runtime.Podman.Kill(controlCtx, p.runtime.name)
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
	finished := p.runtime.now().UTC()
	duration := int64(time.Since(p.monotonic))
	result := jobcontract.TargetResult{Kind: "exited", StartedAt: p.startedAt.Format(time.RFC3339Nano), FinishedAt: finished.Format(time.RFC3339Nano), DurationNS: &duration}
	status := int64(0)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() < 0 || exit.ExitCode() > 255 || exit.ExitCode() >= 125 {
			p.mu.Lock()
			p.waited, p.waitErr = true, err
			p.mu.Unlock()
			return jobcontract.TargetResult{Kind: "unknown"}, err
		}
		status = int64(exit.ExitCode())
	}
	result.ExitStatus = &status
	p.mu.Lock()
	p.waited, p.waitErr = true, err
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
	if err := p.runtime.Podman.StopWithin(ctx, p.runtime.name, 1); err != nil {
		if killErr := p.runtime.Podman.Kill(ctx, p.runtime.name); killErr != nil {
			return job.RuntimeFinalization{}, errors.Join(err, killErr)
		}
		p.runtime.mu.Lock()
		p.runtime.forced = true
		p.runtime.mu.Unlock()
	}
	evidence, err := p.runtime.Podman.Inspect(ctx, p.runtime.name)
	if err != nil {
		return job.RuntimeFinalization{}, err
	}
	if evidence.Running {
		return job.RuntimeFinalization{}, errors.New("runtime remains running after finalization stop")
	}
	if err := verifyStoppedEvidence(evidence, publicProviderPlan(p.invocation.Prepared.Result), p.runtime.name, p.runtime.containerID, p.runtime.ownerToken, p.identity.ImageDigest); err != nil {
		return job.RuntimeFinalization{}, fmt.Errorf("verify stopped job runtime: %w", err)
	}
	after, err := runtimeEvidence("after", p.runtime.now().UTC(), evidence)
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

func targetEnvironment(invocation job.Invocation) ([]byte, error) {
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
	return jobenv.Encode(items)
}

func readSecretSource(copy plan.Copy) ([]byte, error) {
	before, err := os.Lstat(copy.Source)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("secret environment source is not a regular file")
	}
	if observed, err := plan.DigestSource(copy.Source); err != nil || observed != copy.SourceDigest {
		return nil, errors.New("secret environment source changed after planning")
	}
	file, err := os.Open(copy.Source)
	if err != nil {
		return nil, errors.New("open secret environment source")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("secret environment source changed during open")
	}
	raw, err := io.ReadAll(io.LimitReader(file, jobenv.MaximumValueBytes+1))
	if err != nil || len(raw) > jobenv.MaximumValueBytes || bytes.IndexByte(raw, 0) >= 0 {
		return nil, errors.New("secret environment source is unreadable, oversized, or contains NUL")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("secret environment source changed during read")
	}
	if observed, err := plan.DigestSource(copy.Source); err != nil || observed != copy.SourceDigest {
		return nil, errors.New("secret environment source changed after read")
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
		p.runtime.helperSource, "_job-collect", p.runtime.name, p.runtime.containerID,
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
func CollectArtifacts(ctx context.Context, podman *backend.Podman, name, containerID, ownerToken, containerRoot, destination string, maximumEntries, maximumBytes int64) (retErr error) {
	if podman == nil || name == "" || !containerIDPattern.MatchString(containerID) || !ownerTokenPattern.MatchString(ownerToken) ||
		!filepath.IsAbs(containerRoot) || filepath.Clean(containerRoot) != containerRoot ||
		!filepath.IsAbs(destination) || filepath.Clean(destination) != destination ||
		maximumEntries < 1 || maximumEntries > 10_000 || maximumBytes < 1 || maximumBytes > 1<<30 {
		return errors.New("artifact collector authority is invalid")
	}
	evidence, err := podman.Inspect(ctx, name)
	if err != nil || evidence.Running || evidence.ID != containerID || evidence.Name != name || evidence.Labels["io.kenogram.job-owner"] != ownerToken {
		return errors.New("stopped artifact source identity is unproved")
	}
	mounted, err := podman.MountRoot(ctx, name)
	if err != nil {
		return err
	}
	defer func() {
		unmountCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, podman.Unmount(unmountCtx, name))
	}()
	source := filepath.Join(mounted, filepath.FromSlash(strings.TrimPrefix(containerRoot, "/")))
	return collectArtifactTree(ctx, source, destination, maximumEntries, maximumBytes)
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

type runtimeObservation struct {
	Schema     string           `json:"schema"`
	Phase      string           `json:"phase"`
	ObservedAt string           `json:"observed_at"`
	Evidence   backend.Evidence `json:"evidence"`
}

func runtimeEvidence(phase string, observed time.Time, evidence backend.Evidence) ([]byte, error) {
	return json.Marshal(runtimeObservation{Schema: "kenogram.podman-runtime-observation.v1", Phase: phase, ObservedAt: observed.Format(time.RFC3339Nano), Evidence: evidence})
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

func stageHelper(resolve func() (string, error), scratch string) (string, error) {
	source, err := resolve()
	if err != nil {
		return "", fmt.Errorf("resolve governed job helper: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", errors.New("open governed job helper")
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<30 {
		return "", errors.New("governed job helper is not a bounded regular file")
	}
	target := filepath.Join(scratch, "job-exec")
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, (1<<30)+1))
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || written != info.Size() {
		return "", errors.Join(copyErr, syncErr, closeErr, errors.New("governed job helper copy is incomplete"))
	}
	if err := os.Chmod(target, 0o555); err != nil {
		return "", err
	}
	return target, nil
}

func jobMounts(layout worldfs.Layout, result plan.Result) ([]backend.Mount, error) {
	mounts := []backend.Mount{}
	targets := []string{jobHelperPath}
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
		mounts = append(mounts, backend.Mount{Source: mount.Source, Target: mount.Target, Mode: mount.Mode})
		targets = append(targets, mount.Target)
	}
	return mounts, nil
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
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("mount source is not a regular file or directory")
	}
	canonical := canonicalHostPath(source)
	uid := fmt.Sprint(os.Getuid())
	protected := []string{
		filepath.Join("/run/user", uid, "podman", "podman.sock"),
		filepath.Join("/run/user", uid, "docker.sock"),
		"/run/podman/podman.sock",
		"/run/docker.sock",
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
	for _, item := range protected {
		if item != "" && hostPathsOverlap(canonical, canonicalHostPath(item)) {
			return fmt.Errorf("mount source overlaps protected host path %q", item)
		}
	}
	return nil
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
		live, err := plan.DigestSource(copy.Source)
		if err != nil || live != copy.SourceDigest {
			return fmt.Errorf("copy source %s changed after planning", copy.Source)
		}
		stage, err := layout.StageSource(generation, index, copy.Source, copy.Mode)
		if err != nil {
			return err
		}
		staged, err := plan.DigestSource(stage)
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

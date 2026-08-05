// Package job implements provider-independent governed-job execution and
// offline evidence verification.
package job

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/idolum-ai/kenogram/internal/app"
	"github.com/idolum-ai/kenogram/internal/jobcontract"
	"github.com/idolum-ai/kenogram/internal/plan"
)

// Runtime is the contained-execution boundary. The executor, not the runtime,
// owns deadlines, evidence publication, and the final truth status.
type Runtime interface {
	Start(context.Context, Invocation, io.Writer, io.Writer) (Process, error)
	Cleanup(context.Context, Invocation) jobcontract.CleanupResult
}

type Process interface {
	Identity(context.Context) (RuntimeIdentity, error)
	BeginWait()
	Wait(context.Context) (jobcontract.TargetResult, error)
	JoinWait(context.Context) error
	BeginFinalization()
	Finalize(context.Context) (RuntimeFinalization, error)
	JoinFinalization(context.Context) error
}

type Invocation struct {
	Request    jobcontract.Request
	Prepared   app.Prepared
	Provenance jobcontract.Provenance
}

type RuntimeIdentity struct {
	Provider       string
	Generation     int64
	ImageReference string
	ImageDigest    string
	Before         []byte
}

type RuntimeFinalization struct {
	After     []byte
	Artifacts []Artifact
}

type Artifact struct {
	Path string
	Open func() (io.ReadCloser, error)
}

type artifactInventory struct {
	Schema  string                   `json:"schema"`
	Root    string                   `json:"root"`
	Entries []artifactInventoryEntry `json:"entries"`
}

type artifactInventoryEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type BuildIdentity struct {
	Version    string
	Commit     string
	SourceDate string
}

type Executor struct {
	Runtime       Runtime
	Executable    string
	Build         BuildIdentity
	Now           func() time.Time
	AfterFileSync func(string) error
}

type Outcome struct {
	Result jobcontract.Result
	Sealed bool
}

type executionError struct{ err error }

func (e *executionError) Error() string { return e.err.Error() }
func (e *executionError) Unwrap() error { return e.err }

// IsPostIdentityError reports whether request and declaration identity were
// established before an execution/publication failure. Callers use this to
// distinguish a failed governed observation from invalid invocation syntax.
func IsPostIdentityError(err error) bool {
	var target *executionError
	return errors.As(err, &target)
}

func (e Executor) Run(ctx context.Context, requestRaw []byte, evidenceDir string) (outcome Outcome, retErr error) {
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	identityEstablished := false
	defer func() {
		if retErr != nil && identityEstablished && !IsPostIdentityError(retErr) {
			retErr = &executionError{err: retErr}
		}
	}()
	request, err := jobcontract.ParseRequest(requestRaw)
	if err != nil {
		return Outcome{}, err
	}
	if e.Runtime == nil {
		return Outcome{}, errors.New("governed job runtime is unavailable")
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	declarationRaw, err := readBoundRegular(ctx, request.Declaration.Path, jobcontract.MaximumRequestBytes)
	if err != nil {
		return Outcome{}, fmt.Errorf("read bound declaration: %w", err)
	}
	if digest(declarationRaw) != request.Declaration.SHA256 {
		return Outcome{}, errors.New("declaration digest does not match job request")
	}
	prepared, err := app.PrepareBytesContext(ctx, declarationRaw, request.Declaration.Path)
	if err != nil {
		return Outcome{}, fmt.Errorf("prepare bound declaration: %w", err)
	}
	if err := validateSecretEnvironment(request, prepared.Result); err != nil {
		return Outcome{}, err
	}
	identityEstablished = true
	planRaw, err := plan.JSON(prepared.Result)
	if err != nil {
		return Outcome{}, err
	}
	provenance, provenanceRaw, err := executableProvenanceContext(ctx, e.Executable, e.Build)
	if err != nil {
		return Outcome{}, err
	}
	evidence, err := createEvidence(evidenceDir, e.AfterFileSync)
	if err != nil {
		return Outcome{}, err
	}
	defer evidence.Close()
	for _, file := range []struct {
		name string
		kind string
		raw  []byte
	}{
		{"declaration.toml", "declaration", declarationRaw},
		{"plan.json", "plan", planRaw},
		{"provenance.json", "provenance", provenanceRaw},
		{"request.json", "request", requestRaw},
	} {
		if err := evidence.Write(file.name, file.kind, file.raw); err != nil {
			return Outcome{}, err
		}
	}
	stdout, err := evidence.Stream("stdout.bin", "stdout", request.Limits.StdoutMaxBytes)
	if err != nil {
		return Outcome{}, err
	}
	stderr, err := evidence.Stream("stderr.bin", "stderr", request.Limits.StderrMaxBytes)
	if err != nil {
		return Outcome{}, err
	}

	invocation := Invocation{Request: request, Prepared: prepared, Provenance: provenance}
	cleanupDone := false
	defer func() {
		if cleanupDone {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Duration(request.Limits.FinalizeNS))
		defer cancel()
		_, _ = cleanupRuntimeBounded(cleanupCtx, e.Runtime, invocation)
	}()
	result := jobcontract.Result{
		Schema: jobcontract.ResultSchema, JobID: request.JobID, Status: "refused",
		RequestSHA256: digest(requestRaw), EvidenceManifest: "manifest.json",
		Identity: jobcontract.ExecutionIdentity{
			DeclarationSHA256: request.Declaration.SHA256,
			PlanSHA256:        prefixedDigest(prepared.Result.EvidenceDigest),
			ImageReference:    prepared.Result.Plan.World.Base,
			ProvenanceSHA256:  digest(provenanceRaw),
		},
		Target:  jobcontract.TargetResult{Kind: "not_started"},
		Reasons: []string{"RUNTIME_START_FAILED"},
	}

	executionCtx, cancelExecution := context.WithTimeout(ctx, time.Duration(request.Limits.TimeoutNS))
	executionStarted := executionCtx.Err() == nil
	process, startErr := startRuntimeBounded(executionCtx, e.Runtime, invocation, stdout, stderr, time.Duration(request.Limits.FinalizeNS))
	admitted := process != nil || (executionStarted && (errors.Is(startErr, context.DeadlineExceeded) || errors.Is(startErr, context.Canceled)))
	if startErr == nil && process == nil {
		startErr = errors.New("runtime returned no process observation")
	}
	var runtimeBefore, runtimeAfter []byte
	artifactInventoryWritten := false
	if startErr == nil {
		identity, identityErr := identityProcessBounded(executionCtx, process)
		result.Identity.Generation = identity.Generation
		result.Identity.RuntimeProvider = identity.Provider
		if validRuntimeDigest(identity.ImageDigest) {
			result.Identity.ImageDigest = identity.ImageDigest
		}
		runtimeBefore = identity.Before
		if err := jobcontract.ValidateJSONDocument(runtimeBefore, jobcontract.MaximumManifestBytes); err != nil {
			startErr = fmt.Errorf("runtime before evidence is invalid: %w", err)
		}
		if validateErr := validateRuntimeIdentity(identity, prepared.Result); validateErr != nil {
			identityErr = errors.Join(identityErr, validateErr)
		}
		startErr = errors.Join(startErr, identityErr)
	}
	if startErr != nil && admitted {
		result.Status = "incomplete"
		result.Target = jobcontract.TargetResult{Kind: "unknown"}
		result.Reasons = []string{"RUNTIME_START_OBSERVATION_FAILED"}
	}
	if startErr == nil {
		result.Target, err = waitProcessBounded(executionCtx, process)
		cancelExecution()
		if targetErr := jobcontract.ValidateTargetResult(result.Target); targetErr != nil {
			result.Target = jobcontract.TargetResult{Kind: "unknown"}
			err = errors.Join(err, targetErr)
		}
		if err != nil {
			result.Status = "incomplete"
			result.Reasons = []string{"TARGET_OBSERVATION_FAILED"}
		} else {
			result.Status = "complete"
			result.Reasons = []string{}
		}
		finalStarted := e.Now().UTC()
		finalMonotonic := time.Now()
		finalCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(request.Limits.FinalizeNS))
		defer cancelFinalize()
		var final RuntimeFinalization
		finalErr := process.JoinWait(finalCtx)
		if finalErr == nil {
			final, finalErr = finalizeProcessBounded(finalCtx, process)
		}
		finalFinished := e.Now().UTC()
		result.Finalization = interval(finalStarted, finalFinished, time.Since(finalMonotonic))
		runtimeAfter = final.After
		if runtimeErr := jobcontract.ValidateJSONDocument(runtimeAfter, jobcontract.MaximumManifestBytes); runtimeErr != nil {
			finalErr = errors.Join(finalErr, runtimeErr)
		}
		if result.Identity.RuntimeProvider == "podman-cli" && finalErr == nil {
			beforeObservation, beforeErr := jobcontract.ParseRuntimeObservation(runtimeBefore)
			afterObservation, afterErr := jobcontract.ParseRuntimeObservation(runtimeAfter)
			observationErr := errors.Join(beforeErr, afterErr)
			if observationErr == nil {
				observationErr = verifyRuntimeObservations(beforeObservation, afterObservation, result, request, prepared.Result, provenance)
			}
			if observationErr != nil {
				finalErr = fmt.Errorf("runtime observation contract: %w", observationErr)
				result.Reasons = appendReason(result.Reasons, "RUNTIME_OBSERVATION_INVALID")
			}
		}
		if finalErr != nil {
			result.Status = "incomplete"
			result.Reasons = appendReason(result.Reasons, "FINALIZATION_FAILED")
		}
		if request.Artifacts != nil {
			artifacts := final.Artifacts
			if finalErr != nil {
				artifacts = nil
			}
			inventory, artifactErr := evidence.WriteArtifacts(finalCtx, *request.Artifacts, artifacts)
			if artifactErr != nil {
				result.Status = "incomplete"
				result.Reasons = appendReason(result.Reasons, "ARTIFACT_COLLECTION_FAILED")
				inventory = artifactInventory{Schema: "kenogram.target-artifact-inventory.v1", Root: request.Artifacts.ContainerRoot, Entries: []artifactInventoryEntry{}}
			}
			inventoryRaw, marshalErr := marshalDocument(inventory)
			if marshalErr != nil {
				return Outcome{}, marshalErr
			}
			if err := evidence.Write("target-inventory.json", "target_inventory", inventoryRaw); err != nil {
				return Outcome{}, err
			}
			artifactInventoryWritten = true
		} else if request.Artifacts == nil && len(final.Artifacts) != 0 {
			result.Status = "incomplete"
			result.Reasons = appendReason(result.Reasons, "UNREQUESTED_ARTIFACTS")
		}
		cancelFinalize()
	} else {
		cancelExecution()
		now := e.Now().UTC()
		result.Finalization = interval(now, now, 0)
	}
	if request.Artifacts != nil && !artifactInventoryWritten {
		inventoryCtx, cancelInventory := context.WithTimeout(context.Background(), time.Duration(request.Limits.FinalizeNS))
		inventory, artifactErr := evidence.WriteArtifacts(inventoryCtx, *request.Artifacts, nil)
		cancelInventory()
		if artifactErr != nil {
			return Outcome{}, artifactErr
		}
		inventoryRaw, marshalErr := marshalDocument(inventory)
		if marshalErr != nil {
			return Outcome{}, marshalErr
		}
		if err := evidence.Write("target-inventory.json", "target_inventory", inventoryRaw); err != nil {
			return Outcome{}, err
		}
	}

	cleanupStarted := time.Now()
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(request.Limits.FinalizeNS))
	var cleanupErr error
	if process != nil {
		cleanupErr = process.JoinWait(cleanupCtx)
		if cleanupErr == nil {
			cleanupErr = process.JoinFinalization(cleanupCtx)
		}
	}
	if cleanupErr == nil {
		result.Cleanup, cleanupErr = cleanupRuntimeBounded(cleanupCtx, e.Runtime, invocation)
	}
	cancelCleanup()
	cleanupDone = true
	result.Cleanup.DurationNS = boundedDuration(time.Since(cleanupStarted))
	if cleanupErr != nil {
		result.Cleanup = jobcontract.CleanupResult{Status: "incomplete", DurationNS: boundedDuration(time.Since(cleanupStarted)), Reasons: []string{"CLEANUP_DEADLINE_EXCEEDED"}}
	}
	if cleanupErr := jobcontract.ValidateCleanupResult(result.Cleanup); cleanupErr != nil {
		result.Cleanup = jobcontract.CleanupResult{Status: "incomplete", DurationNS: boundedDuration(time.Since(cleanupStarted)), Reasons: []string{"CLEANUP_OBSERVATION_INVALID"}}
	}
	if result.Cleanup.Status != "complete" {
		result.Status = "incomplete"
		result.Reasons = appendReason(result.Reasons, "CLEANUP_INCOMPLETE")
	}
	if startErr != nil && result.Cleanup.Status == "complete" {
		if admitted {
			result.Status = "incomplete"
			result.Target = jobcontract.TargetResult{Kind: "unknown"}
			result.Reasons = []string{"RUNTIME_START_OBSERVATION_FAILED"}
		} else {
			result.Status = "refused"
			result.Reasons = []string{"RUNTIME_START_FAILED"}
		}
	}

	if len(runtimeBefore) == 0 {
		runtimeBefore = []byte("{}\n")
	}
	if len(runtimeAfter) == 0 {
		runtimeAfter = []byte("{}\n")
	}
	result.Identity.RuntimeSHA256 = runtimeEvidenceDigest(runtimeBefore, runtimeAfter)
	if err := evidence.Write("runtime-before.json", "runtime", runtimeBefore); err != nil {
		return Outcome{}, err
	}
	if err := evidence.Write("runtime-after.json", "runtime", runtimeAfter); err != nil {
		return Outcome{}, err
	}
	result.Stdout, err = stdout.CloseResult()
	if err != nil {
		return Outcome{}, err
	}
	result.Stderr, err = stderr.CloseResult()
	if err != nil {
		return Outcome{}, err
	}
	if result.Stdout.Truncated || result.Stderr.Truncated {
		result.Status = "incomplete"
		result.Reasons = appendReason(result.Reasons, "OUTPUT_TRUNCATED")
	}
	if result.Status == "complete" && (result.Identity.Generation < 1 || result.Identity.ImageDigest == "") {
		result.Status = "incomplete"
		result.Reasons = appendReason(result.Reasons, "RUNTIME_IDENTITY_INCOMPLETE")
	}
	result.Reasons = sortedUnique(result.Reasons)
	resultRaw, err := marshalDocument(result)
	if err != nil {
		return Outcome{}, err
	}
	if _, err := jobcontract.ParseResult(resultRaw); err != nil {
		return Outcome{}, fmt.Errorf("validate produced result: %w", err)
	}
	if err := evidence.Write("result.json", "result", resultRaw); err != nil {
		return Outcome{}, err
	}
	if _, err := evidence.Seal(request.JobID, result.RequestSHA256, digest(resultRaw), e.Now().UTC()); err != nil {
		return Outcome{}, err
	}
	_ = provenance
	return Outcome{Result: result, Sealed: true}, nil
}

type runtimeStartObservation struct {
	process Process
	err     error
}

func startRuntimeBounded(ctx context.Context, runtime Runtime, invocation Invocation, stdout, stderr io.Writer, cleanupTimeout time.Duration) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(chan runtimeStartObservation, 1)
	go func() {
		process, err := runtime.Start(ctx, invocation, stdout, stderr)
		result <- runtimeStartObservation{process: process, err: err}
	}()
	select {
	case observed := <-result:
		if err := ctx.Err(); err != nil {
			return observed.process, errors.Join(observed.err, err)
		}
		return observed.process, observed.err
	case <-ctx.Done():
		go func() {
			<-result
			lateCleanup(runtime, invocation, cleanupTimeout)
		}()
		return nil, ctx.Err()
	}
}

func lateCleanup(runtime Runtime, invocation Invocation, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _ = cleanupRuntimeBounded(ctx, runtime, invocation)
}

type targetObservation struct {
	target jobcontract.TargetResult
	err    error
}

type identityObservation struct {
	identity RuntimeIdentity
	err      error
}

func identityProcessBounded(ctx context.Context, process Process) (RuntimeIdentity, error) {
	result := make(chan identityObservation, 1)
	go func() {
		identity, err := process.Identity(ctx)
		result <- identityObservation{identity: identity, err: err}
	}()
	select {
	case observed := <-result:
		if err := ctx.Err(); err != nil {
			return RuntimeIdentity{}, errors.Join(observed.err, err)
		}
		return observed.identity, observed.err
	case <-ctx.Done():
		return RuntimeIdentity{}, ctx.Err()
	}
}

func waitProcessBounded(ctx context.Context, process Process) (jobcontract.TargetResult, error) {
	process.BeginWait()
	result := make(chan targetObservation, 1)
	go func() {
		target, err := process.Wait(ctx)
		result <- targetObservation{target: target, err: err}
	}()
	select {
	case observed := <-result:
		if err := ctx.Err(); err != nil {
			return jobcontract.TargetResult{Kind: "unknown"}, err
		}
		return observed.target, observed.err
	case <-ctx.Done():
		return jobcontract.TargetResult{Kind: "unknown"}, ctx.Err()
	}
}

type finalizationObservation struct {
	final RuntimeFinalization
	err   error
}

func finalizeProcessBounded(ctx context.Context, process Process) (RuntimeFinalization, error) {
	process.BeginFinalization()
	result := make(chan finalizationObservation, 1)
	go func() {
		final, err := process.Finalize(ctx)
		result <- finalizationObservation{final: final, err: err}
	}()
	select {
	case observed := <-result:
		if err := ctx.Err(); err != nil {
			return RuntimeFinalization{}, err
		}
		return observed.final, observed.err
	case <-ctx.Done():
		return RuntimeFinalization{}, ctx.Err()
	}
}

func cleanupRuntimeBounded(ctx context.Context, runtime Runtime, invocation Invocation) (jobcontract.CleanupResult, error) {
	result := make(chan jobcontract.CleanupResult, 1)
	go func() { result <- runtime.Cleanup(ctx, invocation) }()
	select {
	case observed := <-result:
		if err := ctx.Err(); err != nil {
			return jobcontract.CleanupResult{}, err
		}
		return observed, nil
	case <-ctx.Done():
		return jobcontract.CleanupResult{}, ctx.Err()
	}
}

func validateRuntimeIdentity(identity RuntimeIdentity, prepared plan.Result) error {
	if identity.Generation < 1 || identity.Generation > jobcontract.MaximumWireInteger {
		return errors.New("runtime generation is outside the evidence boundary")
	}
	if identity.ImageReference != prepared.Plan.World.Base {
		return errors.New("runtime image reference disagrees with declared authority")
	}
	if !validRuntimeDigest(identity.ImageDigest) {
		return errors.New("runtime image digest is invalid or absent")
	}
	const marker = "@sha256:"
	if index := strings.LastIndex(prepared.Plan.World.Base, marker); index >= 0 {
		declaredDigest := prepared.Plan.World.Base[index+1:]
		if identity.ImageDigest != declaredDigest {
			return errors.New("observed image digest disagrees with pinned declaration")
		}
	}
	return nil
}

func validRuntimeDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func validateSecretEnvironment(request jobcontract.Request, prepared plan.Result) error {
	secretTargets := map[string]int{}
	for _, copy := range prepared.Plan.Copies {
		if copy.Secret {
			secretTargets[copy.Target]++
		}
	}
	for _, item := range request.Command.Environment {
		if item.SecretFile == "" {
			continue
		}
		if secretTargets[item.SecretFile] != 1 {
			return fmt.Errorf("secret_file %q is not bound to exactly one declaration-owned secret copy target", item.SecretFile)
		}
	}
	return nil
}

func interval(start, finish time.Time, duration time.Duration) jobcontract.FinalizationResult {
	return jobcontract.FinalizationResult{StartedAt: start.Format(time.RFC3339Nano), FinishedAt: finish.Format(time.RFC3339Nano), DurationNS: boundedDuration(duration)}
}

func boundedDuration(value time.Duration) int64 {
	if value < 0 {
		return 0
	}
	if int64(value) > jobcontract.MaximumWireInteger {
		return jobcontract.MaximumWireInteger
	}
	return int64(value)
}

func appendReason(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	return values
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func prefixedDigest(value string) string {
	if strings.HasPrefix(value, "sha256:") {
		return value
	}
	return "sha256:" + value
}

func runtimeEvidenceDigest(before, after []byte) string {
	hash := sha256.New()
	for _, raw := range [][]byte{before, after} {
		fmt.Fprintf(hash, "%d\x00", len(raw))
		hash.Write(raw)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func marshalDocument(value any) ([]byte, error) {
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(out.String()), nil
}

func executableProvenance(executable string, build BuildIdentity) (jobcontract.Provenance, []byte, error) {
	return executableProvenanceContext(context.Background(), executable, build)
}

func executableProvenanceContext(ctx context.Context, executable string, build BuildIdentity) (jobcontract.Provenance, []byte, error) {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return jobcontract.Provenance{}, nil, err
		}
	}
	raw, err := readBoundRegular(ctx, executable, 1<<30)
	if err != nil {
		return jobcontract.Provenance{}, nil, fmt.Errorf("read executing file: %w", err)
	}
	provenance := currentProvenance(build, digest(raw))
	encoded, err := marshalDocument(provenance)
	if err != nil {
		return provenance, nil, err
	}
	if _, err := jobcontract.ParseProvenance(encoded); err != nil {
		return provenance, nil, err
	}
	return provenance, encoded, nil
}

// Provenance reports the exact executable and build identity used by the
// governed-job producer. The returned bytes are the canonical retained form.
func Provenance(executable string, build BuildIdentity) (jobcontract.Provenance, []byte, error) {
	return executableProvenance(executable, build)
}

func planContentDigest(raw []byte) (plan.Result, string, error) {
	if err := jobcontract.ValidateJSONDocument(raw, jobcontract.MaximumManifestBytes); err != nil {
		return plan.Result{}, "", err
	}
	var result plan.Result
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return plan.Result{}, "", fmt.Errorf("decode retained plan: %w", err)
	}
	_, evidenceDigest, err := plan.EvidenceCanonical(result.Plan)
	if err != nil {
		return plan.Result{}, "", err
	}
	return result, prefixedDigest(evidenceDigest), nil
}

// ReadRequestFile descriptor-binds and bounds the operator-supplied job request
// before it enters semantic parsing.
func ReadRequestFile(ctx context.Context, path string) ([]byte, error) {
	return readBoundRegular(ctx, path, jobcontract.MaximumRequestBytes)
}

func readBoundRegular(ctx context.Context, path string, maximum int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > int64(maximum) {
		return nil, fmt.Errorf("%s is not a bounded regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed during open", path)
	}
	raw, err := io.ReadAll(&contextReader{ctx: ctx, reader: io.LimitReader(file, int64(maximum)+1)})
	if err != nil {
		return nil, err
	}
	if len(raw) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("%s changed during read", path)
	}
	return raw, nil
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

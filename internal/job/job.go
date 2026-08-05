// Package job implements provider-independent governed-job execution and
// offline evidence verification.
package job

import (
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
	Identity() RuntimeIdentity
	Wait(context.Context) (jobcontract.TargetResult, error)
	Finalize(context.Context) (RuntimeFinalization, error)
}

type Invocation struct {
	Request  jobcontract.Request
	Prepared app.Prepared
}

type RuntimeIdentity struct {
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

func (e Executor) Run(ctx context.Context, requestRaw []byte, evidenceDir string) (Outcome, error) {
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
	declarationRaw, err := readBoundRegular(request.Declaration.Path, jobcontract.MaximumRequestBytes)
	if err != nil {
		return Outcome{}, fmt.Errorf("read bound declaration: %w", err)
	}
	if digest(declarationRaw) != request.Declaration.SHA256 {
		return Outcome{}, errors.New("declaration digest does not match job request")
	}
	prepared, err := app.PrepareBytes(declarationRaw, request.Declaration.Path)
	if err != nil {
		return Outcome{}, fmt.Errorf("prepare bound declaration: %w", err)
	}
	planRaw, err := plan.JSON(prepared.Result)
	if err != nil {
		return Outcome{}, err
	}
	provenance, provenanceRaw, err := executableProvenance(e.Executable, e.Build)
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

	invocation := Invocation{Request: request, Prepared: prepared}
	result := jobcontract.Result{
		Schema: jobcontract.ResultSchema, JobID: request.JobID, Status: "refused",
		RequestSHA256: digest(requestRaw), EvidenceManifest: "manifest.json",
		Identity: jobcontract.ExecutionIdentity{
			DeclarationSHA256: request.Declaration.SHA256,
			PlanSHA256:        prefixedDigest(prepared.Result.PlanDigest),
			ImageReference:    prepared.Result.Plan.World.Base,
			ProvenanceSHA256:  digest(provenanceRaw),
		},
		Target:  jobcontract.TargetResult{Kind: "not_started"},
		Reasons: []string{"RUNTIME_START_FAILED"},
	}

	process, startErr := e.Runtime.Start(ctx, invocation, stdout, stderr)
	admitted := process != nil
	if startErr == nil && process == nil {
		startErr = errors.New("runtime returned no process observation")
	}
	var runtimeBefore, runtimeAfter []byte
	artifactInventoryWritten := false
	if startErr == nil {
		identity := process.Identity()
		result.Identity.Generation = identity.Generation
		result.Identity.ImageReference = identity.ImageReference
		result.Identity.ImageDigest = identity.ImageDigest
		runtimeBefore = identity.Before
		if err := jobcontract.ValidateJSONDocument(runtimeBefore, jobcontract.MaximumManifestBytes); err != nil {
			startErr = fmt.Errorf("runtime before evidence is invalid: %w", err)
		}
	}
	if startErr != nil && admitted {
		result.Status = "incomplete"
		result.Target = jobcontract.TargetResult{Kind: "unknown"}
		result.Reasons = []string{"RUNTIME_START_OBSERVATION_FAILED"}
	}
	if startErr == nil {
		targetCtx, cancel := context.WithTimeout(ctx, time.Duration(request.Limits.TimeoutNS))
		result.Target, err = process.Wait(targetCtx)
		cancel()
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
		final, finalErr := process.Finalize(finalCtx)
		cancelFinalize()
		finalFinished := e.Now().UTC()
		result.Finalization = interval(finalStarted, finalFinished, time.Since(finalMonotonic))
		runtimeAfter = final.After
		if runtimeErr := jobcontract.ValidateJSONDocument(runtimeAfter, jobcontract.MaximumManifestBytes); runtimeErr != nil {
			finalErr = errors.Join(finalErr, runtimeErr)
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
			inventory, artifactErr := evidence.WriteArtifacts(*request.Artifacts, artifacts)
			if artifactErr != nil {
				return Outcome{}, fmt.Errorf("publish target artifacts: %w", artifactErr)
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
	} else {
		now := e.Now().UTC()
		result.Finalization = interval(now, now, 0)
	}
	if request.Artifacts != nil && !artifactInventoryWritten {
		inventory, artifactErr := evidence.WriteArtifacts(*request.Artifacts, nil)
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
	result.Cleanup = e.Runtime.Cleanup(cleanupCtx, invocation)
	cancelCleanup()
	result.Cleanup.DurationNS = boundedDuration(time.Since(cleanupStarted))
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
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return jobcontract.Provenance{}, nil, err
		}
	}
	raw, err := readBoundRegular(executable, 1<<30)
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

func planContentDigest(raw []byte) (string, error) {
	if err := jobcontract.ValidateJSONDocument(raw, jobcontract.MaximumManifestBytes); err != nil {
		return "", err
	}
	var result plan.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	canonical, err := plan.Canonical(result.Plan)
	if err != nil {
		return "", err
	}
	return digest(canonical), nil
}

func readBoundRegular(path string, maximum int) ([]byte, error) {
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
	raw, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
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

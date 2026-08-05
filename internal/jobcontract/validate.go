package jobcontract

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maximumJobIDBytes       = 63
	maximumPathBytes        = 4096
	maximumArgumentBytes    = 4096
	maximumArguments        = 128
	maximumEnvironmentItems = 128
	maximumEnvironmentValue = 16 << 10
	maximumOutputBytes      = int64(64 << 20)
	maximumArtifactEntries  = int64(10_000)
	maximumArtifactBytes    = int64(1 << 30)
	maximumManifestEntries  = 10_032
	minimumTimeoutNS        = int64(time.Millisecond)
	maximumTimeoutNS        = int64(24 * time.Hour)
	maximumFinalizeNS       = int64(10 * time.Minute)
)

var (
	portableIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	environmentName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reasonPattern     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	platformPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	versionPattern    = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

func ValidateRequest(value Request) error {
	if value.Schema != RequestSchema {
		return errors.New("job request schema is invalid")
	}
	if !validID(value.JobID) {
		return errors.New("job_id is not a bounded portable identifier")
	}
	if !validOperatorPath(value.Declaration.Path) || !validDigest(value.Declaration.SHA256) {
		return errors.New("declaration binding is invalid")
	}
	if len(value.Command.Argv) == 0 || len(value.Command.Argv) > maximumArguments {
		return errors.New("command argv is empty or exceeds its bound")
	}
	for _, argument := range value.Command.Argv {
		if !validOpaqueText(argument, 1, maximumArgumentBytes) {
			return errors.New("command argv contains an empty, oversized, invalid, or NUL-bearing argument")
		}
	}
	if !validOperatorPath(value.Command.WorkingDirectory) {
		return errors.New("command working_directory is invalid")
	}
	if len(value.Command.Environment) > maximumEnvironmentItems {
		return errors.New("command environment exceeds its bound")
	}
	seenEnvironment := map[string]struct{}{}
	for _, item := range value.Command.Environment {
		public := item.PublicValue != nil
		secret := item.SecretFile != ""
		if !environmentName.MatchString(item.Name) || public == secret ||
			(public && !validOpaqueText(*item.PublicValue, 0, maximumEnvironmentValue)) ||
			(secret && !validOperatorPath(item.SecretFile)) {
			return errors.New("command environment entry is invalid")
		}
		if _, exists := seenEnvironment[item.Name]; exists {
			return fmt.Errorf("command environment contains duplicate name %q", item.Name)
		}
		seenEnvironment[item.Name] = struct{}{}
	}
	if value.Limits.TimeoutNS < minimumTimeoutNS || value.Limits.TimeoutNS > maximumTimeoutNS ||
		value.Limits.FinalizeNS < minimumTimeoutNS || value.Limits.FinalizeNS > maximumFinalizeNS ||
		value.Limits.StdoutMaxBytes < 0 || value.Limits.StdoutMaxBytes > maximumOutputBytes ||
		value.Limits.StderrMaxBytes < 0 || value.Limits.StderrMaxBytes > maximumOutputBytes {
		return errors.New("job limits are outside the normative bounds")
	}
	if value.Artifacts != nil && (!validOperatorPath(value.Artifacts.ContainerRoot) ||
		value.Artifacts.MaxEntries < 1 || value.Artifacts.MaxEntries > maximumArtifactEntries ||
		value.Artifacts.MaxBytes < 1 || value.Artifacts.MaxBytes > maximumArtifactBytes) {
		return errors.New("artifact request is outside the normative bounds")
	}
	return nil
}

func ValidateResult(value Result) error {
	if value.Schema != ResultSchema || !validID(value.JobID) || !validDigest(value.RequestSHA256) || value.EvidenceManifest != "manifest.json" {
		return errors.New("job result envelope is invalid")
	}
	if !slices.Contains([]string{"complete", "incomplete", "refused"}, value.Status) {
		return errors.New("job result status is invalid")
	}
	if err := validateExecutionIdentity(value.Identity, value.Status == "complete"); err != nil {
		return err
	}
	if err := validateTarget(value.Target); err != nil {
		return err
	}
	if err := validateStream(value.Stdout, "stdout.bin"); err != nil {
		return fmt.Errorf("stdout: %w", err)
	}
	if err := validateStream(value.Stderr, "stderr.bin"); err != nil {
		return fmt.Errorf("stderr: %w", err)
	}
	if err := validateInterval(value.Finalization.StartedAt, value.Finalization.FinishedAt, value.Finalization.DurationNS); err != nil {
		return fmt.Errorf("finalization: %w", err)
	}
	if err := validateCleanup(value.Cleanup); err != nil {
		return err
	}
	if !validReasons(value.Reasons) {
		return errors.New("job result reasons are invalid")
	}
	switch value.Status {
	case "complete":
		if len(value.Reasons) != 0 || value.Target.Kind == "not_started" || value.Target.Kind == "unknown" || value.Stdout.Truncated || value.Stderr.Truncated || value.Cleanup.Status != "complete" {
			return errors.New("complete job result carries incomplete evidence")
		}
	case "incomplete":
		if len(value.Reasons) == 0 {
			return errors.New("incomplete job result lacks a reason")
		}
	case "refused":
		if len(value.Reasons) == 0 || value.Target.Kind != "not_started" || value.Cleanup.Status != "complete" {
			return errors.New("refused job result has inconsistent target or cleanup evidence")
		}
	}
	return nil
}

func validateExecutionIdentity(value ExecutionIdentity, complete bool) error {
	if !validDigest(value.DeclarationSHA256) || !validDigest(value.PlanSHA256) ||
		value.Generation < 0 || value.Generation > MaximumWireInteger ||
		!validOpaqueText(value.ImageReference, 1, maximumPathBytes) ||
		(value.ImageDigest != "" && !validDigest(value.ImageDigest)) ||
		(value.RuntimeSHA256 != "" && !validDigest(value.RuntimeSHA256)) ||
		!validDigest(value.ProvenanceSHA256) {
		return errors.New("execution identity is invalid")
	}
	if value.RuntimeProvider != "" && value.RuntimeProvider != "podman-cli" {
		return errors.New("execution runtime provider is invalid")
	}
	if complete && (value.Generation < 1 || value.ImageDigest == "" || value.RuntimeSHA256 == "" || value.RuntimeProvider != "podman-cli") {
		return errors.New("complete result lacks an observed execution identity")
	}
	return nil
}

func validateTarget(value TargetResult) error {
	if !slices.Contains([]string{"exited", "signaled", "not_started", "unknown"}, value.Kind) {
		return errors.New("target result kind is invalid")
	}
	switch value.Kind {
	case "exited":
		if value.ExitStatus == nil || *value.ExitStatus < 0 || *value.ExitStatus > 255 || value.Signal != nil {
			return errors.New("exited target result has invalid status or signal")
		}
	case "signaled":
		if value.Signal == nil || *value.Signal < 1 || *value.Signal > 64 || value.ExitStatus != nil {
			return errors.New("signaled target result has invalid signal or status")
		}
	case "not_started", "unknown":
		if value.ExitStatus != nil || value.Signal != nil || value.StartedAt != "" || value.FinishedAt != "" || value.DurationNS != nil {
			return fmt.Errorf("%s target result invents lifecycle evidence", value.Kind)
		}
		return nil
	}
	if value.DurationNS == nil {
		return errors.New("observed target result lacks a duration")
	}
	return validateInterval(value.StartedAt, value.FinishedAt, *value.DurationNS)
}

// ValidateTargetResult validates a runtime observation before a producer
// incorporates it into a signed-off result envelope.
func ValidateTargetResult(value TargetResult) error { return validateTarget(value) }

func validateStream(value StreamResult, expectedPath string) error {
	if value.Path != expectedPath || !validDigest(value.SHA256) || value.CapturedBytes < 0 || value.TotalBytes < 0 ||
		value.CapturedBytes > value.TotalBytes || value.TotalBytes > MaximumWireInteger ||
		(value.Truncated != (value.CapturedBytes < value.TotalBytes)) {
		return errors.New("stream evidence is invalid")
	}
	return nil
}

func validateCleanup(value CleanupResult) error {
	if !slices.Contains([]string{"complete", "incomplete"}, value.Status) || value.DurationNS < 0 || value.DurationNS > MaximumWireInteger || !validReasons(value.Reasons) {
		return errors.New("cleanup evidence is invalid")
	}
	if value.Status == "complete" {
		if !value.ContainerAbsent || !value.ProxyAbsent || !value.ProcessGroupEmpty || len(value.Reasons) != 0 {
			return errors.New("complete cleanup does not prove owned resources absent")
		}
	} else if len(value.Reasons) == 0 {
		return errors.New("incomplete cleanup lacks a reason")
	}
	return nil
}

// ValidateCleanupResult validates cleanup proof without trusting the provider
// that reported it.
func ValidateCleanupResult(value CleanupResult) error { return validateCleanup(value) }

func ValidateManifest(value Manifest) error {
	if value.Schema != ManifestSchema || !validID(value.JobID) || !validDigest(value.RequestSHA256) ||
		!validDigest(value.ResultSHA256) || !validDigest(value.ContentSHA256) || !validTimestamp(value.SealedAt) ||
		len(value.Entries) == 0 || len(value.Entries) > maximumManifestEntries {
		return errors.New("job evidence manifest envelope is invalid")
	}
	seenRequired := map[string]bool{}
	prior := ""
	for _, entry := range value.Entries {
		if !validEvidencePath(entry.Path) || entry.Path <= prior ||
			!slices.Contains([]string{"request", "declaration", "plan", "provenance", "runtime", "stdout", "stderr", "result", "target_inventory", "target_artifact"}, entry.Kind) ||
			entry.Size < 0 || entry.Size > MaximumWireInteger || !validDigest(entry.SHA256) {
			return errors.New("job evidence manifest entry is invalid, duplicated, or unordered")
		}
		prior = entry.Path
		seenRequired[entry.Path] = true
		if entry.Path == "request.json" && entry.SHA256 != value.RequestSHA256 {
			return errors.New("manifest request digest disagrees with request entry")
		}
		if entry.Path == "result.json" && entry.SHA256 != value.ResultSHA256 {
			return errors.New("manifest result digest disagrees with result entry")
		}
	}
	for _, required := range []string{"declaration.toml", "plan.json", "provenance.json", "request.json", "result.json", "runtime-after.json", "runtime-before.json", "stderr.bin", "stdout.bin"} {
		if !seenRequired[required] {
			return fmt.Errorf("job evidence manifest omits %s", required)
		}
	}
	expectedKinds := map[string]string{
		"declaration.toml": "declaration", "plan.json": "plan", "provenance.json": "provenance",
		"request.json": "request", "result.json": "result", "runtime-after.json": "runtime",
		"runtime-before.json": "runtime", "stderr.bin": "stderr", "stdout.bin": "stdout",
		"target-inventory.json": "target_inventory",
	}
	for _, entry := range value.Entries {
		if expected, fixed := expectedKinds[entry.Path]; fixed && entry.Kind != expected {
			return fmt.Errorf("manifest entry %s has kind %s, want %s", entry.Path, entry.Kind, expected)
		}
	}
	return nil
}

func ValidateProvenance(value Provenance) error {
	if value.Schema != ProvenanceSchema || !slices.Contains([]string{"development", "release"}, value.BuildKind) ||
		!validOpaqueText(value.Version, 1, 128) || !validOpaqueText(value.GoVersion, 1, 128) ||
		!strings.HasPrefix(value.GoVersion, "go1.") || !platformPattern.MatchString(value.GOOS) ||
		!platformPattern.MatchString(value.GOARCH) || !validDigest(value.ExecutableSHA256) {
		return errors.New("executable provenance envelope is invalid")
	}
	switch value.BuildKind {
	case "release":
		if !validVersion(value.Version) || !commitPattern.MatchString(value.Commit) || !validTimestamp(value.SourceDate) {
			return errors.New("release provenance contains placeholder or malformed source identity")
		}
	case "development":
		if value.Version != "dev" && !validVersion(value.Version) {
			return errors.New("development provenance version is invalid")
		}
		if value.Commit != "unknown" && !commitPattern.MatchString(value.Commit) {
			return errors.New("development provenance commit is invalid")
		}
		if value.SourceDate != "unknown" && !validTimestamp(value.SourceDate) {
			return errors.New("development provenance source date is invalid")
		}
	}
	return nil
}

func validID(value string) bool {
	return len(value) <= maximumJobIDBytes && portableIDPattern.MatchString(value)
}
func validDigest(value string) bool { return digestPattern.MatchString(value) }

func validOpaqueText(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validOperatorPath(value string) bool {
	if !validContainerPath(value) {
		return false
	}
	return !hasDisplayControls(value)
}

func validContainerPath(value string) bool {
	return len(value) > 1 && len(value) <= maximumPathBytes && strings.HasPrefix(value, "/") && path.Clean(value) == value && validOpaqueText(value, 1, maximumPathBytes)
}

func validEvidencePath(value string) bool {
	return validOpaqueText(value, 1, maximumPathBytes) && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "." &&
		value != "manifest.json" && !strings.HasPrefix(value, "../") && !strings.Contains(value, "/../") && !hasDisplayControls(value)
}

// ValidateEvidenceRelativePath applies the normative retained-path boundary to
// provider-supplied target artifact names.
func ValidateEvidenceRelativePath(value string) error {
	if !validEvidencePath(value) {
		return errors.New("evidence-relative path is invalid")
	}
	return nil
}

func hasDisplayControls(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

func validTimestamp(value string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Format(time.RFC3339Nano) == value && strings.HasSuffix(value, "Z")
}

func validateInterval(startRaw, finishRaw string, duration int64) error {
	if duration < 0 || duration > MaximumWireInteger || !validTimestamp(startRaw) || !validTimestamp(finishRaw) {
		return errors.New("interval is malformed or outside the wire bound")
	}
	start, _ := time.Parse(time.RFC3339Nano, startRaw)
	finish, _ := time.Parse(time.RFC3339Nano, finishRaw)
	if finish.Before(start) {
		return errors.New("interval finishes before it starts")
	}
	return nil
}

func validReasons(values []string) bool {
	if len(values) > 64 {
		return false
	}
	seen := map[string]struct{}{}
	for _, value := range values {
		if !reasonPattern.MatchString(value) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validVersion(value string) bool {
	if !versionPattern.MatchString(value) {
		return false
	}
	prereleaseAt := strings.IndexByte(value, '-')
	if prereleaseAt < 0 {
		return true
	}
	for _, identifier := range strings.Split(value[prereleaseAt+1:], ".") {
		allDigits := true
		for _, character := range identifier {
			if character < '0' || character > '9' {
				allDigits = false
				break
			}
		}
		if allDigits && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

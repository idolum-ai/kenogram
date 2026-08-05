package job

import (
	"runtime"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

func currentProvenance(build BuildIdentity, executableDigest string) jobcontract.Provenance {
	kind := "release"
	if build.Version == "" || build.Version == "dev" || build.Commit == "" || build.Commit == "unknown" || build.SourceDate == "" || build.SourceDate == "unknown" {
		kind = "development"
	}
	version, commit, sourceDate := build.Version, build.Commit, build.SourceDate
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	if sourceDate == "" {
		sourceDate = "unknown"
	}
	return jobcontract.Provenance{
		Schema: jobcontract.ProvenanceSchema, BuildKind: kind, Version: version,
		Commit: commit, SourceDate: sourceDate, GoVersion: runtime.Version(),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ExecutableSHA256: executableDigest,
	}
}

// Package jobcontract defines Kenogram's language-neutral governed-job handoff.
// It is deliberately pure: execution, runtime observation, and durable state
// packages may depend on this package, but this package may not depend on them.
package jobcontract

const (
	RequestSchema            = "kenogram.job-request.v1"
	ResultSchema             = "kenogram.job-result.v1"
	ManifestSchema           = "kenogram.job-evidence-manifest.v1"
	ProvenanceSchema         = "kenogram.executable-provenance.v1"
	RuntimeObservationSchema = "kenogram.podman-runtime-observation.v1"

	MaximumRequestBytes    = 1 << 20
	MaximumResultBytes     = 1 << 20
	MaximumManifestBytes   = 8 << 20
	MaximumProvenanceBytes = 64 << 10
	MaximumWireInteger     = int64(9_007_199_254_740_991)
	MaxRuntimeMounts       = 512
)

type Request struct {
	Schema      string             `json:"schema"`
	JobID       string             `json:"job_id"`
	Declaration DeclarationBinding `json:"declaration"`
	Command     Command            `json:"command"`
	Limits      Limits             `json:"limits"`
	Artifacts   *ArtifactRequest   `json:"artifacts,omitempty"`
}

type DeclarationBinding struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Command struct {
	Argv             []string          `json:"argv"`
	WorkingDirectory string            `json:"working_directory"`
	Environment      []EnvironmentItem `json:"environment"`
}

// EnvironmentItem is exactly one retained public value or one in-world secret
// file reference. Secret bytes are never duplicated into the request.
type EnvironmentItem struct {
	Name        string  `json:"name"`
	PublicValue *string `json:"public_value,omitempty"`
	SecretFile  string  `json:"secret_file,omitempty"`
}

type Limits struct {
	TimeoutNS      int64 `json:"timeout_ns"`
	FinalizeNS     int64 `json:"finalize_timeout_ns"`
	StdoutMaxBytes int64 `json:"stdout_max_bytes"`
	StderrMaxBytes int64 `json:"stderr_max_bytes"`
}

type ArtifactRequest struct {
	ContainerRoot string `json:"container_root"`
	MaxEntries    int64  `json:"max_entries"`
	MaxBytes      int64  `json:"max_bytes"`
}

type Result struct {
	Schema           string             `json:"schema"`
	JobID            string             `json:"job_id"`
	Status           string             `json:"status"`
	RequestSHA256    string             `json:"request_sha256"`
	EvidenceManifest string             `json:"evidence_manifest"`
	Identity         ExecutionIdentity  `json:"identity"`
	Target           TargetResult       `json:"target"`
	Stdout           StreamResult       `json:"stdout"`
	Stderr           StreamResult       `json:"stderr"`
	Finalization     FinalizationResult `json:"finalization"`
	Cleanup          CleanupResult      `json:"cleanup"`
	Reasons          []string           `json:"reasons"`
}

type ExecutionIdentity struct {
	DeclarationSHA256 string `json:"declaration_sha256"`
	PlanSHA256        string `json:"plan_sha256"`
	Generation        int64  `json:"generation"`
	ImageReference    string `json:"image_reference"`
	ImageDigest       string `json:"image_digest"`
	RuntimeSHA256     string `json:"runtime_evidence_sha256"`
	ProvenanceSHA256  string `json:"provenance_sha256"`
	RuntimeProvider   string `json:"runtime_provider"`
}

type TargetResult struct {
	Kind       string `json:"kind"`
	ExitStatus *int64 `json:"exit_status,omitempty"`
	Signal     *int64 `json:"signal,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationNS *int64 `json:"duration_ns,omitempty"`
}

type StreamResult struct {
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	CapturedBytes int64  `json:"captured_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Truncated     bool   `json:"truncated"`
}

type FinalizationResult struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationNS int64  `json:"duration_ns"`
}

type CleanupResult struct {
	Status            string   `json:"status"`
	ContainerAbsent   bool     `json:"container_absent"`
	ProxyAbsent       bool     `json:"proxy_absent"`
	ProcessGroupEmpty bool     `json:"process_group_empty"`
	Forced            bool     `json:"forced"`
	DurationNS        int64    `json:"duration_ns"`
	Reasons           []string `json:"reasons"`
}

type Manifest struct {
	Schema        string          `json:"schema"`
	JobID         string          `json:"job_id"`
	RequestSHA256 string          `json:"request_sha256"`
	ResultSHA256  string          `json:"result_sha256"`
	ContentSHA256 string          `json:"content_sha256"`
	SealedAt      string          `json:"sealed_at"`
	Entries       []ManifestEntry `json:"entries"`
}

type ManifestEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Provenance struct {
	Schema           string `json:"schema"`
	BuildKind        string `json:"build_kind"`
	Version          string `json:"version"`
	Commit           string `json:"commit"`
	SourceDate       string `json:"source_date"`
	GoVersion        string `json:"go_version"`
	GOOS             string `json:"goos"`
	GOARCH           string `json:"goarch"`
	ExecutableSHA256 string `json:"executable_sha256"`
}

// RuntimeObservation is the closed, provider-specific public proof retained
// around a direct governed job. It intentionally contains only public
// authority and observed enforcement facts; capability material never enters
// this document.
type RuntimeObservation struct {
	Schema            string                    `json:"schema"`
	Phase             string                    `json:"phase"`
	ObservedAt        string                    `json:"observed_at"`
	Provider          string                    `json:"provider"`
	ContainerID       string                    `json:"container_id"`
	ContainerName     string                    `json:"container_name"`
	Running           bool                      `json:"running"`
	ImageReference    string                    `json:"image_reference"`
	ImageDigest       string                    `json:"image_digest"`
	PlanSHA256        string                    `json:"plan_sha256"`
	DeclarationSHA256 string                    `json:"declaration_sha256"`
	Generation        int64                     `json:"generation"`
	NetworkMode       string                    `json:"network_mode"`
	IPCMode           string                    `json:"ipc_mode"`
	IPCIsolated       bool                      `json:"ipc_isolated"`
	PIDMode           string                    `json:"pid_mode"`
	UTSMode           string                    `json:"uts_mode"`
	UserNSMode        string                    `json:"userns_mode"`
	User              string                    `json:"user"`
	Hostname          string                    `json:"hostname"`
	WorkingDirectory  string                    `json:"working_directory"`
	BoundingCaps      []string                  `json:"bounding_caps"`
	NoNewPrivileges   bool                      `json:"no_new_privileges"`
	SeccompMode       int64                     `json:"seccomp_mode"`
	Devices           int64                     `json:"devices"`
	UIDIdentity       bool                      `json:"uid_identity"`
	GIDIdentity       bool                      `json:"gid_identity"`
	MemoryBytes       int64                     `json:"memory_bytes"`
	NanoCPUs          int64                     `json:"nano_cpus"`
	PIDs              int64                     `json:"pids"`
	Mounts            []RuntimeMountObservation `json:"mounts"`
}

type RuntimeMountObservation struct {
	Role             string `json:"role"`
	AuthoritySource  string `json:"authority_source,omitempty"`
	AuthoritySHA256  string `json:"authority_sha256,omitempty"`
	PermissionPolicy string `json:"permission_policy,omitempty"`
	Source           string `json:"source"`
	Target           string `json:"target"`
	Mode             string `json:"mode"`
	Device           uint64 `json:"device"`
	Inode            uint64 `json:"inode"`
	FileType         string `json:"file_type"`
	SHA256           string `json:"sha256,omitempty"`
	IdentityVerified bool   `json:"identity_verified"`
}

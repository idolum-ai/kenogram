// Package plan resolves declarations into deterministic semantic plans.
package plan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/idolum-ai/kenogram/internal/decl"
	"github.com/idolum-ai/kenogram/internal/mountpath"
	"github.com/idolum-ai/kenogram/internal/sourcetree"
)

// Plan is the fully resolved, canonical provisioning intent at M1.
type Plan struct {
	Version       int64          `json:"version"`
	Name          string         `json:"name"`
	AllowUnpinned bool           `json:"allow_unpinned"`
	World         World          `json:"world"`
	Resources     Resources      `json:"resources"`
	Workspace     []string       `json:"workspace_paths"`
	Copies        []Copy         `json:"copies"`
	Mounts        []Mount        `json:"mounts"`
	NetworkAllow  []NetworkAllow `json:"network_allow"`
	Interfaces    []Interface    `json:"interfaces,omitempty"`
	Services      []Service      `json:"services"`
}

type World struct {
	Hostname string `json:"hostname"`
	Base     string `json:"base"`
	Workdir  string `json:"workdir"`
	User     string `json:"user"`
}
type Resources struct {
	CPUs        int64 `json:"cpus"`
	MemoryBytes int64 `json:"memory_bytes"`
	PIDs        int64 `json:"pids"`
}
type Copy struct {
	Source       string `json:"source"`
	SourceDigest string `json:"source_digest"`
	Target       string `json:"target"`
	Mode         string `json:"mode"`
	Secret       bool   `json:"secret"`
}
type Mount struct {
	Source     string `json:"source"`
	SourceType string `json:"source_type"`
	Target     string `json:"target"`
	Mode       string `json:"mode"`
}
type NetworkAllow struct {
	Host string `json:"host"`
	Port int64  `json:"port"`
}
type Interface struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}
type Service struct {
	Name      string   `json:"name"`
	Command   []string `json:"command"`
	Autostart bool     `json:"autostart"`
	Restart   string   `json:"restart"`
}

// Result carries semantic intent and both required provenance digests.
type Result struct {
	PlanDigest        string   `json:"plan_digest"`
	EvidenceDigest    string   `json:"evidence_digest"`
	DeclarationDigest string   `json:"declaration_digest"`
	Warnings          []string `json:"warnings"`
	Plan              Plan     `json:"plan"`
}

func (r Result) MarshalJSON() ([]byte, error) {
	type wire Result
	safe := r
	redacted, digest, err := EvidenceCanonical(r.Plan)
	if err != nil {
		return nil, err
	}
	safe.Plan = redacted
	safe.EvidenceDigest = digest
	return json.Marshal(wire(safe))
}

// Build validates and resolves a declaration relative to its file location.
func Build(d decl.Declaration, declarationPath string, declarationBytes []byte) (Result, error) {
	return BuildContext(context.Background(), d, declarationPath, declarationBytes)
}

// BuildContext is Build with cancellation threaded through bounded source-tree
// digest work.
func BuildContext(ctx context.Context, d decl.Declaration, declarationPath string, declarationBytes []byte) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	dir, err := filepath.Abs(filepath.Dir(declarationPath))
	if err != nil {
		return Result{}, fmt.Errorf("resolve declaration directory: %w", err)
	}
	if err := decl.ValidateContext(ctx, d, dir); err != nil {
		return Result{}, err
	}
	p := Plan{
		Version: d.Version, Name: d.Name, AllowUnpinned: d.AllowUnpinned,
		World:     World{Hostname: d.World.Hostname, Base: d.World.Base, Workdir: filepath.Clean(d.World.Workdir), User: d.World.User},
		Resources: Resources{CPUs: d.Resources.CPUs, MemoryBytes: d.Resources.MemoryBytes, PIDs: d.Resources.PIDs},
		Workspace: append([]string{}, d.Workspace.Paths...),
		Copies:    make([]Copy, 0, len(d.Copies)), Mounts: make([]Mount, 0, len(d.Mounts)),
		NetworkAllow: make([]NetworkAllow, 0, len(d.Network.Allow)), Interfaces: make([]Interface, 0, len(d.Interfaces)), Services: make([]Service, 0, len(d.Services)),
	}
	for _, target := range p.Workspace {
		if err := mountpath.Validate(target); err != nil {
			return Result{}, fmt.Errorf("workspace target %s: %w", target, err)
		}
	}
	for _, c := range d.Copies {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		source, err := decl.ResolveSource(dir, c.Source)
		if err != nil {
			return Result{}, err
		}
		digest, err := DigestSourceContext(ctx, source)
		if err != nil {
			return Result{}, fmt.Errorf("digest copy source %s: %w", c.Source, err)
		}
		p.Copies = append(p.Copies, Copy{Source: source, SourceDigest: digest, Target: filepath.Clean(c.Target), Mode: c.Mode, Secret: c.Secret})
	}
	for _, m := range d.Mounts {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		source, err := decl.ResolveSource(dir, m.Source)
		if err != nil {
			return Result{}, err
		}
		sourceType, err := mountSourceType(source)
		if err != nil {
			return Result{}, fmt.Errorf("inspect mount source %s: %w", m.Source, err)
		}
		target := filepath.Clean(m.Target)
		if err := mountpath.Validate(source); err != nil {
			return Result{}, fmt.Errorf("mount source %s: %w", m.Source, err)
		}
		if err := mountpath.Validate(target); err != nil {
			return Result{}, fmt.Errorf("mount target %s: %w", m.Target, err)
		}
		p.Mounts = append(p.Mounts, Mount{Source: source, SourceType: sourceType, Target: target, Mode: m.Mode})
	}
	for _, a := range d.Network.Allow {
		p.NetworkAllow = append(p.NetworkAllow, NetworkAllow{Host: a.Host, Port: a.Port})
	}
	for _, endpoint := range d.Interfaces {
		p.Interfaces = append(p.Interfaces, Interface{Name: endpoint.Name, Address: endpoint.Address})
	}
	for _, s := range d.Services {
		p.Services = append(p.Services, Service{Name: s.Name, Command: append([]string{}, s.Command...), Autostart: s.Autostart, Restart: s.Restart})
	}
	canonical, err := Canonical(p)
	if err != nil {
		return Result{}, err
	}
	planSum, declarationSum := sha256.Sum256(canonical), sha256.Sum256(declarationBytes)
	_, evidenceDigest, err := EvidenceCanonical(p)
	if err != nil {
		return Result{}, err
	}
	result := Result{PlanDigest: hex.EncodeToString(planSum[:]), EvidenceDigest: evidenceDigest, DeclarationDigest: hex.EncodeToString(declarationSum[:]), Warnings: []string{}, Plan: p}
	if !decl.ImagePinned(d.World.Base) {
		result.Warnings = append(result.Warnings, "UNPINNED BASE IMAGE: reproducibility depends on mutable external state")
	}
	return result, nil
}

// EvidenceCanonical returns the public, independently verifiable plan
// projection. Secret copy content digests remain operational commitments and
// never enter retained or operator-rendered evidence.
func EvidenceCanonical(p Plan) (Plan, string, error) {
	redacted := p
	redacted.Copies = append([]Copy{}, p.Copies...)
	for index := range redacted.Copies {
		if redacted.Copies[index].Secret {
			redacted.Copies[index].SourceDigest = "<redacted>"
		}
	}
	canonical, err := Canonical(redacted)
	if err != nil {
		return Plan{}, "", err
	}
	sum := sha256.Sum256(canonical)
	return redacted, hex.EncodeToString(sum[:]), nil
}

// ProjectEvidence strictly re-derives the retained public plan from the
// declaration. Non-secret source digests are retained content observations;
// secret digests must already be the literal redaction marker.
func ProjectEvidence(d decl.Declaration, declarationPath string, declarationBytes []byte, retained Plan) (Result, error) {
	dir, err := filepath.Abs(filepath.Dir(declarationPath))
	if err != nil {
		return Result{}, err
	}
	if err := decl.ValidateEvidence(d, dir); err != nil {
		return Result{}, err
	}
	if len(retained.Copies) != len(d.Copies) || len(retained.Mounts) != len(d.Mounts) {
		return Result{}, fmt.Errorf("retained plan copy or mount cardinality disagrees with declaration")
	}
	p := Plan{
		Version: d.Version, Name: d.Name, AllowUnpinned: d.AllowUnpinned,
		World:     World{Hostname: d.World.Hostname, Base: d.World.Base, Workdir: filepath.Clean(d.World.Workdir), User: d.World.User},
		Resources: Resources{CPUs: d.Resources.CPUs, MemoryBytes: d.Resources.MemoryBytes, PIDs: d.Resources.PIDs},
		Workspace: append([]string{}, d.Workspace.Paths...),
		Copies:    make([]Copy, 0, len(d.Copies)), Mounts: make([]Mount, 0, len(d.Mounts)),
		NetworkAllow: make([]NetworkAllow, 0, len(d.Network.Allow)), Services: make([]Service, 0, len(d.Services)),
	}
	for _, target := range p.Workspace {
		if err := mountpath.Validate(target); err != nil {
			return Result{}, fmt.Errorf("workspace target %s: %w", target, err)
		}
	}
	for index, copy := range d.Copies {
		source, err := decl.ResolveSource(dir, copy.Source)
		if err != nil {
			return Result{}, err
		}
		digest := retained.Copies[index].SourceDigest
		if copy.Secret {
			if digest != "<redacted>" {
				return Result{}, fmt.Errorf("retained secret copy %d is not redacted", index)
			}
		} else if !validPlanDigest(digest) {
			return Result{}, fmt.Errorf("retained copy %d digest is invalid", index)
		}
		p.Copies = append(p.Copies, Copy{Source: source, SourceDigest: digest, Target: filepath.Clean(copy.Target), Mode: copy.Mode, Secret: copy.Secret})
	}
	for index, mount := range d.Mounts {
		source, err := decl.ResolveSource(dir, mount.Source)
		if err != nil {
			return Result{}, err
		}
		sourceType := retained.Mounts[index].SourceType
		if sourceType != "file" && sourceType != "directory" {
			return Result{}, fmt.Errorf("retained mount %d source type is invalid", index)
		}
		target := filepath.Clean(mount.Target)
		if err := mountpath.Validate(source); err != nil {
			return Result{}, fmt.Errorf("mount source %s: %w", mount.Source, err)
		}
		if err := mountpath.Validate(target); err != nil {
			return Result{}, fmt.Errorf("mount target %s: %w", mount.Target, err)
		}
		p.Mounts = append(p.Mounts, Mount{Source: source, SourceType: sourceType, Target: target, Mode: mount.Mode})
	}
	for _, allow := range d.Network.Allow {
		p.NetworkAllow = append(p.NetworkAllow, NetworkAllow{Host: allow.Host, Port: allow.Port})
	}
	for _, endpoint := range d.Interfaces {
		p.Interfaces = append(p.Interfaces, Interface{Name: endpoint.Name, Address: endpoint.Address})
	}
	for _, service := range d.Services {
		p.Services = append(p.Services, Service{Name: service.Name, Command: append([]string{}, service.Command...), Autostart: service.Autostart, Restart: service.Restart})
	}
	_, evidenceDigest, err := EvidenceCanonical(p)
	if err != nil {
		return Result{}, err
	}
	declarationSum := sha256.Sum256(declarationBytes)
	result := Result{PlanDigest: evidenceDigest, EvidenceDigest: evidenceDigest, DeclarationDigest: hex.EncodeToString(declarationSum[:]), Warnings: []string{}, Plan: p}
	if !decl.ImagePinned(d.World.Base) {
		result.Warnings = append(result.Warnings, "UNPINNED BASE IMAGE: reproducibility depends on mutable external state")
	}
	return result, nil
}

func validPlanDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// DigestSource returns the canonical content and mode fingerprint used for a
// copied file or tree.
func DigestSource(root string) (string, error) {
	return DigestSourceContext(context.Background(), root)
}

// DigestSourceContext computes the canonical source digest under the shared
// source-tree resource and cancellation bounds.
func DigestSourceContext(ctx context.Context, root string) (string, error) {
	return sourcetree.Digest(ctx, root)
}

func mountSourceType(source string) (string, error) {
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
	return "", errors.New("mount source is not a regular file or directory")
}

// DigestRegularCopyBytes computes the canonical plan source digest for one
// regular file from exactly the bytes and mode observed through an already-open
// descriptor.
func DigestRegularCopyBytes(raw []byte, mode fs.FileMode) string {
	content := sha256.Sum256(raw)
	entry := "f\x00.\x00" + hex.EncodeToString(content[:]) + "\x00" + mode.Perm().String() + "\n"
	sum := sha256.Sum256([]byte(entry))
	return hex.EncodeToString(sum[:])
}

// Canonical returns the fixed-field JSON encoding used for the plan fingerprint.
func Canonical(p Plan) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(p); err != nil {
		return nil, fmt.Errorf("encode canonical plan: %w", err)
	}
	return out.Bytes(), nil
}

// JSON returns the stable machine-readable result.
func JSON(result Result) ([]byte, error) {
	_, evidenceDigest, err := EvidenceCanonical(result.Plan)
	if err != nil {
		return nil, err
	}
	result.PlanDigest = evidenceDigest
	result.EvidenceDigest = evidenceDigest
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return nil, fmt.Errorf("encode plan result: %w", err)
	}
	return out.Bytes(), nil
}

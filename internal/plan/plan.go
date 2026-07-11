// Package plan resolves declarations into deterministic semantic plans.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/idolum-ai/kenogram/internal/decl"
)

var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-fA-F]{64}$`)

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
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode"`
	Secret bool   `json:"secret"`
}
type Mount struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode"`
}
type NetworkAllow struct {
	Host string `json:"host"`
	Port int64  `json:"port"`
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
	DeclarationDigest string   `json:"declaration_digest"`
	Warnings          []string `json:"warnings"`
	Plan              Plan     `json:"plan"`
}

// Build validates and resolves a declaration relative to its file location.
func Build(d decl.Declaration, declarationPath string, declarationBytes []byte) (Result, error) {
	dir, err := filepath.Abs(filepath.Dir(declarationPath))
	if err != nil {
		return Result{}, fmt.Errorf("resolve declaration directory: %w", err)
	}
	if err := decl.Validate(d, dir); err != nil {
		return Result{}, err
	}
	p := Plan{
		Version: d.Version, Name: d.Name, AllowUnpinned: d.AllowUnpinned,
		World:     World{Hostname: d.World.Hostname, Base: d.World.Base, Workdir: filepath.Clean(d.World.Workdir), User: d.World.User},
		Resources: Resources{CPUs: d.Resources.CPUs, MemoryBytes: d.Resources.MemoryBytes, PIDs: d.Resources.PIDs},
		Workspace: append([]string{}, d.Workspace.Paths...),
		Copies:    make([]Copy, 0, len(d.Copies)), Mounts: make([]Mount, 0, len(d.Mounts)),
		NetworkAllow: make([]NetworkAllow, 0, len(d.Network.Allow)), Services: make([]Service, 0, len(d.Services)),
	}
	for _, c := range d.Copies {
		p.Copies = append(p.Copies, Copy{Source: resolve(dir, c.Source), Target: filepath.Clean(c.Target), Mode: c.Mode, Secret: c.Secret})
	}
	for _, m := range d.Mounts {
		p.Mounts = append(p.Mounts, Mount{Source: resolve(dir, m.Source), Target: filepath.Clean(m.Target), Mode: m.Mode})
	}
	for _, a := range d.Network.Allow {
		p.NetworkAllow = append(p.NetworkAllow, NetworkAllow{Host: a.Host, Port: a.Port})
	}
	for _, s := range d.Services {
		p.Services = append(p.Services, Service{Name: s.Name, Command: append([]string{}, s.Command...), Autostart: s.Autostart, Restart: s.Restart})
	}
	canonical, err := Canonical(p)
	if err != nil {
		return Result{}, err
	}
	planSum, declarationSum := sha256.Sum256(canonical), sha256.Sum256(declarationBytes)
	result := Result{PlanDigest: hex.EncodeToString(planSum[:]), DeclarationDigest: hex.EncodeToString(declarationSum[:]), Warnings: []string{}, Plan: p}
	if !pinnedImage.MatchString(d.World.Base) {
		result.Warnings = append(result.Warnings, "UNPINNED BASE IMAGE: reproducibility depends on mutable external state")
	}
	return result, nil
}

func resolve(dir, source string) string {
	if filepath.IsAbs(source) {
		return filepath.Clean(source)
	}
	return filepath.Clean(filepath.Join(dir, source))
}

// Canonical returns the fixed-field JSON encoding used for plan identity.
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
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return nil, fmt.Errorf("encode plan result: %w", err)
	}
	return out.Bytes(), nil
}

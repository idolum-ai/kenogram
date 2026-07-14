// Package plan resolves declarations into deterministic semantic plans.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/idolum-ai/kenogram/internal/decl"
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
	Interfaces    []Interface    `json:"interfaces"`
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
	Source string `json:"source"`
	Target string `json:"target"`
	Mode   string `json:"mode"`
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
	DeclarationDigest string   `json:"declaration_digest"`
	Warnings          []string `json:"warnings"`
	Plan              Plan     `json:"plan"`
}

func (r Result) MarshalJSON() ([]byte, error) {
	type wire Result
	safe := r
	safe.Plan.Copies = append([]Copy{}, r.Plan.Copies...)
	for i := range safe.Plan.Copies {
		if safe.Plan.Copies[i].Secret {
			safe.Plan.Copies[i].SourceDigest = "<redacted>"
		}
	}
	return json.Marshal(wire(safe))
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
		NetworkAllow: make([]NetworkAllow, 0, len(d.Network.Allow)), Interfaces: make([]Interface, 0, len(d.Interfaces)), Services: make([]Service, 0, len(d.Services)),
	}
	for _, c := range d.Copies {
		source := resolve(dir, c.Source)
		digest, err := DigestSource(source)
		if err != nil {
			return Result{}, fmt.Errorf("digest copy source %s: %w", c.Source, err)
		}
		p.Copies = append(p.Copies, Copy{Source: source, SourceDigest: digest, Target: filepath.Clean(c.Target), Mode: c.Mode, Secret: c.Secret})
	}
	for _, m := range d.Mounts {
		p.Mounts = append(p.Mounts, Mount{Source: resolve(dir, m.Source), Target: filepath.Clean(m.Target), Mode: m.Mode})
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
	result := Result{PlanDigest: hex.EncodeToString(planSum[:]), DeclarationDigest: hex.EncodeToString(declarationSum[:]), Warnings: []string{}, Plan: p}
	if !decl.ImagePinned(d.World.Base) {
		result.Warnings = append(result.Warnings, "UNPINNED BASE IMAGE: reproducibility depends on mutable external state")
	}
	return result, nil
}

// DigestSource returns the canonical content and mode fingerprint used for a
// copied file or tree.
func DigestSource(root string) (string, error) {
	entries := []string{}
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			entries = append(entries, "d\x00"+filepath.ToSlash(rel)+"\x00"+info.Mode().Perm().String())
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			entries = append(entries, "f\x00"+filepath.ToSlash(rel)+"\x00"+hex.EncodeToString(hash.Sum(nil))+"\x00"+info.Mode().Perm().String())
		default:
			return fmt.Errorf("unsupported source node %s", path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(entries)
	hash := sha256.New()
	for _, entry := range entries {
		io.WriteString(hash, entry)
		hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func resolve(dir, source string) string {
	if filepath.IsAbs(source) {
		return filepath.Clean(source)
	}
	return filepath.Clean(filepath.Join(dir, source))
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
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return nil, fmt.Errorf("encode plan result: %w", err)
	}
	return out.Bytes(), nil
}

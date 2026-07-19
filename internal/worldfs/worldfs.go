// Package worldfs owns Kenogram's durable host-side world layout.
package worldfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/idolum-ai/kenogram/internal/naming"
)

type Layout struct{ Root, Workspace, Digests, Staging, Applied, AppliedPlan, State, History, Transition, Lock, ProxySocket, ProxyPID string }

func BaseDir() (string, error) {
	if base := strings.TrimSpace(os.Getenv("KENOGRAM_STATE_DIR")); base != "" {
		return filepath.Clean(base), nil
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "kenogram", "worlds"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "kenogram", "worlds"), nil
}

func For(base, name string) Layout {
	root := filepath.Join(base, name)
	return Layout{Root: root, Workspace: filepath.Join(root, "workspace"), Digests: filepath.Join(root, "digests"), Staging: filepath.Join(root, "staging"), Applied: filepath.Join(root, "applied.toml"), AppliedPlan: filepath.Join(root, "applied-plan.json"), State: filepath.Join(root, "state.json"), History: filepath.Join(root, "history.jsonl"), Transition: filepath.Join(root, "transition.json"), Lock: filepath.Join(root, "mutation.lock"), ProxySocket: filepath.Join(root, "proxy.sock"), ProxyPID: filepath.Join(root, "proxy.pid")}
}

func ValidateName(name string) error {
	return naming.World(name)
}

func (l Layout) Ensure() error {
	for _, path := range []string{l.Root, l.Workspace, l.Digests, l.Staging} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create world directory %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(l.Root))
}

type State struct {
	Name              string `json:"name"`
	Generation        int64  `json:"generation"`
	Container         string `json:"container"`
	PlanDigest        string `json:"plan_digest"`
	DeclarationDigest string `json:"declaration_digest"`
	DeclarationPath   string `json:"declaration_path,omitempty"`
	Status            string `json:"status"`
	ProxyPID          int    `json:"proxy_pid,omitempty"`
}

// Transition is the durable reconciliation record for an interrupted up.
// Phase is either rollback (the prior state remains authoritative) or commit
// (the verified successor must be made authoritative).
type Transition struct {
	Version              int        `json:"version"`
	Phase                string     `json:"phase"`
	Prior                *State     `json:"prior,omitempty"`
	PriorWasRunning      bool       `json:"prior_was_running,omitempty"`
	PriorDeclaration     []byte     `json:"prior_declaration,omitempty"`
	PriorPlan            []byte     `json:"prior_plan,omitempty"`
	Successor            State      `json:"successor"`
	SuccessorDeclaration []byte     `json:"successor_declaration"`
	SuccessorPlan        []byte     `json:"successor_plan"`
	Workspace            DigestTree `json:"workspace,omitempty"`
	ImageDigests         []string   `json:"image_digests,omitempty"`
}

func (l Layout) ReadState() (State, error) {
	var s State
	raw, err := os.ReadFile(l.State)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("decode state: %w", err)
	}
	return s, nil
}
func (l Layout) WriteState(s State) error      { return atomicJSON(l.State, s, 0o600) }
func (l Layout) WriteApplied(raw []byte) error { return atomicWrite(l.Applied, raw, 0o600) }
func (l Layout) WriteAppliedPlan(raw []byte) error {
	return atomicWrite(l.AppliedPlan, raw, 0o600)
}
func (l Layout) WriteTransition(t Transition) error {
	if t.Version != 1 || (t.Phase != "rollback" && t.Phase != "commit") {
		return fmt.Errorf("invalid transition version or phase")
	}
	if t.Workspace.Root != "" || len(t.Workspace.Entries) != 0 {
		if err := ValidateDigestTree(t.Workspace); err != nil {
			return fmt.Errorf("validate transition workspace before write: %w", err)
		}
	}
	return atomicJSON(l.Transition, t, 0o600)
}
func (l Layout) ReadTransition() (Transition, error) {
	var transition Transition
	raw, err := os.ReadFile(l.Transition)
	if err != nil {
		return transition, err
	}
	if err := json.Unmarshal(raw, &transition); err != nil {
		return transition, fmt.Errorf("decode transition: %w", err)
	}
	if transition.Version != 1 || (transition.Phase != "rollback" && transition.Phase != "commit") {
		return transition, fmt.Errorf("unsupported transition version or phase")
	}
	return transition, nil
}
func (l Layout) ClearTransition() error {
	if err := os.Remove(l.Transition); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(l.Root)
}
func (l Layout) ClearStaging(generation int64) error {
	path := filepath.Join(l.Staging, fmt.Sprintf("g%d", generation))
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return syncDir(l.Staging)
}
func (l Layout) NextGeneration() int64 {
	s, err := l.ReadState()
	if err != nil || s.Generation < 0 {
		return 1
	}
	return s.Generation + 1
}

func atomicJSON(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(raw, '\n'), mode)
}
func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kenogram-write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(name) }
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

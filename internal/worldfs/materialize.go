package worldfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/idolum-ai/kenogram/internal/sourcetree"
)

func (l Layout) WorkspacePath(target string) string {
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(l.Workspace, hex.EncodeToString(sum[:8]))
}
func (l Layout) EnsureWorkspace(target string) (string, error) {
	path := l.WorkspacePath(target)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// EnsurePortableWritableWorkspace creates a Kenogram-owned workspace root and
// gives every contained identity write authority over that root. The enclosing
// layout remains host-private; this policy is never applied to operator-owned
// declared mount sources.
func (l Layout) EnsurePortableWritableWorkspace(target string) (string, error) {
	path, err := l.EnsureWorkspace(target)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o777); err != nil {
		return "", err
	}
	return path, nil
}
func (l Layout) StageSource(generation int64, index int, source, mode string) (string, error) {
	return l.StageSourceContext(context.Background(), generation, index, source, mode)
}
func (l Layout) StageSourceContext(ctx context.Context, generation int64, index int, source, mode string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root := filepath.Join(l.Staging, fmt.Sprintf("g%d", generation), fmt.Sprintf("copy-%d", index))
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	if _, err := parseMode(mode); err != nil {
		return "", err
	}
	if err := sourcetree.Copy(ctx, source, root); err != nil {
		return "", err
	}
	return root, nil
}
func (l Layout) ApplyStageMode(path, mode string) error {
	permissions, err := parseMode(mode)
	if err != nil {
		return err
	}
	return os.Chmod(path, permissions)
}
func parseMode(raw string) (os.FileMode, error) {
	var value uint32
	_, err := fmt.Sscanf(raw, "%o", &value)
	if err != nil {
		return 0, err
	}
	return os.FileMode(value), nil
}

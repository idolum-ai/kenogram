package worldfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
func (l Layout) StageSource(generation int64, index int, source, mode string) (string, error) {
	root := filepath.Join(l.Staging, fmt.Sprintf("g%d", generation), fmt.Sprintf("copy-%d", index))
	if err := os.RemoveAll(root); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		return "", err
	}
	permissions, err := parseMode(mode)
	if err != nil {
		return "", err
	}
	if err := copyNode(source, root, permissions); err != nil {
		return "", err
	}
	return root, nil
}
func copyNode(source, target string, topMode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("copied sources may not contain symlinks: %s", source)
	}
	if info.IsDir() {
		if err := os.Mkdir(target, topMode); err != nil && !os.IsExist(err) {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			childMode := entry.Type().Perm()
			if childMode == 0 {
				childMode = 0o600
			}
			if entry.IsDir() {
				childMode = 0o700
			}
			if err := copyNode(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name()), childMode); err != nil {
				return err
			}
		}
		return os.Chmod(target, topMode)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported copied source type: %s", source)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, topMode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
func parseMode(raw string) (os.FileMode, error) {
	var value uint32
	_, err := fmt.Sscanf(raw, "%o", &value)
	if err != nil {
		return 0, err
	}
	return os.FileMode(value), nil
}

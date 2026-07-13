package worldfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type DigestEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
}
type DigestTree struct {
	Root    string        `json:"root"`
	Entries []DigestEntry `json:"entries"`
}

func Digest(root string) (DigestTree, error) {
	return digestRetry(root, digestOnce)
}

const digestAttempts = 8

// ErrWorkspaceChanging identifies a digest that could not observe one stable
// point-in-time view of a live workspace.
var ErrWorkspaceChanging = errors.New("workspace is changing")

type treeChangedError struct {
	path  string
	cause error
}

func (e *treeChangedError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("workspace changed while hashing %s: %v", e.path, e.cause)
	}
	return fmt.Sprintf("file changed while hashing: %s", e.path)
}

func (e *treeChangedError) Unwrap() error { return e.cause }

func (e *treeChangedError) Is(target error) bool { return target == ErrWorkspaceChanging }

// IsChanging reports whether a digest failed only because the tree mutated
// while it was being read.
func IsChanging(err error) bool { return errors.Is(err, ErrWorkspaceChanging) }

func digestRetry(root string, digest func(string) (DigestTree, error)) (DigestTree, error) {
	var last error
	for attempt := 0; attempt < digestAttempts; attempt++ {
		tree, err := digest(root)
		if err == nil {
			return tree, nil
		}
		var changed *treeChangedError
		if !errors.As(err, &changed) {
			return DigestTree{}, fmt.Errorf("digest workspace: %w", err)
		}
		last = err
		if attempt+1 < digestAttempts {
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
		}
	}
	return DigestTree{}, fmt.Errorf("digest workspace after %d attempts: %w", digestAttempts, last)
}

func digestOnce(root string) (DigestTree, error) {
	entries := []DigestEntry{}
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path != root && os.IsNotExist(walkErr) {
				return &treeChangedError{path: path, cause: walkErr}
			}
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		info, err := item.Info()
		if err != nil {
			if path != root && os.IsNotExist(err) {
				return &treeChangedError{path: path, cause: err}
			}
			return err
		}
		entry := DigestEntry{Path: rel, Mode: uint32(info.Mode().Perm()), Size: info.Size()}
		switch {
		case info.Mode().IsRegular():
			entry.Type = "file"
			sum, err := hashFile(path, info)
			if err != nil {
				return err
			}
			entry.SHA256 = sum
		case info.IsDir():
			entry.Type = "directory"
			// Directory byte sizes are filesystem bookkeeping and do not
			// describe the carried tree's semantic content.
			entry.Size = 0
		case info.Mode()&os.ModeSymlink != 0:
			entry.Type = "symlink"
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry.Link = link
		case info.Mode()&os.ModeSocket != 0:
			entry.Type = "socket"
		case info.Mode()&os.ModeNamedPipe != 0:
			entry.Type = "fifo"
		case info.Mode()&os.ModeDevice != 0:
			entry.Type = "device"
		default:
			entry.Type = "special"
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return DigestTree{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	raw, err := json.Marshal(entries)
	if err != nil {
		return DigestTree{}, err
	}
	sum := sha256.Sum256(raw)
	return DigestTree{Root: hex.EncodeToString(sum[:]), Entries: entries}, nil
}
func hashFile(path string, before os.FileInfo) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return "", &treeChangedError{path: path}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
func (l Layout) WriteDigest(generation int64, tree DigestTree) (string, error) {
	path := filepath.Join(l.Digests, fmt.Sprintf("g%d.json", generation))
	return path, atomicJSON(path, tree, 0o600)
}
func (l Layout) ReadDigest(generation int64) (DigestTree, error) {
	var tree DigestTree
	raw, err := os.ReadFile(filepath.Join(l.Digests, fmt.Sprintf("g%d.json", generation)))
	if err != nil {
		return tree, err
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return tree, err
	}
	return tree, nil
}
func ChangedFiles(before, after DigestTree) int {
	a := map[string]string{}
	for _, e := range before.Entries {
		raw, _ := json.Marshal(e)
		a[e.Path] = string(raw)
	}
	changed := 0
	for _, e := range after.Entries {
		raw, _ := json.Marshal(e)
		if a[e.Path] != string(raw) {
			changed++
		}
		delete(a, e.Path)
	}
	return changed + len(a)
}
func ShortDigest(value string) string {
	if len(value) <= 12 {
		return value
	}
	return strings.ToLower(value[:12])
}

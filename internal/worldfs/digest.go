package worldfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	entries := []DigestEntry{}
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
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
		return DigestTree{}, fmt.Errorf("digest workspace: %w", err)
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
		return "", fmt.Errorf("file changed while hashing: %s", path)
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

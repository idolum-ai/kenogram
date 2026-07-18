package worldfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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

type DigestLimits struct {
	MaxEntries       int
	MaxMetadataBytes int64
	MaxFileBytes     int64
}

func Digest(root string) (DigestTree, error) {
	return DigestContext(context.Background(), root)
}

func DigestContext(ctx context.Context, root string) (DigestTree, error) {
	return DigestContextWithLimits(ctx, root, DigestLimits{})
}

func DigestContextWithLimits(ctx context.Context, root string, limits DigestLimits) (DigestTree, error) {
	return digestRetryContext(ctx, root, func(ctx context.Context, root string) (DigestTree, error) {
		return digestOnceContextWithLimits(ctx, root, limits)
	})
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
	return digestRetryContext(context.Background(), root, func(_ context.Context, root string) (DigestTree, error) {
		return digest(root)
	})
}

func digestRetryContext(ctx context.Context, root string, digest func(context.Context, string) (DigestTree, error)) (DigestTree, error) {
	var last error
	for attempt := 0; attempt < digestAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return DigestTree{}, fmt.Errorf("digest workspace: %w", err)
		}
		tree, err := digest(ctx, root)
		if err == nil {
			return tree, nil
		}
		var changed *treeChangedError
		if !errors.As(err, &changed) {
			return DigestTree{}, fmt.Errorf("digest workspace: %w", err)
		}
		last = err
		if attempt+1 < digestAttempts {
			timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return DigestTree{}, fmt.Errorf("digest workspace: %w", ctx.Err())
			case <-timer.C:
			}
		}
	}
	return DigestTree{}, fmt.Errorf("digest workspace after %d attempts: %w", digestAttempts, last)
}

func digestOnce(root string) (DigestTree, error) {
	return digestOnceContext(context.Background(), root)
}

func digestOnceContext(ctx context.Context, root string) (DigestTree, error) {
	return digestOnceContextWithLimits(ctx, root, DigestLimits{})
}

func digestOnceContextWithLimits(ctx context.Context, rootPath string, limits DigestLimits) (DigestTree, error) {
	entries, err := walkDigestRoot(ctx, rootPath, limits)
	if err != nil {
		return DigestTree{}, err
	}
	if err := ctx.Err(); err != nil {
		return DigestTree{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if err := ctx.Err(); err != nil {
		return DigestTree{}, err
	}
	digestRoot, err := digestEntriesRootContext(ctx, entries)
	if err != nil {
		return DigestTree{}, err
	}
	return DigestTree{Root: digestRoot, Entries: entries}, nil
}
func hashFile(path string, before os.FileInfo) (string, error) {
	return hashFileContext(context.Background(), path, before)
}

func hashFileContext(ctx context.Context, path string, before os.FileInfo) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashOpenedFileContext(ctx, path, f, before)
}

func hashOpenedFileContext(ctx context.Context, path string, f *os.File, before os.FileInfo) (string, error) {
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", &treeChangedError{path: path}
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, reader: f}); err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return "", &treeChangedError{path: path}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
func (l Layout) WriteDigest(generation int64, tree DigestTree) (string, error) {
	path := filepath.Join(l.Digests, fmt.Sprintf("g%d.json", generation))
	return path, atomicJSON(path, tree, 0o600)
}
func (l Layout) ReadDigest(generation int64) (DigestTree, error) {
	return l.ReadDigestContext(context.Background(), generation)
}

func (l Layout) ReadDigestContext(ctx context.Context, generation int64) (DigestTree, error) {
	var tree DigestTree
	if err := ctx.Err(); err != nil {
		return tree, err
	}
	f, err := os.Open(filepath.Join(l.Digests, fmt.Sprintf("g%d.json", generation)))
	if err != nil {
		return tree, err
	}
	defer f.Close()
	decoder := json.NewDecoder(contextReader{ctx: ctx, reader: f})
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tree); err != nil {
		return tree, err
	}
	if err := requireDigestEOF(ctx, decoder); err != nil {
		return DigestTree{}, err
	}
	if err := ValidateDigestTreeContext(ctx, tree); err != nil {
		return DigestTree{}, fmt.Errorf("validate digest tree: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return DigestTree{}, err
	}
	return tree, nil
}

func requireDigestEOF(ctx context.Context, decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err == nil {
		return fmt.Errorf("digest tree contains trailing JSON")
	} else if errors.Is(err, io.EOF) {
		return nil
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	} else {
		return fmt.Errorf("decode trailing digest data: %w", err)
	}
}

// ValidateDigestTree proves that durable workspace evidence has the canonical
// shape and root hash emitted by Digest. Decodable JSON alone is not evidence.
func ValidateDigestTree(tree DigestTree) error {
	return ValidateDigestTreeContext(context.Background(), tree)
}

func ValidateDigestTreeContext(ctx context.Context, tree DigestTree) error {
	if len(tree.Entries) == 0 {
		return fmt.Errorf("entries are empty")
	}
	types := make(map[string]string, len(tree.Entries))
	for i, entry := range tree.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i == 0 {
			if entry.Path != "" || entry.Type != "directory" {
				return fmt.Errorf("first entry is not the workspace root directory")
			}
		} else if tree.Entries[i-1].Path >= entry.Path {
			return fmt.Errorf("entries are not in unique canonical path order at %q", entry.Path)
		}
		if entry.Path != "" && (path.IsAbs(entry.Path) || entry.Path == "." || entry.Path == ".." || strings.HasPrefix(entry.Path, "../") || path.Clean(entry.Path) != entry.Path) {
			return fmt.Errorf("entry path %q is not canonical", entry.Path)
		}
		if entry.Path != "" {
			parent := path.Dir(entry.Path)
			if parent == "." {
				parent = ""
			}
			if types[parent] != "directory" {
				return fmt.Errorf("entry %q has missing or non-directory parent %q", entry.Path, parent)
			}
		}
		if entry.Mode > 0o777 {
			return fmt.Errorf("entry %q has invalid mode", entry.Path)
		}
		if entry.Size < 0 {
			return fmt.Errorf("entry %q has negative size", entry.Path)
		}
		switch entry.Type {
		case "file":
			if !isLowerSHA256(entry.SHA256) {
				return fmt.Errorf("file entry %q has invalid sha256", entry.Path)
			}
		case "directory":
			if entry.Size != 0 {
				return fmt.Errorf("directory entry %q has nonzero size", entry.Path)
			}
		case "symlink", "socket", "fifo", "device", "special":
		default:
			return fmt.Errorf("entry %q has invalid type %q", entry.Path, entry.Type)
		}
		if entry.Type != "file" && entry.SHA256 != "" {
			return fmt.Errorf("non-file entry %q has a sha256", entry.Path)
		}
		if entry.Type != "symlink" && entry.Link != "" {
			return fmt.Errorf("non-symlink entry %q has a link target", entry.Path)
		}
		types[entry.Path] = entry.Type
	}
	want, err := digestEntriesRootContext(ctx, tree.Entries)
	if err != nil {
		return err
	}
	if tree.Root != want {
		return fmt.Errorf("root hash mismatch")
	}
	return nil
}

func digestEntriesRoot(entries []DigestEntry) (string, error) {
	return digestEntriesRootContext(context.Background(), entries)
}

func digestEntriesRootContext(ctx context.Context, entries []DigestEntry) (string, error) {
	hash := sha256.New()
	_, _ = io.WriteString(hash, "[")
	for index, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if index > 0 {
			_, _ = io.WriteString(hash, ",")
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return "", err
		}
		_, _ = hash.Write(raw)
	}
	_, _ = io.WriteString(hash, "]")
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func isLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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

// Package sourcetree owns the bounded, context-aware traversal used whenever
// Kenogram plans, digests, inspects, or stages an operator-supplied source tree.
package sourcetree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	MaxEntries   = int64(20_000)
	MaxBytes     = int64(1 << 30)
	MaxDepth     = 128
	MaxPathBytes = 4096
)

// Entry is one descriptor-rooted source observation. Reader is non-nil only
// for regular files during content traversal and must be consumed completely.
type Entry struct {
	Relative string
	Info     fs.FileInfo
	Reader   io.Reader
}

// Inspect traverses metadata only. It still applies every tree bound and
// rejects symlinks and special nodes after the visitor has a chance to classify
// the node more specifically.
func Inspect(ctx context.Context, source string, visit func(Entry) error) error {
	return walk(ctx, source, false, visit)
}

// Walk traverses every ordinary node and supplies bounded descriptor bytes for
// each regular file.
func Walk(ctx context.Context, source string, visit func(Entry) error) error {
	return walk(ctx, source, true, visit)
}

func walk(ctx context.Context, source string, content bool, visit func(Entry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	initial, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if initial.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source tree contains symlink at %s", source)
	}
	state := walkState{ctx: ctx, content: content, visit: visit}
	if !initial.IsDir() {
		return state.one(source, ".", initial, func() (*os.File, error) { return os.Open(source) })
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	bound, err := root.Lstat(".")
	if err != nil || !os.SameFile(initial, bound) {
		return errors.New("source tree root changed during descriptor binding")
	}
	return fs.WalkDir(root.FS(), ".", func(relative string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := root.Lstat(relative)
		if err != nil {
			return err
		}
		return state.one(source, relative, info, func() (*os.File, error) { return root.Open(relative) })
	})
}

type walkState struct {
	ctx     context.Context
	content bool
	visit   func(Entry) error
	entries int64
	bytes   int64
}

func (s *walkState) one(source, relative string, info fs.FileInfo, open func() (*os.File, error)) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	relative = filepath.ToSlash(relative)
	if len(relative) > MaxPathBytes || treeDepth(relative) > MaxDepth {
		return fmt.Errorf("source tree path exceeds bound at %s", relative)
	}
	s.entries++
	if s.entries > MaxEntries {
		return fmt.Errorf("source tree exceeds %d entries", MaxEntries)
	}
	if info.Mode().IsRegular() {
		if info.Size() < 0 || info.Size() > MaxBytes-s.bytes {
			return fmt.Errorf("source tree exceeds %d bytes", MaxBytes)
		}
		s.bytes += info.Size()
	}
	entry := Entry{Relative: relative, Info: info}
	if !s.content || !info.Mode().IsRegular() {
		if s.visit != nil {
			if err := s.visit(entry); err != nil {
				return err
			}
		}
		return rejectSpecial(source, relative, info)
	}
	file, err := open()
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		file.Close()
		return fmt.Errorf("source tree file changed during open at %s", relative)
	}
	counter := &countingReader{reader: &contextReader{ctx: s.ctx, reader: io.LimitReader(file, opened.Size()+1)}}
	entry.Info, entry.Reader = opened, counter
	visitErr := error(nil)
	if s.visit != nil {
		visitErr = s.visit(entry)
	}
	after, statErr := file.Stat()
	closeErr := file.Close()
	if visitErr != nil {
		return errors.Join(visitErr, statErr, closeErr)
	}
	if statErr != nil || closeErr != nil || counter.read != opened.Size() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return errors.Join(statErr, closeErr, fmt.Errorf("source tree file changed or was incompletely read at %s", relative))
	}
	return s.ctx.Err()
}

func rejectSpecial(source, relative string, info fs.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source tree contains symlink at %s", filepath.Join(source, relative))
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("source tree contains special node at %s", filepath.Join(source, relative))
	}
	return nil
}

func treeDepth(relative string) int {
	if relative == "." || relative == "" {
		return 0
	}
	return strings.Count(relative, "/") + 1
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

type countingReader struct {
	reader io.Reader
	read   int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	r.read += int64(count)
	return count, err
}

// Digest returns the canonical content-and-mode fingerprint used by plans and
// runtime source attestations.
func Digest(ctx context.Context, source string) (string, error) {
	exact, _, err := DigestAndReadOnlyProjection(ctx, source)
	return exact, err
}

// ReadOnlyProjectionDigest derives the canonical digest that
// ProjectReadOnly must produce without mutating the exact source tree.
func ReadOnlyProjectionDigest(ctx context.Context, source string) (string, error) {
	_, projected, err := DigestAndReadOnlyProjection(ctx, source)
	return projected, err
}

// DigestAndReadOnlyProjection computes the exact and portable-read-only
// inventories in one bounded traversal so both digests bind the same bytes,
// paths, types, and source modes.
func DigestAndReadOnlyProjection(ctx context.Context, source string) (string, string, error) {
	exactEntries, projectedEntries := []string{}, []string{}
	err := Walk(ctx, source, func(entry Entry) error {
		switch {
		case entry.Info.IsDir():
			exactEntries = append(exactEntries, "d\x00"+entry.Relative+"\x00"+entry.Info.Mode().Perm().String())
			projectedEntries = append(projectedEntries, "d\x00"+entry.Relative+"\x00"+portableReadOnlyMode(entry.Info).String())
		case entry.Info.Mode().IsRegular():
			hash := sha256.New()
			if _, err := io.Copy(hash, entry.Reader); err != nil {
				return err
			}
			content := hex.EncodeToString(hash.Sum(nil))
			exactEntries = append(exactEntries, "f\x00"+entry.Relative+"\x00"+content+"\x00"+entry.Info.Mode().Perm().String())
			projectedEntries = append(projectedEntries, "f\x00"+entry.Relative+"\x00"+content+"\x00"+portableReadOnlyMode(entry.Info).String())
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	canonical := func(entries []string) string {
		sort.Strings(entries)
		hash := sha256.New()
		for _, entry := range entries {
			_, _ = io.WriteString(hash, entry)
			_, _ = hash.Write([]byte{'\n'})
		}
		return hex.EncodeToString(hash.Sum(nil))
	}
	return canonical(exactEntries), canonical(projectedEntries), nil
}

// ContentDigest preserves the runtime mount observation convention: a regular
// root is hashed as its exact bytes, while a directory is hashed as the
// canonical content-and-mode inventory produced by the shared walker.
func ContentDigest(ctx context.Context, source string) (string, error) {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if rootInfo.Mode().IsRegular() {
		var result string
		err := Walk(ctx, source, func(entry Entry) error {
			if entry.Relative != "." || !entry.Info.Mode().IsRegular() || entry.Reader == nil {
				return errors.New("regular source root changed during content digest")
			}
			hash := sha256.New()
			if _, err := io.Copy(hash, entry.Reader); err != nil {
				return err
			}
			result = hex.EncodeToString(hash.Sum(nil))
			return nil
		})
		return result, err
	}
	return Digest(ctx, source)
}

// ProjectReadOnly makes a private staging copy readable and traversable by an
// arbitrary contained identity without making any node writable. Regular-file
// executability is retained as a boolean property and propagated to every
// contained identity. The caller must keep the staging parent host-private.
func ProjectReadOnly(ctx context.Context, source string) error {
	return rewritePermissions(ctx, source, func(info fs.FileInfo) (fs.FileMode, bool) {
		if !info.IsDir() && !info.Mode().IsRegular() {
			return 0, false
		}
		return portableReadOnlyMode(info), true
	})
}

func portableReadOnlyMode(info fs.FileInfo) fs.FileMode {
	if info.IsDir() {
		return 0o555
	}
	mode := fs.FileMode(0o444)
	if info.Mode().Perm()&0o111 != 0 {
		mode |= 0o111
	}
	return mode
}

// PrepareRemoval restores owner write permission only to directories in a
// bounded staging tree. It is used after the contained runtime is absent so a
// host-private read-only projection can be removed without broadening files.
func PrepareRemoval(ctx context.Context, source string) error {
	return rewritePermissions(ctx, source, func(info fs.FileInfo) (fs.FileMode, bool) {
		if info.IsDir() {
			return 0o700, true
		}
		return 0, false
	})
}

func rewritePermissions(ctx context.Context, source string, projected func(fs.FileInfo) (fs.FileMode, bool)) error {
	return Inspect(ctx, source, func(entry Entry) error {
		mode, ok := projected(entry.Info)
		if !ok {
			return nil
		}
		path := source
		if entry.Relative != "." {
			path = filepath.Join(source, filepath.FromSlash(entry.Relative))
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(entry.Info, opened) || opened.Mode().Type() != entry.Info.Mode().Type() || opened.Mode().Perm() != entry.Info.Mode().Perm() || opened.Size() != entry.Info.Size() || !opened.ModTime().Equal(entry.Info.ModTime()) {
			return errors.Join(statErr, file.Close(), fmt.Errorf("staged source changed before permission projection at %s", entry.Relative))
		}
		chmodErr := file.Chmod(mode)
		after, afterErr := file.Stat()
		if chmodErr != nil || afterErr != nil || !os.SameFile(opened, after) || after.Mode().Type() != opened.Mode().Type() || after.Mode().Perm() != mode || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
			return errors.Join(chmodErr, afterErr, file.Close(), fmt.Errorf("staged source permission projection failed at %s", entry.Relative))
		}
		return file.Close()
	})
}

// Copy creates a bounded, create-only content-and-mode snapshot at target.
func Copy(ctx context.Context, source, target string) (retErr error) {
	directories := []Entry{}
	created := false
	defer func() {
		if retErr != nil && created {
			_ = os.RemoveAll(target)
		}
	}()
	err := Walk(ctx, source, func(entry Entry) error {
		destination := target
		if entry.Relative != "." {
			destination = filepath.Join(target, filepath.FromSlash(entry.Relative))
		}
		if entry.Info.IsDir() {
			if err := os.Mkdir(destination, 0o700); err != nil {
				return err
			}
			if entry.Relative == "." {
				created = true
			}
			directories = append(directories, entry)
			return nil
		}
		if !entry.Info.Mode().IsRegular() || entry.Reader == nil {
			return fmt.Errorf("source snapshot contains a non-regular file at %s", entry.Relative)
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if entry.Relative == "." {
			created = true
		}
		written, copyErr := io.Copy(output, entry.Reader)
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || written != entry.Info.Size() {
			return errors.Join(copyErr, syncErr, closeErr, errors.New("source snapshot copy is incomplete"))
		}
		return os.Chmod(destination, entry.Info.Mode().Perm())
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		destination := target
		if directories[index].Relative != "." {
			destination = filepath.Join(target, filepath.FromSlash(directories[index].Relative))
		}
		if err := os.Chmod(destination, directories[index].Info.Mode().Perm()); err != nil {
			return err
		}
	}
	return ctx.Err()
}

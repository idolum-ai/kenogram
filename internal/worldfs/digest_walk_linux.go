//go:build linux

package worldfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
)

const maxDigestDirectoryDepth = 256

type digestWalkState struct {
	limits        DigestLimits
	entries       []DigestEntry
	metadataBytes int64
	fileBytes     int64
}

// walkDigestRoot keeps every descendant access relative to an already-opened
// directory descriptor. O_NOFOLLOW prevents an inhabitant from replacing an
// observed ancestor with a symlink that escapes the workspace between lookup
// and open.
func walkDigestRoot(ctx context.Context, rootPath string, limits DigestLimits) ([]DigestEntry, error) {
	before, err := os.Lstat(rootPath)
	if err != nil {
		return nil, err
	}
	root, err := os.Open(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	opened, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, &treeChangedError{path: rootPath}
	}
	state := &digestWalkState{limits: limits}
	if err := state.appendMetadata(DigestEntry{Path: "", Type: "directory", Mode: uint32(opened.Mode().Perm())}); err != nil {
		return nil, err
	}
	if err := walkDigestDirectory(ctx, root, "", 0, state); err != nil {
		return nil, err
	}
	return state.entries, nil
}

func walkDigestDirectory(ctx context.Context, directory *os.File, prefix string, depth int, state *digestWalkState) error {
	if depth >= maxDigestDirectoryDepth {
		return fmt.Errorf("workspace observation exceeds directory depth %d", maxDigestDirectoryDepth)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		children, readErr := directory.Readdir(256)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, observed := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := observed.Name()
			if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
				return fmt.Errorf("workspace directory contains noncanonical entry %q", name)
			}
			rel := name
			if prefix != "" {
				rel = path.Join(prefix, name)
			}
			switch {
			case observed.Mode().IsRegular():
				if err := walkDigestRegular(ctx, directory, name, rel, observed, state); err != nil {
					return err
				}
			case observed.IsDir():
				if err := walkDigestChildDirectory(ctx, directory, name, rel, observed, depth, state); err != nil {
					return err
				}
			case observed.Mode()&os.ModeSymlink != 0:
				if err := walkDigestSymlink(directory, name, rel, observed, state); err != nil {
					return err
				}
			default:
				current, err := lstatAt(directory, name)
				if err != nil {
					return err
				}
				if !os.SameFile(observed, current) || observed.Mode() != current.Mode() {
					return &treeChangedError{path: rel}
				}
				entry := DigestEntry{Path: rel, Mode: uint32(current.Mode().Perm()), Size: current.Size(), Type: digestSpecialType(current.Mode())}
				if err := state.appendMetadata(entry); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

func walkDigestRegular(ctx context.Context, directory *os.File, name, rel string, observed os.FileInfo, state *digestWalkState) error {
	fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return &treeChangedError{path: rel, cause: err}
	}
	file := os.NewFile(uintptr(fd), rel)
	if file == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open workspace file %q", rel)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(observed, opened) {
		return &treeChangedError{path: rel}
	}
	if err := state.addFileBytes(opened.Size()); err != nil {
		return err
	}
	sum, err := hashOpenedFileContext(ctx, rel, file, observed)
	if err != nil {
		return err
	}
	return state.appendMetadata(DigestEntry{Path: rel, Type: "file", Mode: uint32(opened.Mode().Perm()), Size: opened.Size(), SHA256: sum})
}

func walkDigestChildDirectory(ctx context.Context, directory *os.File, name, rel string, observed os.FileInfo, depth int, state *digestWalkState) error {
	fd, err := syscall.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return &treeChangedError{path: rel, cause: err}
	}
	child := os.NewFile(uintptr(fd), rel)
	if child == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open workspace directory %q", rel)
	}
	defer child.Close()
	opened, err := child.Stat()
	if err != nil {
		return err
	}
	if !opened.IsDir() || !os.SameFile(observed, opened) {
		return &treeChangedError{path: rel}
	}
	if err := state.appendMetadata(DigestEntry{Path: rel, Type: "directory", Mode: uint32(opened.Mode().Perm())}); err != nil {
		return err
	}
	return walkDigestDirectory(ctx, child, rel, depth+1, state)
}

func walkDigestSymlink(directory *os.File, name, rel string, observed os.FileInfo, state *digestWalkState) error {
	linkPath := fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), name)
	target, err := os.Readlink(linkPath)
	if err != nil {
		return &treeChangedError{path: rel, cause: err}
	}
	current, err := os.Lstat(linkPath)
	if err != nil {
		return &treeChangedError{path: rel, cause: err}
	}
	if current.Mode()&os.ModeSymlink == 0 || !os.SameFile(observed, current) {
		return &treeChangedError{path: rel}
	}
	return state.appendMetadata(DigestEntry{Path: rel, Type: "symlink", Mode: uint32(current.Mode().Perm()), Size: current.Size(), Link: target})
}

func lstatAt(directory *os.File, name string) (os.FileInfo, error) {
	return os.Lstat(fmt.Sprintf("/proc/self/fd/%d/%s", directory.Fd(), name))
}

func digestSpecialType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeNamedPipe != 0:
		return "fifo"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return "special"
	}
}

func (s *digestWalkState) appendMetadata(entry DigestEntry) error {
	if s.limits.MaxEntries > 0 && len(s.entries) >= s.limits.MaxEntries {
		return fmt.Errorf("workspace observation exceeds %d entries", s.limits.MaxEntries)
	}
	bytes := int64(len(entry.Path) + len(entry.Link))
	if s.limits.MaxMetadataBytes > 0 && bytes > s.limits.MaxMetadataBytes-s.metadataBytes {
		return fmt.Errorf("workspace observation exceeds %d metadata bytes", s.limits.MaxMetadataBytes)
	}
	s.metadataBytes += bytes
	s.entries = append(s.entries, entry)
	return nil
}

func (s *digestWalkState) addFileBytes(size int64) error {
	if size < 0 {
		return fmt.Errorf("workspace regular file has negative size")
	}
	if s.limits.MaxFileBytes > 0 && size > s.limits.MaxFileBytes-s.fileBytes {
		return fmt.Errorf("workspace observation exceeds %d regular-file bytes", s.limits.MaxFileBytes)
	}
	s.fileBytes += size
	return nil
}

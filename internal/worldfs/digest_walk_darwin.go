//go:build darwin

package worldfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// walkDigestRoot uses os.Root on Darwin. Root keeps every lookup beneath an
// opened directory capability even when inhabitants race symlink changes.
// Linux retains its stricter O_NOFOLLOW descriptor-relative implementation.
func walkDigestRoot(ctx context.Context, rootPath string, limits DigestLimits) ([]DigestEntry, error) {
	before, err := os.Lstat(rootPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	opened, err := root.Stat(".")
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
	if err := walkDigestRootDirectory(ctx, root, ".", "", 0, state); err != nil {
		return nil, err
	}
	return state.entries, nil
}

func walkDigestRootDirectory(ctx context.Context, root *os.Root, rootName, prefix string, depth int, state *digestWalkState) error {
	if depth >= maxDigestDirectoryDepth {
		return fmt.Errorf("workspace observation exceeds directory depth %d", maxDigestDirectoryDepth)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := root.Open(rootName)
	if err != nil {
		return &treeChangedError{path: prefix, cause: err}
	}
	defer directory.Close()
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
			if err := validateDigestEntryUTF8(DigestEntry{Path: rel}); err != nil {
				return err
			}
			current, err := root.Lstat(rel)
			if err != nil || !os.SameFile(observed, current) || observed.Mode() != current.Mode() {
				return &treeChangedError{path: rel, cause: err}
			}
			switch {
			case current.Mode().IsRegular():
				if err := walkDigestRootRegular(ctx, root, rel, current, state); err != nil {
					return err
				}
			case current.IsDir():
				if err := state.appendMetadata(DigestEntry{Path: rel, Type: "directory", Mode: uint32(current.Mode().Perm())}); err != nil {
					return err
				}
				if err := walkDigestRootDirectory(ctx, root, rel, rel, depth+1, state); err != nil {
					return err
				}
			case current.Mode()&os.ModeSymlink != 0:
				target, err := root.Readlink(rel)
				if err != nil {
					return &treeChangedError{path: rel, cause: err}
				}
				after, err := root.Lstat(rel)
				if err != nil || after.Mode()&os.ModeSymlink == 0 || !os.SameFile(current, after) {
					return &treeChangedError{path: rel, cause: err}
				}
				if err := state.appendMetadata(DigestEntry{Path: rel, Type: "symlink", Mode: uint32(after.Mode().Perm()), Size: after.Size(), Link: target}); err != nil {
					return err
				}
			default:
				if err := state.appendMetadata(DigestEntry{Path: rel, Mode: uint32(current.Mode().Perm()), Size: current.Size(), Type: digestSpecialType(current.Mode())}); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

func walkDigestRootRegular(ctx context.Context, root *os.Root, rel string, observed os.FileInfo, state *digestWalkState) error {
	file, err := root.Open(rel)
	if err != nil {
		return &treeChangedError{path: rel, cause: err}
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	afterLookup, err := root.Lstat(rel)
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(observed, opened) || !os.SameFile(observed, afterLookup) || afterLookup.Mode()&os.ModeSymlink != 0 {
		return &treeChangedError{path: rel, cause: err}
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

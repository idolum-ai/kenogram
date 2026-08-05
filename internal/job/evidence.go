package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

type evidenceWriter struct {
	root      *os.Root
	entries   []jobcontract.ManifestEntry
	afterSync func(string) error
}

func createEvidence(path string, afterSync func(string) error) (*evidenceWriter, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("evidence directory must be an absolute clean path")
	}
	parent := filepath.Dir(path)
	leaf := filepath.Base(path)
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("open evidence parent: %w", err)
	}
	defer parentRoot.Close()
	if err := parentRoot.Mkdir(leaf, 0o700); err != nil {
		return nil, fmt.Errorf("create evidence directory without replacement: %w", err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	created, statErr := parentRoot.Lstat(leaf)
	opened, openErr := root.Stat(".")
	if statErr != nil || openErr != nil || !created.IsDir() || !opened.IsDir() || !os.SameFile(created, opened) {
		root.Close()
		return nil, fmt.Errorf("evidence directory identity changed during creation")
	}
	parentDirectory, err := parentRoot.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	syncErr := parentDirectory.Sync()
	closeErr := parentDirectory.Close()
	if syncErr != nil || closeErr != nil {
		root.Close()
		return nil, errors.Join(syncErr, closeErr)
	}
	return &evidenceWriter{root: root, afterSync: afterSync}, nil
}

func (w *evidenceWriter) Close() error { return w.root.Close() }

func (w *evidenceWriter) Write(name, kind string, raw []byte) error {
	file, err := w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create evidence %s: %w", name, err)
	}
	writeErr := writeAndSync(file, raw)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if w.afterSync != nil {
		if err := w.afterSync(name); err != nil {
			return err
		}
	}
	w.entries = append(w.entries, jobcontract.ManifestEntry{Path: name, Kind: kind, Size: int64(len(raw)), SHA256: digest(raw)})
	return nil
}

func (w *evidenceWriter) Stream(name, kind string, maximum int64) (*capture, error) {
	file, err := w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &capture{owner: w, file: file, name: name, kind: kind, maximum: maximum, hash: sha256.New()}, nil
}

func (w *evidenceWriter) WriteArtifacts(ctx context.Context, request jobcontract.ArtifactRequest, artifacts []Artifact) (inventory artifactInventory, retErr error) {
	if int64(len(artifacts)) > request.MaxEntries {
		return artifactInventory{}, fmt.Errorf("artifact inventory exceeds %d entries", request.MaxEntries)
	}
	entryStart := len(w.entries)
	defer func() {
		if retErr == nil {
			return
		}
		w.entries = w.entries[:entryStart]
		_ = w.root.RemoveAll("target-artifacts")
	}()
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	inventory = artifactInventory{Schema: "kenogram.target-artifact-inventory.v1", Root: request.ContainerRoot, Entries: []artifactInventoryEntry{}}
	prior := ""
	var total int64
	for _, artifact := range artifacts {
		if artifact.Path <= prior || artifact.Open == nil || jobcontract.ValidateEvidenceRelativePath(artifact.Path) != nil {
			return artifactInventory{}, fmt.Errorf("artifact path %q is invalid, duplicated, or unordered", artifact.Path)
		}
		prior = artifact.Path
		reader, err := openArtifact(ctx, artifact.Open)
		if err != nil {
			return artifactInventory{}, err
		}
		name := path.Join("target-artifacts", artifact.Path)
		if err := w.root.MkdirAll(path.Dir(name), 0o700); err != nil {
			reader.Close()
			return artifactInventory{}, err
		}
		file, err := w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			reader.Close()
			return artifactInventory{}, err
		}
		hash := sha256.New()
		remaining := request.MaxBytes - total
		written, copyErr := copyArtifact(ctx, io.MultiWriter(file, hash), reader, remaining+1)
		closeReadErr := closeArtifact(ctx, reader)
		syncErr := file.Sync()
		closeWriteErr := file.Close()
		if copyErr != nil || closeReadErr != nil || syncErr != nil || closeWriteErr != nil {
			return artifactInventory{}, errors.Join(copyErr, closeReadErr, syncErr, closeWriteErr)
		}
		if written > remaining {
			return artifactInventory{}, fmt.Errorf("artifact content exceeds %d bytes", request.MaxBytes)
		}
		total += written
		sum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		w.entries = append(w.entries, jobcontract.ManifestEntry{Path: name, Kind: "target_artifact", Size: written, SHA256: sum})
		inventory.Entries = append(inventory.Entries, artifactInventoryEntry{Path: artifact.Path, Size: written, SHA256: sum})
	}
	return inventory, nil
}

type artifactOpenResult struct {
	reader io.ReadCloser
	err    error
}

func openArtifact(ctx context.Context, open func() (io.ReadCloser, error)) (io.ReadCloser, error) {
	result := make(chan artifactOpenResult, 1)
	go func() {
		reader, err := open()
		result <- artifactOpenResult{reader: reader, err: err}
	}()
	select {
	case observed := <-result:
		return observed.reader, observed.err
	case <-ctx.Done():
		go func() {
			observed := <-result
			if observed.reader != nil {
				_ = observed.reader.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

type artifactCopyResult struct {
	written int64
	err     error
}

func copyArtifact(ctx context.Context, destination io.Writer, source io.ReadCloser, maximum int64) (int64, error) {
	result := make(chan artifactCopyResult, 1)
	go func() {
		written, err := io.Copy(destination, io.LimitReader(source, maximum))
		result <- artifactCopyResult{written: written, err: err}
	}()
	select {
	case observed := <-result:
		return observed.written, observed.err
	case <-ctx.Done():
		go source.Close()
		return 0, ctx.Err()
	}
}

func closeArtifact(ctx context.Context, source io.Closer) error {
	result := make(chan error, 1)
	go func() { result <- source.Close() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *evidenceWriter) Seal(jobID, requestDigest, resultDigest string, at time.Time) (jobcontract.Manifest, error) {
	sort.Slice(w.entries, func(i, j int) bool { return w.entries[i].Path < w.entries[j].Path })
	manifest := jobcontract.Manifest{
		Schema: jobcontract.ManifestSchema, JobID: jobID, RequestSHA256: requestDigest,
		ResultSHA256: resultDigest, ContentSHA256: contentDigest(w.entries),
		SealedAt: at.Format(time.RFC3339Nano), Entries: append([]jobcontract.ManifestEntry{}, w.entries...),
	}
	raw, err := marshalDocument(manifest)
	if err != nil {
		return manifest, err
	}
	if _, err := jobcontract.ParseManifest(raw); err != nil {
		return manifest, fmt.Errorf("validate produced manifest: %w", err)
	}
	if err := w.syncDirectories(); err != nil {
		return manifest, err
	}
	file, err := w.root.OpenFile("manifest.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return manifest, err
	}
	if err := writeAndSync(file, raw); err != nil {
		file.Close()
		return manifest, err
	}
	if w.afterSync != nil {
		if err := w.afterSync("manifest.json"); err != nil {
			file.Close()
			return manifest, err
		}
	}
	published, err := file.Stat()
	if err != nil {
		file.Close()
		return manifest, err
	}
	if err := file.Close(); err != nil {
		return manifest, err
	}
	lookup, err := w.root.Lstat("manifest.json")
	if err != nil || !lookup.Mode().IsRegular() || !os.SameFile(published, lookup) || lookup.Size() != int64(len(raw)) {
		return manifest, errors.New("published manifest identity changed before directory sync")
	}
	directory, err := w.root.Open(".")
	if err != nil {
		return manifest, err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return manifest, err
	}
	return manifest, closeErr
}

func (w *evidenceWriter) syncDirectories() error {
	directories := map[string]struct{}{".": {}}
	for _, entry := range w.entries {
		for directory := path.Dir(entry.Path); directory != "."; directory = path.Dir(directory) {
			directories[directory] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(directories))
	for directory := range directories {
		ordered = append(ordered, directory)
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := strings.Count(ordered[i], "/"), strings.Count(ordered[j], "/")
		if left != right {
			return left > right
		}
		return ordered[i] > ordered[j]
	})
	for _, name := range ordered {
		directory, err := w.root.Open(name)
		if err != nil {
			return err
		}
		info, statErr := directory.Stat()
		lookup, lookupErr := w.root.Lstat(name)
		syncErr := directory.Sync()
		closeErr := directory.Close()
		var identityErr error
		if statErr == nil && lookupErr == nil && (!info.IsDir() || !lookup.IsDir() || lookup.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, lookup)) {
			identityErr = errors.New("evidence directory identity changed before sync")
		}
		if err := errors.Join(statErr, lookupErr, syncErr, closeErr, identityErr); err != nil {
			return err
		}
	}
	return nil
}

type capture struct {
	mu      sync.Mutex
	owner   *evidenceWriter
	file    *os.File
	name    string
	kind    string
	maximum int64
	total   int64
	written int64
	hash    hash.Hash
	closed  bool
}

func (c *capture) Write(raw []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, os.ErrClosed
	}
	original := len(raw)
	if int64(original) > jobcontract.MaximumWireInteger-c.total {
		return 0, errorsNew("stream byte count exceeds JSON wire integer")
	}
	c.total += int64(original)
	remaining := c.maximum - c.written
	if remaining < 0 {
		remaining = 0
	}
	retained := raw
	if int64(len(retained)) > remaining {
		retained = retained[:remaining]
	}
	if len(retained) > 0 {
		written, err := c.file.Write(retained)
		if err != nil {
			return 0, err
		}
		if written != len(retained) {
			return 0, io.ErrShortWrite
		}
		c.hash.Write(retained)
		c.written += int64(written)
	}
	return original, nil
}

func (c *capture) CloseResult() (jobcontract.StreamResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return jobcontract.StreamResult{}, errorsNew("stream already closed")
	}
	c.closed = true
	if err := c.file.Sync(); err != nil {
		c.file.Close()
		return jobcontract.StreamResult{}, err
	}
	if err := c.file.Close(); err != nil {
		return jobcontract.StreamResult{}, err
	}
	sum := "sha256:" + hex.EncodeToString(c.hash.Sum(nil))
	c.owner.entries = append(c.owner.entries, jobcontract.ManifestEntry{Path: c.name, Kind: c.kind, Size: c.written, SHA256: sum})
	return jobcontract.StreamResult{Path: c.name, SHA256: sum, CapturedBytes: c.written, TotalBytes: c.total, Truncated: c.written < c.total}, nil
}

func writeAndSync(file *os.File, raw []byte) error {
	if _, err := file.Write(raw); err != nil {
		return err
	}
	return file.Sync()
}

func contentDigest(entries []jobcontract.ManifestEntry) string {
	hash := sha256.New()
	for _, entry := range entries {
		fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%s\n", entry.Path, entry.Kind, entry.Size, entry.SHA256)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func errorsNew(value string) error { return fmt.Errorf("%s", value) }

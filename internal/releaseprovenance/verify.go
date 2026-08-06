// Package releaseprovenance independently binds a packaged Kenogram
// executable and its machine-readable provenance to verifier-owned release
// coordinates.
package releaseprovenance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
)

const maximumExecutableBytes = int64(1 << 30)

type Expected struct {
	Version    string
	Commit     string
	SourceDate string
	GOOS       string
	GOARCH     string
}

func Verify(executable, provenancePath string, expected Expected) (jobcontract.Provenance, error) {
	if expected.Version == "" || expected.Commit == "" || expected.SourceDate == "" || expected.GOOS == "" || expected.GOARCH == "" {
		return jobcontract.Provenance{}, errors.New("expected release identity is incomplete")
	}
	digest, err := regularFileDigest(executable, maximumExecutableBytes)
	if err != nil {
		return jobcontract.Provenance{}, fmt.Errorf("digest packaged executable: %w", err)
	}
	raw, err := readRegular(provenancePath, int64(jobcontract.MaximumProvenanceBytes))
	if err != nil {
		return jobcontract.Provenance{}, fmt.Errorf("read retained provenance: %w", err)
	}
	provenance, err := jobcontract.ParseProvenance(raw)
	if err != nil {
		return jobcontract.Provenance{}, err
	}
	if provenance.BuildKind != "release" || provenance.Version != expected.Version ||
		provenance.Commit != expected.Commit || provenance.SourceDate != expected.SourceDate ||
		provenance.GOOS != expected.GOOS || provenance.GOARCH != expected.GOARCH ||
		provenance.ExecutableSHA256 != digest {
		return jobcontract.Provenance{}, errors.New("packaged executable provenance disagrees with verifier-owned release identity")
	}
	return provenance, nil
}

func regularFileDigest(path string, maximum int64) (string, error) {
	file, info, err := openRegular(path, maximum)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil || written != info.Size() || written > maximum {
		return "", errors.New("executable changed or exceeded its byte boundary during hashing")
	}
	if err := recheck(file, path, info); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func readRegular(path string, maximum int64) ([]byte, error) {
	file, info, err := openRegular(path, maximum)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(raw)) != info.Size() || int64(len(raw)) > maximum {
		return nil, errors.New("file changed or exceeded its byte boundary during read")
	}
	if err := recheck(file, path, info); err != nil {
		return nil, err
	}
	return raw, nil
}

func openRegular(path string, maximum int64) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maximum {
		return nil, nil, errors.New("file is absent, irregular, symlinked, or oversized")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		file.Close()
		return nil, nil, errors.New("file changed while opening")
	}
	return file, opened, nil
}

func recheck(file *os.File, path string, opened os.FileInfo) error {
	after, err := file.Stat()
	lookup, lookupErr := os.Lstat(path)
	if err != nil || lookupErr != nil || !lookup.Mode().IsRegular() || lookup.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, after) || !os.SameFile(after, lookup) || opened.Size() != after.Size() {
		return errors.New("file changed during verification")
	}
	return nil
}

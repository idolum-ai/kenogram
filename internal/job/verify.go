package job

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/idolum-ai/kenogram/internal/jobcontract"
	"github.com/idolum-ai/kenogram/internal/plan"
)

type Verification struct {
	Schema  string `json:"schema"`
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Entries int    `json:"entries"`
}

func Verify(evidenceDir string) (Verification, error) {
	if !filepath.IsAbs(evidenceDir) || filepath.Clean(evidenceDir) != evidenceDir {
		return Verification{}, errors.New("evidence directory must be an absolute clean path")
	}
	root, err := os.OpenRoot(evidenceDir)
	if err != nil {
		return Verification{}, err
	}
	defer root.Close()
	manifestRaw, err := readRegular(root, "manifest.json", jobcontract.MaximumManifestBytes)
	if err != nil {
		return Verification{}, fmt.Errorf("read seal: %w", err)
	}
	manifest, err := jobcontract.ParseManifest(manifestRaw)
	if err != nil {
		return Verification{}, err
	}
	observed := map[string][]byte{}
	for _, entry := range manifest.Entries {
		if entry.Kind == "target_artifact" {
			if err := verifyRegularDigest(root, entry); err != nil {
				return Verification{}, err
			}
			continue
		}
		raw, err := readRegular(root, entry.Path, maximumEvidenceEntry(entry))
		if err != nil {
			return Verification{}, err
		}
		if int64(len(raw)) != entry.Size || digest(raw) != entry.SHA256 {
			return Verification{}, fmt.Errorf("evidence entry %s has changed", entry.Path)
		}
		observed[entry.Path] = raw
	}
	if contentDigest(manifest.Entries) != manifest.ContentSHA256 {
		return Verification{}, errors.New("manifest content root mismatch")
	}
	if err := verifyClosedInventory(root, manifest); err != nil {
		return Verification{}, err
	}
	request, err := jobcontract.ParseRequest(observed["request.json"])
	if err != nil {
		return Verification{}, err
	}
	result, err := jobcontract.ParseResult(observed["result.json"])
	if err != nil {
		return Verification{}, err
	}
	provenance, err := jobcontract.ParseProvenance(observed["provenance.json"])
	if err != nil {
		return Verification{}, err
	}
	if request.JobID != manifest.JobID || result.JobID != manifest.JobID ||
		digest(observed["request.json"]) != manifest.RequestSHA256 ||
		digest(observed["result.json"]) != manifest.ResultSHA256 ||
		result.RequestSHA256 != manifest.RequestSHA256 {
		return Verification{}, errors.New("job, request, result, and manifest identities disagree")
	}
	if digest(observed["declaration.toml"]) != request.Declaration.SHA256 || result.Identity.DeclarationSHA256 != request.Declaration.SHA256 {
		return Verification{}, errors.New("declaration identity mismatch")
	}
	planDigest, err := planContentDigest(observed["plan.json"])
	if err != nil {
		return Verification{}, fmt.Errorf("re-derive plan digest: %w", err)
	}
	var retainedPlan plan.Result
	if err := json.Unmarshal(observed["plan.json"], &retainedPlan); err != nil {
		return Verification{}, err
	}
	if planDigest != prefixedDigest(retainedPlan.PlanDigest) || result.Identity.PlanSHA256 != planDigest {
		return Verification{}, errors.New("plan identity mismatch")
	}
	if digest(observed["provenance.json"]) != result.Identity.ProvenanceSHA256 || provenance.ExecutableSHA256 == "" {
		return Verification{}, errors.New("provenance identity mismatch")
	}
	if runtimeEvidenceDigest(observed["runtime-before.json"], observed["runtime-after.json"]) != result.Identity.RuntimeSHA256 {
		return Verification{}, errors.New("runtime evidence identity mismatch")
	}
	if err := verifyArtifacts(request, manifest, observed); err != nil {
		return Verification{}, err
	}
	if digest(observed["stdout.bin"]) != result.Stdout.SHA256 || int64(len(observed["stdout.bin"])) != result.Stdout.CapturedBytes ||
		digest(observed["stderr.bin"]) != result.Stderr.SHA256 || int64(len(observed["stderr.bin"])) != result.Stderr.CapturedBytes {
		return Verification{}, errors.New("stream evidence mismatch")
	}
	return Verification{Schema: "kenogram.job-verification.v1", JobID: manifest.JobID, Status: result.Status, Entries: len(manifest.Entries)}, nil
}

func verifyArtifacts(request jobcontract.Request, manifest jobcontract.Manifest, observed map[string][]byte) error {
	manifestArtifacts := map[string]jobcontract.ManifestEntry{}
	for _, entry := range manifest.Entries {
		if entry.Kind == "target_artifact" {
			manifestArtifacts[entry.Path] = entry
		}
	}
	if request.Artifacts == nil {
		if len(manifestArtifacts) != 0 || observed["target-inventory.json"] != nil {
			return errors.New("unrequested target artifact evidence is present")
		}
		return nil
	}
	raw, exists := observed["target-inventory.json"]
	if !exists {
		return errors.New("requested target artifact inventory is absent")
	}
	if err := jobcontract.ValidateJSONDocument(raw, jobcontract.MaximumManifestBytes); err != nil {
		return err
	}
	var inventory artifactInventory
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return err
	}
	if inventory.Schema != "kenogram.target-artifact-inventory.v1" || inventory.Root != request.Artifacts.ContainerRoot || int64(len(inventory.Entries)) > request.Artifacts.MaxEntries || len(inventory.Entries) != len(manifestArtifacts) {
		return errors.New("target artifact inventory envelope is inconsistent")
	}
	prior := ""
	var total int64
	for _, artifact := range inventory.Entries {
		if artifact.Path <= prior || jobcontract.ValidateEvidenceRelativePath(artifact.Path) != nil || artifact.Size < 0 || artifact.Size > request.Artifacts.MaxBytes-total {
			return errors.New("target artifact inventory is invalid, duplicated, unordered, or oversized")
		}
		prior = artifact.Path
		total += artifact.Size
		entry, ok := manifestArtifacts["target-artifacts/"+artifact.Path]
		if !ok || entry.Size != artifact.Size || entry.SHA256 != artifact.SHA256 {
			return fmt.Errorf("target artifact %s disagrees with sealed evidence", artifact.Path)
		}
	}
	return nil
}

func verifyRegularDigest(root *os.Root, entry jobcontract.ManifestEntry) error {
	info, err := root.Lstat(entry.Path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
		return fmt.Errorf("target artifact %s is absent, changed, or not regular", entry.Path)
	}
	file, err := root.Open(entry.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("target artifact %s changed during open", entry.Path)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, entry.Size+1))
	if err != nil || written != entry.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("target artifact %s digest mismatch", entry.Path)
	}
	after, statErr := file.Stat()
	afterLookup, lookupErr := root.Lstat(entry.Path)
	if statErr != nil || lookupErr != nil || !afterLookup.Mode().IsRegular() || !os.SameFile(opened, after) || !os.SameFile(after, afterLookup) || opened.Size() != after.Size() {
		return fmt.Errorf("target artifact %s changed during read", entry.Path)
	}
	return nil
}

func maximumEvidenceEntry(entry jobcontract.ManifestEntry) int {
	maximum := entry.Size
	if maximum > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(maximum)
}

func readRegular(root *os.Root, name string, maximum int) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("evidence entry %s is not a regular file", name)
	}
	if info.Size() < 0 || info.Size() > int64(maximum) {
		return nil, fmt.Errorf("evidence entry %s exceeds its bound", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("evidence entry %s changed during open", name)
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maximum {
		return nil, fmt.Errorf("evidence entry %s exceeds its bound", name)
	}
	after, err := file.Stat()
	afterLookup, lookupErr := root.Lstat(name)
	if err != nil || lookupErr != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !afterLookup.Mode().IsRegular() || !os.SameFile(after, afterLookup) {
		return nil, fmt.Errorf("evidence entry %s changed during read", name)
	}
	return raw, nil
}

func verifyClosedInventory(root *os.Root, manifest jobcontract.Manifest) error {
	expected := map[string]bool{"manifest.json": true}
	expectedDirectories := map[string]bool{".": true}
	for _, entry := range manifest.Entries {
		expected[entry.Path] = true
		for directory := filepath.ToSlash(filepath.Dir(entry.Path)); directory != "."; directory = filepath.ToSlash(filepath.Dir(directory)) {
			expectedDirectories[directory] = true
		}
	}
	observed := []string{}
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if entry.IsDir() {
			if !expectedDirectories[path] {
				return fmt.Errorf("evidence directory contains undeclared directory %s", path)
			}
			return nil
		}
		observed = append(observed, strings.TrimPrefix(path, "./"))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(observed)
	if len(observed) != len(expected) {
		return errors.New("evidence directory contains an unsealed or missing entry")
	}
	for _, path := range observed {
		if !expected[path] {
			return fmt.Errorf("evidence directory contains undeclared entry %s", path)
		}
	}
	return nil
}

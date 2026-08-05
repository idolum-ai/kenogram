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
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/idolum-ai/kenogram/internal/decl"
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
	manifestRaw, err := readRegular(root, "manifest.json", int64(jobcontract.MaximumManifestBytes))
	if err != nil {
		return Verification{}, fmt.Errorf("read seal: %w", err)
	}
	manifest, err := jobcontract.ParseManifest(manifestRaw)
	if err != nil {
		return Verification{}, err
	}
	if contentDigest(manifest.Entries) != manifest.ContentSHA256 {
		return Verification{}, errors.New("manifest content root mismatch")
	}
	entries, artifacts, err := classifyManifest(manifest)
	if err != nil {
		return Verification{}, err
	}
	if err := verifyClosedInventory(root, manifest); err != nil {
		return Verification{}, err
	}
	requestRaw, err := readSealedEntry(root, entries["request.json"], int64(jobcontract.MaximumRequestBytes))
	if err != nil {
		return Verification{}, err
	}
	request, err := jobcontract.ParseRequest(requestRaw)
	if err != nil {
		return Verification{}, err
	}
	if err := validateArtifactManifestAuthority(request, entries, artifacts); err != nil {
		return Verification{}, err
	}
	limits := map[string]int64{
		"declaration.toml":    int64(jobcontract.MaximumRequestBytes),
		"plan.json":           int64(jobcontract.MaximumManifestBytes),
		"provenance.json":     int64(jobcontract.MaximumProvenanceBytes),
		"request.json":        int64(jobcontract.MaximumRequestBytes),
		"result.json":         int64(jobcontract.MaximumResultBytes),
		"runtime-before.json": int64(jobcontract.MaximumManifestBytes),
		"runtime-after.json":  int64(jobcontract.MaximumManifestBytes),
		"stdout.bin":          request.Limits.StdoutMaxBytes,
		"stderr.bin":          request.Limits.StderrMaxBytes,
	}
	if request.Artifacts != nil {
		limits["target-inventory.json"] = int64(jobcontract.MaximumManifestBytes)
	}
	observed := map[string][]byte{"request.json": requestRaw}
	for name, maximum := range limits {
		if name == "request.json" {
			continue
		}
		raw, err := readSealedEntry(root, entries[name], maximum)
		if err != nil {
			return Verification{}, err
		}
		observed[name] = raw
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
	declaration, err := decl.Parse(observed["declaration.toml"])
	if err != nil {
		return Verification{}, err
	}
	expectedPlan, err := plan.ProjectEvidence(declaration, request.Declaration.Path, observed["declaration.toml"], retainedPlan.Plan)
	if err != nil {
		return Verification{}, fmt.Errorf("re-project retained plan: %w", err)
	}
	if !reflect.DeepEqual(retainedPlan, expectedPlan) {
		return Verification{}, errors.New("retained public plan disagrees with declaration semantics")
	}
	if planDigest != prefixedDigest(retainedPlan.PlanDigest) || retainedPlan.PlanDigest != retainedPlan.EvidenceDigest || result.Identity.PlanSHA256 != planDigest {
		return Verification{}, errors.New("plan identity mismatch")
	}
	if err := validateSecretEnvironment(request, retainedPlan); err != nil {
		return Verification{}, err
	}
	if result.Identity.ImageReference != retainedPlan.Plan.World.Base {
		return Verification{}, errors.New("result image reference disagrees with retained plan")
	}
	if index := strings.LastIndex(retainedPlan.Plan.World.Base, "@sha256:"); index >= 0 && result.Identity.ImageDigest != "" && result.Identity.ImageDigest != retainedPlan.Plan.World.Base[index+1:] {
		if result.Status == "complete" {
			return Verification{}, errors.New("complete result carries a mismatched observed image digest")
		}
		if !slices.Contains(result.Reasons, "RUNTIME_START_OBSERVATION_FAILED") {
			return Verification{}, errors.New("image digest mismatch lacks runtime observation failure")
		}
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
	if request.Artifacts != nil {
		for _, entry := range artifacts {
			if err := verifyRegularDigest(root, entry, request.Artifacts.MaxBytes); err != nil {
				return Verification{}, err
			}
		}
	}
	if digest(observed["stdout.bin"]) != result.Stdout.SHA256 || int64(len(observed["stdout.bin"])) != result.Stdout.CapturedBytes ||
		digest(observed["stderr.bin"]) != result.Stderr.SHA256 || int64(len(observed["stderr.bin"])) != result.Stderr.CapturedBytes {
		return Verification{}, errors.New("stream evidence mismatch")
	}
	return Verification{Schema: "kenogram.job-verification.v1", JobID: manifest.JobID, Status: result.Status, Entries: len(manifest.Entries)}, nil
}

type evidenceKindBound struct {
	kind string
}

var mandatoryEvidenceKinds = map[string]evidenceKindBound{
	"declaration.toml":    {kind: "declaration"},
	"plan.json":           {kind: "plan"},
	"provenance.json":     {kind: "provenance"},
	"request.json":        {kind: "request"},
	"result.json":         {kind: "result"},
	"runtime-before.json": {kind: "runtime"},
	"runtime-after.json":  {kind: "runtime"},
	"stdout.bin":          {kind: "stdout"},
	"stderr.bin":          {kind: "stderr"},
}

func classifyManifest(manifest jobcontract.Manifest) (map[string]jobcontract.ManifestEntry, []jobcontract.ManifestEntry, error) {
	entries := make(map[string]jobcontract.ManifestEntry, len(manifest.Entries))
	artifacts := []jobcontract.ManifestEntry{}
	for _, entry := range manifest.Entries {
		if specification, ok := mandatoryEvidenceKinds[entry.Path]; ok {
			if entry.Kind != specification.kind {
				return nil, nil, fmt.Errorf("evidence entry %s has kind %q, want %q", entry.Path, entry.Kind, specification.kind)
			}
			entries[entry.Path] = entry
			continue
		}
		if entry.Path == "target-inventory.json" {
			if entry.Kind != "target_inventory" {
				return nil, nil, errors.New("target artifact inventory kind is invalid")
			}
			entries[entry.Path] = entry
			continue
		}
		if strings.HasPrefix(entry.Path, "target-artifacts/") && entry.Kind == "target_artifact" {
			artifacts = append(artifacts, entry)
			continue
		}
		return nil, nil, fmt.Errorf("evidence entry %s has an unknown path or kind", entry.Path)
	}
	for path := range mandatoryEvidenceKinds {
		if _, ok := entries[path]; !ok {
			return nil, nil, fmt.Errorf("mandatory evidence entry %s is absent", path)
		}
	}
	return entries, artifacts, nil
}

func validateArtifactManifestAuthority(request jobcontract.Request, entries map[string]jobcontract.ManifestEntry, artifacts []jobcontract.ManifestEntry) error {
	_, hasInventory := entries["target-inventory.json"]
	if request.Artifacts == nil {
		if hasInventory || len(artifacts) != 0 {
			return errors.New("unrequested target artifact evidence is present")
		}
		return nil
	}
	if !hasInventory {
		return errors.New("requested target artifact inventory is absent")
	}
	if int64(len(artifacts)) > request.Artifacts.MaxEntries {
		return errors.New("target artifact manifest exceeds requested entry bound")
	}
	var total int64
	for _, entry := range artifacts {
		if entry.Size < 0 || entry.Size > request.Artifacts.MaxBytes-total {
			return errors.New("target artifact manifest exceeds requested byte bound")
		}
		total += entry.Size
	}
	return nil
}

func readSealedEntry(root *os.Root, entry jobcontract.ManifestEntry, maximum int64) ([]byte, error) {
	if maximum < 0 || entry.Size < 0 || entry.Size > maximum {
		return nil, fmt.Errorf("evidence entry %s exceeds its semantic bound", entry.Path)
	}
	raw, err := readRegular(root, entry.Path, maximum)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != entry.Size || digest(raw) != entry.SHA256 {
		return nil, fmt.Errorf("evidence entry %s has changed", entry.Path)
	}
	return raw, nil
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

func verifyRegularDigest(root *os.Root, entry jobcontract.ManifestEntry, maximum int64) error {
	if entry.Size < 0 || entry.Size > maximum {
		return fmt.Errorf("target artifact %s exceeds its semantic bound", entry.Path)
	}
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

func readRegular(root *os.Root, name string, maximum int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("evidence entry %s is not a regular file", name)
	}
	if maximum < 0 || info.Size() < 0 || info.Size() > maximum {
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
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximum {
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
	visited := 0
	maximumVisited := len(expected) + len(expectedDirectories)
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		visited++
		if visited > maximumVisited {
			return errors.New("evidence inventory traversal exceeds sealed bounds")
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

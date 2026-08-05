package jobcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
)

const RuntimeLifecycleSource = "kenogram-lifecycle:v1"

// RuntimeMountSource derives the durable semantic source reference for one
// runtime mount. Ephemeral scratch paths never become retained authority.
func RuntimeMountSource(role, target, mode, authoritySource, contentDigest string) (string, error) {
	switch role {
	case "declared":
		if !filepath.IsAbs(authoritySource) || filepath.Clean(authoritySource) != authoritySource {
			return "", errors.New("declared mount authority source is invalid")
		}
		if mode == "rw" && contentDigest == "" {
			return authoritySource, nil
		}
		if mode == "ro" && runtimeHexDigest.MatchString(contentDigest) {
			return "kenogram-snapshot:" + contentDigest, nil
		}
	case "helper":
		if mode == "ro" && authoritySource == "" && runtimeHexDigest.MatchString(contentDigest) {
			return "kenogram-snapshot:" + contentDigest, nil
		}
	case "workspace":
		if mode == "rw" && authoritySource == "" && filepath.IsAbs(target) && filepath.Clean(target) == target && contentDigest == "" {
			sum := sha256.Sum256([]byte(target))
			return "kenogram-workspace:sha256:" + hex.EncodeToString(sum[:]), nil
		}
	case "lifecycle":
		if mode == "rw" && authoritySource == "" && contentDigest == "" {
			return RuntimeLifecycleSource, nil
		}
	}
	return "", errors.New("runtime mount role, mode, authority, and digest are inconsistent")
}

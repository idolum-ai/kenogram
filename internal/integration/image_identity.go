package integration

import (
	"fmt"
	"strings"
)

// canonicalPodmanImageID accepts the two exact forms emitted by supported
// Podman image-inspect versions. It normalizes a bare lowercase hexadecimal
// ID without weakening the immutable sha256 identity contract.
func canonicalPodmanImageID(value string) (string, error) {
	digest := value
	if strings.HasPrefix(digest, "sha256:") {
		digest = strings.TrimPrefix(digest, "sha256:")
	}
	if len(digest) != 64 {
		return "", fmt.Errorf("Podman image ID %q is not a canonical sha256 digest", value)
	}
	for _, character := range digest {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return "", fmt.Errorf("Podman image ID %q is not a canonical sha256 digest", value)
		}
	}
	return "sha256:" + digest, nil
}

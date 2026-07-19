// Package naming defines the bounded identifiers that become host paths,
// container names, labels, and generated filenames.
package naming

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var operational = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// World validates a world name before it is used as a host path component.
func World(value string) error { return validate("world", value) }

// Service validates a service name before it is used as a generated filename.
func Service(value string) error { return validate("service", value) }

// Interface validates an operator-facing interface name.
func Interface(value string) error { return validate("interface", value) }

// Host validates an exact, non-wildcard network name or IP address.
func Host(value string) error {
	if value == "" || !utf8.ValidString(value) {
		return fmt.Errorf("invalid host %q: use an exact non-wildcard name or address", value)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune("*/\\@?#[]", r) {
			return fmt.Errorf("invalid host %q: use an exact non-wildcard name or address", value)
		}
	}
	if strings.Contains(value, ":") && net.ParseIP(value) == nil {
		return fmt.Errorf("invalid host %q: use an exact non-wildcard name or address", value)
	}
	return nil
}

func validate(kind, value string) error {
	if !operational.MatchString(value) || value == "." || value == ".." {
		return fmt.Errorf("invalid %s name %q: use 1-63 lowercase ASCII letters, digits, dots, underscores, or hyphens; start with a letter or digit", kind, value)
	}
	return nil
}

// JoinUnder joins a relative name beneath root and verifies lexical
// containment. Callers must still validate the name's domain syntax.
func JoinUnder(root, relative string) (string, error) {
	joined := filepath.Join(root, relative)
	rel, err := filepath.Rel(root, joined)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes root %q", relative, root)
	}
	return joined, nil
}

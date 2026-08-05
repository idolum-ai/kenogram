// Package mountpath validates paths embedded in Podman's --mount grammar.
package mountpath

import (
	"errors"
	"path/filepath"
	"strings"
)

// Validate rejects bytes that can be reinterpreted as a mount field separator,
// assignment, escape, or quote.
func Validate(value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, ",=\\\"'\x00\r\n") {
		return errors.New("path is not an unambiguous absolute --mount value")
	}
	return nil
}

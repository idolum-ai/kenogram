package decl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-fA-F]{64}$`)

// Validate checks schema constraints that depend on values and host metadata.
func Validate(d Declaration, declarationDir string) error {
	if d.Version != 1 {
		return fmt.Errorf("version must be 1, got %d", d.Version)
	}
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("name must not be empty")
	}
	if strings.ContainsAny(d.Name, "/\\") {
		return fmt.Errorf("name must not contain a path separator")
	}
	if d.World.Hostname == "" || d.World.Base == "" || d.World.User == "" {
		return fmt.Errorf("world hostname, base, and user must not be empty")
	}
	if !pinnedImage.MatchString(d.World.Base) && !d.AllowUnpinned {
		return fmt.Errorf("world.base must be pinned by sha256 digest or allow_unpinned must be true")
	}
	if err := absoluteClean("world.workdir", d.World.Workdir); err != nil {
		return err
	}
	if d.Resources.CPUs <= 0 || d.Resources.MemoryBytes <= 0 || d.Resources.PIDs <= 0 {
		return fmt.Errorf("resource values must be positive")
	}
	if len(d.Workspace.Paths) == 0 {
		return fmt.Errorf("workspace.paths must not be empty")
	}
	seenPaths := map[string]bool{}
	for i, path := range d.Workspace.Paths {
		if err := absoluteClean(fmt.Sprintf("workspace.paths[%d]", i), path); err != nil {
			return err
		}
		if seenPaths[path] {
			return fmt.Errorf("duplicate workspace path %q", path)
		}
		seenPaths[path] = true
	}
	for i, c := range d.Copies {
		if err := sourceExists(declarationDir, c.Source); err != nil {
			return fmt.Errorf("copies[%d]: %w", i, err)
		}
		if err := absoluteClean(fmt.Sprintf("copies[%d].target", i), c.Target); err != nil {
			return err
		}
		if err := fileMode(c.Mode); err != nil {
			return fmt.Errorf("copies[%d].mode: %w", i, err)
		}
		if reservedOverlap(c.Target) {
			return fmt.Errorf("copies[%d].target %q overlaps a reserved path", i, c.Target)
		}
		if c.Secret {
			info, err := os.Stat(resolveSource(declarationDir, c.Source))
			if err != nil {
				return fmt.Errorf("copies[%d]: stat secret: %w", i, err)
			}
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("copies[%d].source secret permissions %04o grant group or other access", i, info.Mode().Perm())
			}
		}
	}
	for i, m := range d.Mounts {
		if err := sourceExists(declarationDir, m.Source); err != nil {
			return fmt.Errorf("mounts[%d]: %w", i, err)
		}
		if err := absoluteClean(fmt.Sprintf("mounts[%d].target", i), m.Target); err != nil {
			return err
		}
		if m.Mode != "ro" && m.Mode != "rw" {
			return fmt.Errorf("mounts[%d].mode must be ro or rw", i)
		}
		if reservedOverlap(m.Target) {
			return fmt.Errorf("mounts[%d].target %q overlaps a reserved path", i, m.Target)
		}
		for j := 0; j < i; j++ {
			if pathsOverlap(m.Target, d.Mounts[j].Target) {
				return fmt.Errorf("mount targets %q and %q overlap", d.Mounts[j].Target, m.Target)
			}
		}
	}
	seenNetwork := map[string]bool{}
	for i, allow := range d.Network.Allow {
		if strings.TrimSpace(allow.Host) == "" || strings.ContainsAny(allow.Host, "*/ ") {
			return fmt.Errorf("network.allow[%d].host must be an exact non-wildcard name or address", i)
		}
		if allow.Port < 1 || allow.Port > 65535 {
			return fmt.Errorf("network.allow[%d].port must be between 1 and 65535", i)
		}
		key := strings.ToLower(allow.Host) + ":" + strconv.FormatInt(allow.Port, 10)
		if seenNetwork[key] {
			return fmt.Errorf("duplicate network allowance %s", key)
		}
		seenNetwork[key] = true
	}
	seenServices := map[string]bool{}
	for i, service := range d.Services {
		if service.Name == "" {
			return fmt.Errorf("services[%d].name must not be empty", i)
		}
		if seenServices[service.Name] {
			return fmt.Errorf("duplicate service name %q", service.Name)
		}
		seenServices[service.Name] = true
		if len(service.Command) == 0 || service.Command[0] == "" {
			return fmt.Errorf("services[%d].command must not be empty", i)
		}
		switch service.Restart {
		case "never", "on-failure", "always":
		default:
			return fmt.Errorf("services[%d].restart must be never, on-failure, or always", i)
		}
	}
	return nil
}

func resolveSource(dir, source string) string {
	if filepath.IsAbs(source) {
		return filepath.Clean(source)
	}
	return filepath.Join(dir, source)
}
func sourceExists(dir, source string) error {
	if source == "" {
		return fmt.Errorf("source must not be empty")
	}
	_, err := os.Lstat(resolveSource(dir, source))
	if err != nil {
		return fmt.Errorf("source %q: %w", source, err)
	}
	return nil
}
func absoluteClean(label, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be absolute", label)
	}
	if filepath.Clean(path) != path {
		return fmt.Errorf("%s must be clean", label)
	}
	return nil
}
func fileMode(mode string) error {
	if len(mode) != 4 || mode[0] != '0' {
		return fmt.Errorf("must be four octal digits such as 0700")
	}
	n, err := strconv.ParseUint(mode, 8, 12)
	if err != nil || n > 0o777 {
		return fmt.Errorf("must be four octal digits such as 0700")
	}
	return nil
}
func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}
func reservedOverlap(target string) bool {
	return pathsOverlap(target, "/KENOGRAM.md") || pathsOverlap(target, "/etc/kenogram")
}

//go:build !darwin

package decl

func canonicalPlatformPath(path string) string { return path }

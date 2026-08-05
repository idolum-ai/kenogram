//go:build darwin

package decl

import "strings"

func canonicalPlatformPath(path string) string {
	for alias, canonical := range map[string]string{
		"/etc": "/private/etc",
		"/tmp": "/private/tmp",
		"/var": "/private/var",
	} {
		if path == alias {
			return canonical
		}
		if strings.HasPrefix(path, alias+"/") {
			return canonical + strings.TrimPrefix(path, alias)
		}
	}
	return path
}

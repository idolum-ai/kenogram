//go:build !linux && !darwin

package doctor

import "fmt"

func diskFree(path string) (uint64, error) {
	return 0, fmt.Errorf("disk observation unsupported for %s", path)
}

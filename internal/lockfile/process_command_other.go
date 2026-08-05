//go:build !linux && !darwin

package lockfile

import "fmt"

func ProcessCommand(int) (string, error) {
	return "", fmt.Errorf("process command observation is unavailable")
}

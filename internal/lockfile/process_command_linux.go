//go:build linux

package lockfile

import (
	"fmt"
	"os"
)

func ProcessCommand(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return string(raw), err
}

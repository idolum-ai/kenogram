//go:build linux

package lockfile

import (
	"fmt"
	"os"
)

// ProcessStart returns the Linux process start-time field used to distinguish
// a live process from a later process that reused its PID.
func ProcessStart(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	return ProcessStartFromStat(string(raw))
}

//go:build darwin

package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strconv"
	"strings"
)

// ProcessStart uses Darwin's native ps observation. lstart has whole-second
// resolution, which is sufficient to reject a later PID occupant; an absent or
// zombie process fails closed.
func ProcessStart(pid int) string {
	if pid <= 0 {
		return ""
	}
	raw, err := exec.Command("/bin/ps", "-o", "stat=", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 6 || strings.HasPrefix(fields[0], "Z") {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(fields[1:], " ")))
	return hex.EncodeToString(sum[:])
}

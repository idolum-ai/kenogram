//go:build darwin

package lockfile

import (
	"os/exec"
	"strconv"
	"strings"
)

func ProcessCommand(pid int) (string, error) {
	raw, err := exec.Command("/bin/ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(raw)), err
}

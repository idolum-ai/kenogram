//go:build linux

package backend

import (
	"os"
	"testing"
)

func TestIPCNamespaceIsolatedFromHostRejectsSelfAndInvalidPID(t *testing.T) {
	isolated, err := ipcNamespaceIsolatedFromHost(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if isolated {
		t.Fatal("current process reported isolated from its own IPC namespace")
	}
	for _, pid := range []int{0, -1, 1 << 30} {
		if _, err := ipcNamespaceIsolatedFromHost(pid); err == nil {
			t.Fatalf("PID %d accepted", pid)
		}
	}
}

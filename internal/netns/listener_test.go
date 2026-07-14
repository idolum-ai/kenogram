package netns

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/lockfile"
)

func TestParseHelperArgs(t *testing.T) {
	fd, address, err := ParseHelperArgs([]string{"--control-fd", "3", "--address", "127.0.0.1:3128"})
	if err != nil || fd != 3 || address != "127.0.0.1:3128" {
		t.Fatalf("fd=%d address=%q err=%v", fd, address, err)
	}
	if _, _, err := ParseHelperArgs(nil); err == nil {
		t.Fatal("empty accepted")
	}
}

func TestAcquireConnectionRejectsChangedProcessIdentityBeforeRevalidation(t *testing.T) {
	called := false
	started := time.Now()
	_, err := AcquireConnection(context.Background(), os.Getpid(), lockfile.ProcessStart(os.Getpid())+"-wrong", "127.0.0.1:1", func() error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("identity mismatch = %v", err)
	}
	if called {
		t.Fatal("runtime was revalidated after namespace identity mismatch")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("identity mismatch took %s", elapsed)
	}
}

func TestReceivedConnectionDescriptorIsCloseOnExec(t *testing.T) {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(pair[0])
	defer syscall.Close(pair[1])
	source, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := syscall.Sendmsg(pair[1], []byte{1}, syscall.UnixRights(int(source.Fd())), nil, 0); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1)
	control := make([]byte, syscall.CmsgSpace(4))
	_, controlN, _, err := receiveControlFD(pair[0], payload, control)
	if err != nil {
		t.Fatal(err)
	}
	fds, err := receivedDescriptors(control[:controlN])
	if err != nil || len(fds) != 1 {
		t.Fatalf("fds=%v err=%v", fds, err)
	}
	defer closeDescriptors(fds)
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fds[0]), uintptr(syscall.F_GETFD), 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor flags=%#x errno=%v", flags, errno)
	}
}

func TestConnectionControlRejectsAndClosesMultipleDescriptors(t *testing.T) {
	first, err := syscall.Open("/dev/null", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := syscall.Open("/dev/null", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		syscall.Close(first)
		t.Fatal(err)
	}
	message := controlMessage{payload: []byte{1}, control: syscall.UnixRights(first, second)}
	if _, err := connectionFromControl(message, nil, ""); err == nil || !strings.Contains(err.Error(), "want 1") {
		t.Fatalf("multiple descriptors = %v", err)
	}
	for _, fd := range []int{first, second} {
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0); errno != syscall.EBADF {
			t.Fatalf("descriptor %d remained open: %v", fd, errno)
		}
	}
}

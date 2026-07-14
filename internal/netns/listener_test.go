package netns

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
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

func TestNamespaceHelperCancellationIsBoundedReapedAndFDStable(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		want    error
	}{
		{name: "cancel", context: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			return ctx, cancel
		}, want: context.Canceled},
		{name: "timeout", context: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 20*time.Millisecond)
		}, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseline := openFDCount(t)
			parent, child := controlPair(t)
			ctx, cancel := test.context()
			defer cancel()
			command := exec.CommandContext(ctx, "sleep", "30")
			var stderr bytes.Buffer
			command.Stderr = &stderr
			started := time.Now()
			connection, err := runNamespaceConnectionHelper(ctx, command, parent, child, &stderr)
			if connection != nil {
				connection.Close()
				t.Fatal("cancelled helper returned a connection")
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("helper error = %v, want %v", err, test.want)
			}
			if time.Since(started) > time.Second {
				t.Fatalf("helper cancellation was not prompt: %s", time.Since(started))
			}
			if command.ProcessState == nil {
				t.Fatal("helper process was not reaped")
			}
			parent.Close()
			child.Close()
			if got := openFDCount(t); got != baseline {
				t.Fatalf("open descriptors after cancellation = %d, want %d", got, baseline)
			}
		})
	}
}

func TestNamespaceHelperEarlyFailurePreservesDiagnosticAndFDs(t *testing.T) {
	baseline := openFDCount(t)
	for iteration := 0; iteration < 3; iteration++ {
		parent, child := controlPair(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		command := exec.CommandContext(ctx, "sh", "-c", "printf 'early helper failure\\n' >&2; exit 7")
		var stderr bytes.Buffer
		command.Stderr = &stderr
		connection, err := runNamespaceConnectionHelper(ctx, command, parent, child, &stderr)
		cancel()
		if connection != nil {
			connection.Close()
			t.Fatal("failed helper returned a connection")
		}
		if err == nil || !strings.Contains(err.Error(), "early helper failure") {
			t.Fatalf("early helper error = %v", err)
		}
		if command.ProcessState == nil {
			t.Fatal("failed helper process was not reaped")
		}
		parent.Close()
		child.Close()
		if got := openFDCount(t); got != baseline {
			t.Fatalf("iteration %d left %d descriptors, want %d", iteration, got, baseline)
		}
	}
}

func TestSendConnectionTransfersBidirectionalTCPDescriptorWithoutLeaks(t *testing.T) {
	baseline := openFDCount(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("sandbox does not permit loopback sockets")
		}
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer connection.Close()
		payload := make([]byte, 4)
		if _, readErr := io.ReadFull(connection, payload); readErr != nil {
			serverDone <- readErr
			return
		}
		if string(payload) != "ping" {
			serverDone <- errors.New("transferred connection changed upload")
			return
		}
		_, writeErr := connection.Write([]byte("pong"))
		serverDone <- writeErr
	}()
	parent, child := controlPair(t)
	if err := parent.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() { sent <- SendConnection(int(child.Fd()), listener.Addr().String()) }()
	message := receiveControl(parent)
	sendErr := <-sent
	connection, err := connectionFromControl(message, sendErr, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(connection, reply); err != nil {
		t.Fatal(err)
	}
	if string(reply) != "pong" {
		t.Fatalf("reply = %q", reply)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	connection.Close()
	parent.Close()
	child.Close()
	listener.Close()
	if got := openFDCount(t); got != baseline {
		t.Fatalf("round trip left %d descriptors, want %d", got, baseline)
	}
}

func TestSendConnectionReturnsDialDiagnosticWithoutDescriptorLeaks(t *testing.T) {
	baseline := openFDCount(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("sandbox does not permit loopback sockets")
		}
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	parent, child := controlPair(t)
	if err := parent.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() { sent <- SendConnection(int(child.Fd()), address) }()
	message := receiveControl(parent)
	sendErr := <-sent
	connection, transferErr := connectionFromControl(message, sendErr, "")
	if connection != nil {
		connection.Close()
		t.Fatal("failed dial transferred a connection")
	}
	if transferErr == nil || !strings.Contains(transferErr.Error(), "connect") {
		t.Fatalf("dial diagnostic = %v", transferErr)
	}
	parent.Close()
	child.Close()
	if got := openFDCount(t); got != baseline {
		t.Fatalf("failed dial left %d descriptors, want %d", got, baseline)
	}
}

func controlPair(t *testing.T) (*net.UnixConn, *os.File) {
	t.Helper()
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parentFile := os.NewFile(uintptr(pair[0]), "test-control-parent")
	child := os.NewFile(uintptr(pair[1]), "test-control-child")
	generic, err := net.FileConn(parentFile)
	parentFile.Close()
	if err != nil {
		child.Close()
		if errors.Is(err, syscall.EPERM) {
			t.Skip("sandbox does not permit Unix socket inspection")
		}
		t.Fatal(err)
	}
	parent, ok := generic.(*net.UnixConn)
	if !ok {
		generic.Close()
		child.Close()
		t.Fatal("control socket is not Unix")
	}
	return parent, child
}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

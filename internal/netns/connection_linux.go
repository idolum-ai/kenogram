//go:build linux

package netns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idolum-ai/kenogram/internal/lockfile"
)

const connectionTransferTimeout = 15 * time.Second

// AcquireConnection pins the inspected process's namespace descriptors before
// asking the caller to revalidate runtime authority. nsenter receives those
// descriptors, never fresh /proc/<pid>/ns paths that could follow PID reuse.
func AcquireConnection(ctx context.Context, pid int, processStart, address string, revalidate func() error) (net.Conn, error) {
	helperContext, cancelHelper := context.WithTimeout(ctx, connectionTransferTimeout)
	defer cancelHelper()
	userNS, networkNS, err := pinNamespaces(pid, processStart)
	if err != nil {
		return nil, err
	}
	defer userNS.Close()
	defer networkNS.Close()
	if revalidate == nil {
		return nil, fmt.Errorf("runtime revalidation is required")
	}
	if err := revalidate(); err != nil {
		return nil, fmt.Errorf("revalidate pinned runtime: %w", err)
	}

	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(pair[0]), "netns-connect-parent")
	child := os.NewFile(uintptr(pair[1]), "netns-connect-child")
	defer parentFile.Close()
	defer child.Close()
	genericParent, err := net.FileConn(parentFile)
	if err != nil {
		return nil, fmt.Errorf("open namespace control connection: %w", err)
	}
	parent, ok := genericParent.(*net.UnixConn)
	if !ok {
		genericParent.Close()
		return nil, fmt.Errorf("namespace control connection is not Unix")
	}
	defer parent.Close()
	if err := parent.SetReadDeadline(time.Now().Add(connectionTransferTimeout)); err != nil {
		return nil, fmt.Errorf("set namespace control deadline: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(helperContext, "nsenter",
		"--user=/proc/self/fd/4", "--net=/proc/self/fd/5",
		"--preserve-credentials", "--", executable, "_netns-connect",
		"--control-fd", "3", "--address", address)
	command.ExtraFiles = []*os.File{child, userNS, networkNS}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start namespace connection helper: %w", err)
	}
	_ = child.Close()

	received := make(chan controlMessage, 1)
	go func() { received <- receiveControl(parent) }()
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	var message controlMessage
	var waitErr error
	select {
	case message = <-received:
		select {
		case waitErr = <-waited:
		case <-helperContext.Done():
			_ = command.Process.Kill()
			waitErr = <-waited
			closeReceivedDescriptors(message.control)
			return nil, context.Cause(helperContext)
		}
	case waitErr = <-waited:
		if waitErr != nil {
			_ = parent.Close()
			message = <-received
			closeReceivedDescriptors(message.control)
			return nil, namespaceHelperError(waitErr, stderr.String(), message.payload)
		}
		message = <-received
	case <-helperContext.Done():
		_ = parent.Close()
		_ = command.Process.Kill()
		waitErr = <-waited
		message = <-received
		closeReceivedDescriptors(message.control)
		return nil, context.Cause(helperContext)
	}
	if message.err != nil {
		closeReceivedDescriptors(message.control)
		return nil, namespaceHelperError(waitErr, stderr.String(), message.payload)
	}
	return connectionFromControl(message, waitErr, stderr.String())
}

type controlMessage struct {
	payload []byte
	control []byte
	flags   int
	err     error
}

func receiveControl(connection *net.UnixConn) controlMessage {
	message := controlMessage{payload: make([]byte, 1024), control: make([]byte, syscall.CmsgSpace(4*4))}
	raw, err := connection.SyscallConn()
	if err != nil {
		message.err = err
		return message
	}
	var n, controlN int
	err = raw.Read(func(fd uintptr) bool {
		var recvErr error
		n, controlN, message.flags, recvErr = receiveControlFD(int(fd), message.payload, message.control)
		if errors.Is(recvErr, syscall.EAGAIN) || errors.Is(recvErr, syscall.EWOULDBLOCK) {
			return false
		}
		message.err = recvErr
		return true
	})
	if message.err == nil && err != nil {
		message.err = err
	}
	message.payload = message.payload[:n]
	message.control = message.control[:controlN]
	return message
}

func receiveControlFD(fd int, payload, control []byte) (int, int, int, error) {
	n, controlN, flags, _, err := syscall.Recvmsg(fd, payload, control, syscall.MSG_CMSG_CLOEXEC)
	return n, controlN, flags, err
}

func connectionFromControl(message controlMessage, waitErr error, stderr string) (net.Conn, error) {
	fds, parseErr := receivedDescriptors(message.control)
	if parseErr != nil {
		closeDescriptors(fds)
		return nil, fmt.Errorf("parse namespace connection descriptor: %w", parseErr)
	}
	fail := func(err error) (net.Conn, error) {
		closeDescriptors(fds)
		return nil, err
	}
	if message.flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 {
		return fail(fmt.Errorf("namespace connection response was truncated"))
	}
	if waitErr != nil {
		return fail(namespaceHelperError(waitErr, stderr, message.payload))
	}
	if len(message.payload) != 1 || message.payload[0] != 1 {
		return fail(namespaceHelperError(nil, stderr, message.payload))
	}
	if len(fds) != 1 {
		return fail(fmt.Errorf("namespace helper returned %d connection descriptors, want 1", len(fds)))
	}
	file := os.NewFile(uintptr(fds[0]), "world-connection")
	defer file.Close()
	connection, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func receivedDescriptors(control []byte) ([]int, error) {
	messages, err := syscall.ParseSocketControlMessage(control)
	if err != nil {
		return nil, err
	}
	fds := []int{}
	for _, message := range messages {
		rights, err := syscall.ParseUnixRights(&message)
		if err != nil {
			return fds, err
		}
		fds = append(fds, rights...)
	}
	return fds, nil
}

func closeReceivedDescriptors(control []byte) {
	fds, _ := receivedDescriptors(control)
	closeDescriptors(fds)
}

func closeDescriptors(fds []int) {
	for _, fd := range fds {
		_ = syscall.Close(fd)
	}
}

func namespaceHelperError(waitErr error, stderr string, payload []byte) error {
	detail := strings.TrimSpace(string(payload))
	if text := strings.TrimSpace(stderr); text != "" {
		detail = text
	}
	if detail != "" {
		return fmt.Errorf("namespace connection helper: %s", detail)
	}
	if waitErr != nil {
		return fmt.Errorf("namespace connection helper: %w", waitErr)
	}
	return fmt.Errorf("namespace connection helper returned no connection")
}

func pinNamespaces(pid int, expectedStart string) (*os.File, *os.File, error) {
	if pid <= 0 || expectedStart == "" {
		return nil, nil, fmt.Errorf("runtime process identity is required")
	}
	proc, err := os.Open(filepathProc(pid))
	if err != nil {
		return nil, nil, fmt.Errorf("open runtime process: %w", err)
	}
	defer proc.Close()
	actual, err := processStartAt(int(proc.Fd()))
	if err != nil {
		return nil, nil, err
	}
	if actual != expectedStart {
		return nil, nil, fmt.Errorf("runtime process identity changed before namespace pin")
	}
	userNS, err := openAt(int(proc.Fd()), "ns/user", "world-user-namespace")
	if err != nil {
		return nil, nil, fmt.Errorf("pin runtime user namespace: %w", err)
	}
	networkNS, err := openAt(int(proc.Fd()), "ns/net", "world-network-namespace")
	if err != nil {
		userNS.Close()
		return nil, nil, fmt.Errorf("pin runtime network namespace: %w", err)
	}
	actual, err = processStartAt(int(proc.Fd()))
	if err != nil || actual != expectedStart {
		userNS.Close()
		networkNS.Close()
		return nil, nil, fmt.Errorf("runtime process identity changed while pinning namespaces")
	}
	return userNS, networkNS, nil
}

func processStartAt(procFD int) (string, error) {
	stat, err := openAt(procFD, "stat", "runtime-process-stat")
	if err != nil {
		return "", fmt.Errorf("read runtime process identity: %w", err)
	}
	defer stat.Close()
	raw, err := io.ReadAll(io.LimitReader(stat, 8192))
	if err != nil {
		return "", fmt.Errorf("read runtime process identity: %w", err)
	}
	start := lockfile.ProcessStartFromStat(string(raw))
	if start == "" {
		return "", fmt.Errorf("runtime process identity is absent")
	}
	return start, nil
}

func openAt(directoryFD int, path, name string) (*os.File, error) {
	fd, err := syscall.Openat(directoryFD, path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func filepathProc(pid int) string { return "/proc/" + strconv.Itoa(pid) }

// SendConnection runs inside the pinned target namespaces and transfers one
// TCP connection, or a bounded error message, to the host process.
func SendConnection(controlFD int, address string) error {
	control := os.NewFile(uintptr(controlFD), "netns-connect-control")
	if control == nil {
		return fmt.Errorf("invalid control fd")
	}
	defer control.Close()
	connection, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		detail := []byte(err.Error())
		if len(detail) > 1024 {
			detail = detail[:1024]
		}
		_ = syscall.Sendmsg(int(control.Fd()), detail, nil, nil, 0)
		return err
	}
	defer connection.Close()
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("world interface is not TCP")
	}
	file, err := tcp.File()
	if err != nil {
		return err
	}
	defer file.Close()
	return syscall.Sendmsg(int(control.Fd()), []byte{1}, syscall.UnixRights(int(file.Fd())), nil, 0)
}

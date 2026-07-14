//go:build linux

package netns

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// AcquireConnection asks a short-lived helper in the exact user and network
// namespaces of pid to dial address, then receives the connected descriptor.
func AcquireConnection(ctx context.Context, pid int, address string) (net.Conn, error) {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parent := os.NewFile(uintptr(pair[0]), "netns-connect-parent")
	child := os.NewFile(uintptr(pair[1]), "netns-connect-child")
	defer parent.Close()
	defer child.Close()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "nsenter",
		fmt.Sprintf("--user=/proc/%d/ns/user", pid),
		fmt.Sprintf("--net=/proc/%d/ns/net", pid),
		"--preserve-credentials", "--", executable, "_netns-connect",
		"--control-fd", "3", "--address", address)
	command.ExtraFiles = []*os.File{child}
	done := make(chan error, 1)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start namespace connection helper: %w", err)
	}
	child.Close()
	go func() { done <- command.Wait() }()
	timeval := syscall.NsecToTimeval((15 * time.Second).Nanoseconds())
	_ = syscall.SetsockoptTimeval(pair[0], syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeval)
	message := make([]byte, 1024)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, recvErr := syscall.Recvmsg(pair[0], message, oob, 0)
	if recvErr != nil {
		_ = command.Process.Kill()
		<-done
		return nil, fmt.Errorf("receive namespace connection: %w", recvErr)
	}
	waitErr := <-done
	control, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	if len(control) != 1 {
		detail := string(message[:n])
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		return nil, fmt.Errorf("namespace connection helper: %s", detail)
	}
	fds, err := syscall.ParseUnixRights(&control[0])
	if err != nil || len(fds) != 1 {
		return nil, fmt.Errorf("parse namespace connection descriptor: %w", err)
	}
	if waitErr != nil {
		_ = syscall.Close(fds[0])
		return nil, fmt.Errorf("namespace connection helper: %w", waitErr)
	}
	file := os.NewFile(uintptr(fds[0]), "world-connection")
	defer file.Close()
	connection, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

// SendConnection runs inside the target namespaces and transfers one TCP
// connection, or a bounded error message, to the host process.
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

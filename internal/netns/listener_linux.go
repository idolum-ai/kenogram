//go:build linux

// Package netns transfers a listener created in a world's network namespace
// back to the host-side proxy process.
package netns

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func AcquireListener(ctx context.Context, pid int, address string) (net.Listener, error) {
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parent := os.NewFile(uintptr(pair[0]), "netns-parent")
	child := os.NewFile(uintptr(pair[1]), "netns-child")
	defer parent.Close()
	defer child.Close()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	userNS, netNS := fmt.Sprintf("/proc/%d/ns/user", pid), fmt.Sprintf("/proc/%d/ns/net", pid)
	command := exec.CommandContext(ctx, "nsenter", "--user="+userNS, "--net="+netNS, "--preserve-credentials", "--", executable, "_netns-listener", "--control-fd", "3", "--address", address)
	command.ExtraFiles = []*os.File{child}
	output := make(chan error, 1)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start namespace listener helper: %w", err)
	}
	child.Close()
	go func() { output <- command.Wait() }()
	timeval := syscall.NsecToTimeval((15 * time.Second).Nanoseconds())
	_ = syscall.SetsockoptTimeval(pair[0], syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeval)
	buffer := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(4))
	_, oobn, _, _, recvErr := syscall.Recvmsg(pair[0], buffer, oob, 0)
	if recvErr != nil {
		command.Process.Kill()
		<-output
		return nil, fmt.Errorf("receive namespace listener: %w", recvErr)
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	if len(messages) != 1 {
		return nil, fmt.Errorf("namespace helper returned no listener")
	}
	fds, err := syscall.ParseUnixRights(&messages[0])
	if err != nil || len(fds) != 1 {
		return nil, fmt.Errorf("parse namespace listener descriptor: %w", err)
	}
	if err := <-output; err != nil {
		syscall.Close(fds[0])
		return nil, fmt.Errorf("namespace listener helper: %w", err)
	}
	file := os.NewFile(uintptr(fds[0]), "world-listener")
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

func SendListener(controlFD int, address string) error {
	control := os.NewFile(uintptr(controlFD), "netns-control")
	if control == nil {
		return fmt.Errorf("invalid control fd")
	}
	defer control.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("bind door: %w", err)
	}
	defer listener.Close()
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return fmt.Errorf("door is not TCP")
	}
	file, err := tcp.File()
	if err != nil {
		return err
	}
	defer file.Close()
	rights := syscall.UnixRights(int(file.Fd()))
	if err := syscall.Sendmsg(int(control.Fd()), []byte{1}, rights, nil, 0); err != nil {
		return err
	}
	return nil
}

func ParseHelperArgs(args []string) (int, string, error) {
	fd := 0
	address := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--control-fd":
			i++
			if i >= len(args) {
				return 0, "", fmt.Errorf("missing control fd")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return 0, "", err
			}
			fd = n
		case "--address":
			i++
			if i >= len(args) {
				return 0, "", fmt.Errorf("missing address")
			}
			address = args[i]
		default:
			return 0, "", fmt.Errorf("unknown helper argument %q", args[i])
		}
	}
	if fd < 3 || address == "" {
		return 0, "", fmt.Errorf("control fd and address are required")
	}
	return fd, address, nil
}

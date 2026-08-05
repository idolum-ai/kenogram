//go:build linux

// Package netns transfers a listener created in a world's network namespace
// back to the host-side proxy process.
package netns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// AcquireBoundListener pins the exact inspected process's user and network
// namespace descriptors before caller revalidation. The helper receives those
// descriptors, never fresh /proc paths that could follow PID reuse.
func AcquireBoundListener(ctx context.Context, pid int, processStart, address string, revalidate func() error) (net.Listener, NamespaceIdentity, error) {
	helperContext, cancel := context.WithTimeout(ctx, connectionTransferTimeout)
	defer cancel()
	userNS, networkNS, err := pinNamespaces(pid, processStart)
	if err != nil {
		return nil, NamespaceIdentity{}, err
	}
	defer userNS.Close()
	defer networkNS.Close()
	identity, err := namespaceIdentity(userNS, networkNS)
	if err != nil {
		return nil, NamespaceIdentity{}, err
	}
	if revalidate == nil {
		return nil, NamespaceIdentity{}, errors.New("runtime revalidation is required")
	}
	if err := revalidate(); err != nil {
		return nil, NamespaceIdentity{}, fmt.Errorf("revalidate pinned runtime: %w", err)
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, NamespaceIdentity{}, fmt.Errorf("socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(pair[0]), "netns-listener-parent")
	child := os.NewFile(uintptr(pair[1]), "netns-listener-child")
	defer parentFile.Close()
	defer child.Close()
	genericParent, err := net.FileConn(parentFile)
	if err != nil {
		return nil, NamespaceIdentity{}, err
	}
	parent, ok := genericParent.(*net.UnixConn)
	if !ok {
		genericParent.Close()
		return nil, NamespaceIdentity{}, errors.New("namespace listener control is not Unix")
	}
	defer parent.Close()
	executable, err := os.Executable()
	if err != nil {
		return nil, NamespaceIdentity{}, err
	}
	command := exec.CommandContext(helperContext, "nsenter",
		"--user=/proc/self/fd/4", "--net=/proc/self/fd/5", "--preserve-credentials", "--",
		executable, "_netns-listener", "--control-fd", "3", "--address", address)
	command.ExtraFiles = []*os.File{child, userNS, networkNS}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	listener, err := runBoundListenerHelper(helperContext, command, parent, child, &stderr)
	if err != nil {
		return nil, NamespaceIdentity{}, err
	}
	return listener, identity, nil
}

func namespaceIdentity(userNS, networkNS *os.File) (NamespaceIdentity, error) {
	identity := NamespaceIdentity{}
	for file, pointers := range map[*os.File]struct{ device, inode *uint64 }{
		userNS:    {&identity.UserDevice, &identity.UserInode},
		networkNS: {&identity.NetworkDevice, &identity.NetworkInode},
	} {
		info, err := file.Stat()
		if err != nil {
			return NamespaceIdentity{}, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Ino == 0 {
			return NamespaceIdentity{}, errors.New("namespace descriptor identity is unavailable")
		}
		*pointers.device = uint64(stat.Dev)
		*pointers.inode = stat.Ino
	}
	return identity, nil
}

func runBoundListenerHelper(ctx context.Context, command *exec.Cmd, parent *net.UnixConn, child *os.File, stderr *bytes.Buffer) (net.Listener, error) {
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start namespace listener helper: %w", err)
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
		case <-ctx.Done():
			_ = command.Process.Kill()
			waitErr = <-waited
		}
	case waitErr = <-waited:
		_ = parent.Close()
		message = <-received
	case <-ctx.Done():
		_ = parent.Close()
		_ = command.Process.Kill()
		<-waited
		message = <-received
		closeReceivedDescriptors(message.control)
		return nil, context.Cause(ctx)
	}
	if cause := context.Cause(ctx); cause != nil {
		closeReceivedDescriptors(message.control)
		return nil, cause
	}
	if message.err != nil {
		closeReceivedDescriptors(message.control)
		return nil, namespaceHelperError(waitErr, stderr.String(), message.payload)
	}
	fds, parseErr := receivedDescriptors(message.control)
	if parseErr != nil || waitErr != nil || message.flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 || len(message.payload) != 1 || message.payload[0] != 1 || len(fds) != 1 {
		closeDescriptors(fds)
		return nil, namespaceHelperError(errors.Join(parseErr, waitErr), stderr.String(), message.payload)
	}
	file := os.NewFile(uintptr(fds[0]), "bound-world-listener")
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

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

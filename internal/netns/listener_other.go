//go:build !linux

package netns

import (
	"context"
	"fmt"
	"net"
)

func AcquireListener(context.Context, int, string) (net.Listener, error) {
	return nil, fmt.Errorf("Kenogram network namespace doors require Linux; use the Apple container-machine launcher on macOS")
}

func SendListener(int, string) error {
	return fmt.Errorf("Kenogram network namespace doors require Linux")
}

func AcquireConnection(context.Context, int, string) (net.Conn, error) {
	return nil, fmt.Errorf("Kenogram network namespace connections require Linux; use the Apple container-machine launcher on macOS")
}

func SendConnection(int, string) error {
	return fmt.Errorf("Kenogram network namespace connections require Linux")
}

func ParseHelperArgs([]string) (int, string, error) {
	return 0, "", fmt.Errorf("Kenogram network namespace helpers require Linux")
}

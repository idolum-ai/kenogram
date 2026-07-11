package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if filepath.Base(os.Args[0]) == "tail" {
		select {}
	}
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "interfaces":
		interfaces()
	case "dial":
		dial(os.Args[2])
	case "proxy":
		proxyDial(os.Args[2], os.Args[3])
	case "resolve":
		resolve(os.Args[2])
	case "udp":
		udp(os.Args[2])
	case "listeners":
		listeners()
	default:
		os.Exit(2)
	}
}
func interfaces() {
	items, err := net.Interfaces()
	if err != nil {
		fatal(err)
	}
	for _, item := range items {
		fmt.Println(item.Name)
	}
}
func dial(address string) {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		fmt.Println("unroutable")
		return
	}
	conn.Close()
	fmt.Println("connected")
}
func proxyDial(address, target string) {
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		fatal(err)
	}
	fmt.Println(strings.TrimSpace(line))
}
func resolve(host string) {
	if _, err := net.LookupHost(host); err != nil {
		fmt.Println("absent")
		return
	}
	fmt.Println("resolved")
}
func udp(address string) {
	conn, err := net.DialTimeout("udp", address, time.Second)
	if err != nil {
		fmt.Println("unroutable")
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err = conn.Write([]byte{0}); err != nil {
		fmt.Println("unroutable")
		return
	}
	buffer := make([]byte, 1)
	if _, err = conn.Read(buffer); err != nil {
		fmt.Println("unroutable")
		return
	}
	fmt.Println("answered")
}
func listeners() {
	raw, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) > 3 && fields[3] == "0A" {
			fmt.Println(fields[1])
		}
	}
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

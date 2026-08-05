//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(40)
	}
	switch os.Args[1] {
	case "--success":
		fmt.Println("success")
	case "--proof":
		runProof()
	case "--read-only":
		runReadOnly()
	case "--egress":
		if len(os.Args) != 3 {
			os.Exit(40)
		}
		runEgress(os.Args[2])
	case "--hang-orphan":
		child := exec.Command(os.Args[0], "--child")
		if err := child.Start(); err != nil {
			os.Exit(50)
		}
		fmt.Printf("orphan=%d\n", child.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	case "--child":
		time.Sleep(time.Hour)
	default:
		os.Exit(40)
	}
}

func runReadOnly() {
	raw, err := os.ReadFile("/input/read-only.txt")
	if err != nil || string(raw) != "mounted\n" {
		fmt.Fprintln(os.Stderr, "read-only mount missing")
		os.Exit(43)
	}
	if err := os.WriteFile("/input/read-only.txt", []byte("changed"), 0o600); err == nil {
		fmt.Fprintln(os.Stderr, "read-only file accepted a write")
		os.Exit(44)
	}
	if err := os.WriteFile("/input/must-not-write", []byte("x"), 0o600); err == nil {
		fmt.Fprintln(os.Stderr, "read-only directory accepted a write")
		os.Exit(45)
	}
	if err := os.Chmod("/input/read-only.txt", 0o600); err == nil {
		fmt.Fprintln(os.Stderr, "read-only file accepted chmod")
		os.Exit(46)
	}
	if err := os.Rename("/input/read-only.txt", "/input/renamed"); err == nil {
		fmt.Fprintln(os.Stderr, "read-only file accepted rename")
		os.Exit(47)
	}
	if err := os.Remove("/input/read-only.txt"); err == nil {
		fmt.Fprintln(os.Stderr, "read-only file accepted unlink")
		os.Exit(48)
	}
	probePortableWorkspace(49)
	fmt.Println("private read-only and writable workspace mounts are portable")
}

func probePortableWorkspace(exitCode int) {
	root := "/workspace/portable-writable-probe"
	if err := os.Mkdir(root, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "portable workspace directory could not be created")
		os.Exit(exitCode)
	}
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("portable\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "portable workspace file could not be written")
		os.Exit(exitCode + 1)
	}
	if err := os.Chmod(first, 0o640); err != nil {
		fmt.Fprintln(os.Stderr, "portable workspace file could not be chmodded")
		os.Exit(exitCode + 2)
	}
	if err := os.Rename(first, second); err != nil {
		fmt.Fprintln(os.Stderr, "portable workspace file could not be renamed")
		os.Exit(exitCode + 3)
	}
	if err := os.Remove(second); err != nil {
		fmt.Fprintln(os.Stderr, "portable workspace file could not be removed")
		os.Exit(exitCode + 4)
	}
	if err := os.Remove(root); err != nil {
		fmt.Fprintln(os.Stderr, "portable workspace directory could not be removed")
		os.Exit(exitCode + 5)
	}
}

func runEgress(target string) {
	proxyURL := os.Getenv("HTTPS_PROXY")
	for _, name := range []string{"HTTP_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if os.Getenv(name) != proxyURL || proxyURL == "" {
			fmt.Fprintln(os.Stderr, "governed proxy environment mismatch")
			os.Exit(60)
		}
	}
	if os.Getenv("NO_PROXY") != "" || os.Getenv("no_proxy") != "" {
		fmt.Fprintln(os.Stderr, "governed target received NO_PROXY")
		os.Exit(61)
	}
	proxyAddress := strings.TrimPrefix(proxyURL, "http://")
	connection, err := net.DialTimeout("tcp", proxyAddress, time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "governed proxy door unavailable")
		os.Exit(62)
	}
	reader := bufio.NewReader(connection)
	_, _ = fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nX-Canary: egress-secret-canary\r\n\r\n", target, target)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		fmt.Fprintln(os.Stderr, "declared CONNECT failed")
		os.Exit(63)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			os.Exit(63)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = connection.Write([]byte("ping"))
	response := make([]byte, 4)
	if _, err := io.ReadFull(reader, response); err != nil || string(response) != "pong" {
		os.Exit(64)
	}
	connection.Close()
	denied, err := net.DialTimeout("tcp", proxyAddress, time.Second)
	if err != nil {
		os.Exit(65)
	}
	_, _ = io.WriteString(denied, "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n")
	deniedStatus, _ := bufio.NewReader(denied).ReadString('\n')
	denied.Close()
	if !strings.Contains(deniedStatus, "403") {
		os.Exit(66)
	}
	direct, err := net.DialTimeout("tcp", target, 250*time.Millisecond)
	if err == nil {
		direct.Close()
		os.Exit(67)
	}
	fmt.Println("governed egress complete")
}

func runProof() {
	if os.Getenv("KENOGRAM_RETAINED") != "explicit" || os.Getenv("KENOGRAM_SECRET") != "integrated-secret\nline" || len(os.Environ()) != 2 {
		fmt.Fprintln(os.Stderr, "governed environment mismatch")
		os.Exit(41)
	}
	secret, err := os.ReadFile("/usr/local/bin/secret-token")
	if err != nil || string(secret) != "integrated-secret\nline" {
		fmt.Fprintln(os.Stderr, "secret copy mismatch")
		os.Exit(42)
	}
	raw, err := os.ReadFile("/input/read-only.txt")
	if err != nil || string(raw) != "mounted\n" {
		fmt.Fprintln(os.Stderr, "read-only mount missing")
		os.Exit(43)
	}
	if err := os.WriteFile("/input/must-not-write", []byte("x"), 0o600); err == nil {
		fmt.Fprintln(os.Stderr, "read-only mount accepted a write")
		os.Exit(44)
	}
	connection, err := net.DialTimeout("tcp", "1.1.1.1:53", 250*time.Millisecond)
	if err == nil {
		connection.Close()
		fmt.Fprintln(os.Stderr, "network-none admitted egress")
		os.Exit(45)
	}
	for _, path := range []string{"/run/podman/podman.sock", "/run/docker.sock", "/var/run/docker.sock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "runtime control socket is visible")
			os.Exit(46)
		}
	}
	hostile := "/workspace/cleanup-hostile/locked"
	if err := os.MkdirAll(hostile, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup-hostile tree could not be created")
		os.Exit(47)
	}
	if err := os.WriteFile(filepath.Join(hostile, "private"), []byte("remove me"), 0o000); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup-hostile file could not be created")
		os.Exit(48)
	}
	if err := os.Symlink("private", filepath.Join(hostile, "link")); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup-hostile symlink could not be created")
		os.Exit(49)
	}
	if err := syscall.Mkfifo(filepath.Join(hostile, "fifo"), 0o000); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup-hostile fifo could not be created")
		os.Exit(50)
	}
	if err := os.Chmod(hostile, 0o000); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup-hostile directory could not be locked")
		os.Exit(51)
	}
	if err := os.Mkdir("/workspace/artifacts", 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "artifact root could not be created")
		os.Exit(52)
	}
	if err := os.WriteFile(filepath.Join("/workspace/artifacts", "report.json"), []byte("{\"proof\":true}\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "artifact could not be written")
		os.Exit(53)
	}
	fmt.Println("governed stdout")
	fmt.Fprintln(os.Stderr, "governed stderr")
	os.Exit(7)
}

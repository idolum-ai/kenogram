//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(40)
	}
	switch os.Args[1] {
	case "--success":
		fmt.Println("success")
	case "--proof":
		runProof()
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
	if err := os.Mkdir("/artifacts", 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "artifact root could not be created")
		os.Exit(47)
	}
	if err := os.WriteFile(filepath.Join("/artifacts", "report.json"), []byte("{\"proof\":true}\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "artifact could not be written")
		os.Exit(48)
	}
	fmt.Println("governed stdout")
	fmt.Fprintln(os.Stderr, "governed stderr")
	os.Exit(7)
}

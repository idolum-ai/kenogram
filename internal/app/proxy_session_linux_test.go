//go:build linux

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/kenogram/internal/worldfs"
)

const (
	proxySessionHelper = "KENOGRAM_PROXY_SESSION_HELPER"
	proxySessionBase   = "KENOGRAM_PROXY_SESSION_BASE"
)

func TestProxySessionHelper(t *testing.T) {
	if os.Getenv(proxySessionHelper) != "1" {
		return
	}
	base := os.Getenv(proxySessionBase)
	layout := worldfs.For(base, "w")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	a := &App{BaseDir: base, Executable: crashProxyExecutable(t, base), proxyReady: crashProxyReady}
	if _, err := a.startProxy(context.Background(), layout, 123, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "applying-session-ready"), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestProxySurvivesApplyingSessionTeardown(t *testing.T) {
	base := t.TempDir()
	layout := worldfs.For(base, "w")
	a := &App{BaseDir: base, Executable: filepath.Join(base, "fake-proxy.sh"), proxyReady: crashProxyReady}
	t.Cleanup(func() { _ = a.stopProxy(layout) })
	command := exec.Command(os.Args[0], "-test.run=^TestProxySessionHelper$")
	command.Env = append(os.Environ(), proxySessionHelper+"=1", proxySessionBase+"="+base)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helperPID := command.Process.Pid
	helperFinished := false
	t.Cleanup(func() {
		if !helperFinished {
			_ = syscall.Kill(-helperPID, syscall.SIGKILL)
			_ = command.Wait()
		}
	})
	waitForProxySessionFile(t, filepath.Join(base, "applying-session-ready"))

	raw, err := os.ReadFile(layout.ProxyPID)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		t.Fatalf("proxy identity = %q", raw)
	}
	proxyPID, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	if session := processSessionID(t, proxyPID); session != proxyPID {
		t.Fatalf("proxy session = %d, want session leader PID %d", session, proxyPID)
	}

	// Model a launcher or terminal tearing down the applying process group.
	// The proxy must remain in the independent session established by startProxy.
	if err := syscall.Kill(-helperPID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	helperFinished = true
	var exitErr *exec.ExitError
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if !errors.As(waitErr, &exitErr) || !ok || !status.Signaled() || status.Signal() != syscall.SIGHUP {
		t.Fatalf("applying helper exit = %v\n%s", waitErr, output.String())
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !a.proxyAlive(layout) {
			t.Fatal("proxy died with the applying process group")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProxySessionFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func processSessionID(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		t.Fatalf("invalid proc stat for PID %d", pid)
	}
	fields := strings.Fields(string(raw)[end+1:])
	if len(fields) < 4 {
		t.Fatalf("short proc stat for PID %d", pid)
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		t.Fatal(err)
	}
	return session
}

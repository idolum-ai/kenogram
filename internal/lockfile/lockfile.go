// Package lockfile serializes mutations of one world.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type Lock struct {
	path string
	file *os.File
}

func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("world is already being mutated: %s", path)
		}
		return nil, fmt.Errorf("lock world: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("truncate lock: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("rewind lock: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + " " + ProcessStart(os.Getpid()) + "\n"); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("write lock: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, fmt.Errorf("sync lock: %w", err)
	}
	return &Lock{path: path, file: f}, nil
}

// ProcessStart returns the Linux process start-time field used to distinguish
// a live process from a later process that reused its PID.
func ProcessStart(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	return processStartFromStat(string(raw))
}

func processStartFromStat(text string) string {
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return ""
	}
	// A zombie cannot own a lock or serve a proxy even if an unreaping parent
	// leaves its /proc entry visible temporarily.
	if fields[0] == "Z" {
		return ""
	}
	return fields[19]
}

func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	return l.releaseAt(l.path)
}

// ReleaseMoved releases a lock whose containing directory was atomically
// renamed while held.
func (l *Lock) ReleaseMoved(path string) error {
	return l.releaseAt(path)
}

func (l *Lock) releaseAt(path string) error {
	_ = path
	if l == nil || l.file == nil {
		return nil
	}
	err := errors.Join(syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN), l.file.Close())
	l.file = nil
	if err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}

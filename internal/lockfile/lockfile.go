// Package lockfile serializes mutations of one world.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Lock struct {
	path string
	file *os.File
}

func Acquire(path string) (*Lock, error) {
	return acquire(path, true)
}

func acquire(path string, allowStale bool) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		if allowStale && stale(path) {
			if removeErr := os.Remove(path); removeErr == nil {
				return acquire(path, false)
			}
		}
		return nil, fmt.Errorf("world is already being mutated: %s", path)
	}
	if err != nil {
		return nil, fmt.Errorf("create lock: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + " " + processStart(os.Getpid()) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("write lock: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("sync lock: %w", err)
	}
	return &Lock{path: path, file: f}, nil
}

func stale(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return false
	}
	start := processStart(pid)
	return start == "" || start != fields[1]
}
func processStart(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	text := string(raw)
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := errors.Join(l.file.Close(), os.Remove(l.path))
	l.file = nil
	if err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}

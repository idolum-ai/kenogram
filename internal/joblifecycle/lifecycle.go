// Package joblifecycle authenticates the target-local helper's bounded process
// lifecycle observation. The target never receives the MAC key.
package joblifecycle

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

const Schema = "kenogram.target-lifecycle.v1"
const MaximumBytes = 16 << 10
const FileName = "target-lifecycle.json"

type Record struct {
	Schema     string `json:"schema"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationNS int64  `json:"duration_ns"`
	ExitStatus *int64 `json:"exit_status,omitempty"`
	Signal     *int64 `json:"signal,omitempty"`
	MAC        string `json:"mac"`
}

// FileIdentity binds a precreated lifecycle slot to the exact inode staged by
// the host. The target-local helper may write that inode, but it may not create
// a different path and have the host accept it as lifecycle authority.
type FileIdentity struct {
	Device uint64
	Inode  uint64
}

type payload struct {
	Schema     string `json:"schema"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationNS int64  `json:"duration_ns"`
	ExitStatus *int64 `json:"exit_status,omitempty"`
	Signal     *int64 `json:"signal,omitempty"`
}

func Seal(record Record, key []byte) (Record, error) {
	if err := validate(record, key, false); err != nil {
		return Record{}, err
	}
	raw, _ := json.Marshal(payload{record.Schema, record.StartedAt, record.FinishedAt, record.DurationNS, record.ExitStatus, record.Signal})
	hash := hmac.New(sha256.New, key)
	hash.Write(raw)
	record.MAC = hex.EncodeToString(hash.Sum(nil))
	return record, nil
}

func Write(path string, record Record, key []byte) error {
	raw, err := encode(record, key)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// Prepare creates the one lifecycle inode that may be written across the
// rootless user-namespace boundary. The host keeps its parent private and bind
// mounts only this exact file into the contained process.
func Prepare(path string) (FileIdentity, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o622)
	if err != nil {
		return FileIdentity{}, err
	}
	if err = file.Chmod(0o622); err != nil {
		return FileIdentity{}, errors.Join(err, file.Close())
	}
	if err = file.Sync(); err != nil {
		return FileIdentity{}, errors.Join(err, file.Close())
	}
	info, statErr := file.Stat()
	identity, identityErr := identityOf(info)
	return identity, errors.Join(statErr, identityErr, file.Close())
}

// WriteSlot replaces the empty contents of a host-precreated regular file. It
// never creates or follows a path supplied by the contained target.
func WriteSlot(path string, record Record, key []byte) error {
	raw, err := encode(record, key)
	if err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != 0 {
		return errors.Join(err, errors.New("target lifecycle slot is not an empty regular file"))
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return errors.Join(statErr, file.Close(), errors.New("target lifecycle slot identity changed before write"))
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func encode(record Record, key []byte) ([]byte, error) {
	sealed, err := Seal(record, key)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func Read(path string, key []byte) (Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
	return read(file, key)
}

// ReadSlot accepts lifecycle evidence only from the exact regular inode that
// Prepare staged. O_NOFOLLOW and the identity comparison make symlink or path
// substitution fail closed.
func ReadSlot(path string, key []byte, expected FileIdentity) (Record, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Record{}, err
	}
	info, statErr := file.Stat()
	identity, identityErr := identityOf(info)
	if statErr != nil || identityErr != nil || identity != expected {
		return Record{}, errors.Join(statErr, identityErr, file.Close(), errors.New("target lifecycle slot identity changed"))
	}
	return read(file, key)
}

func read(file *os.File, key []byte) (Record, error) {
	raw, readErr := io.ReadAll(io.LimitReader(file, MaximumBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > MaximumBytes {
		return Record{}, errors.Join(readErr, closeErr, errors.New("target lifecycle is unreadable or oversized"))
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Record{}, errors.New("target lifecycle has trailing data")
	}
	if err := validate(record, key, true); err != nil {
		return Record{}, err
	}
	want := record.MAC
	record.MAC = ""
	sealed, _ := Seal(record, key)
	if !hmac.Equal([]byte(want), []byte(sealed.MAC)) {
		return Record{}, errors.New("target lifecycle authentication failed")
	}
	record.MAC = want
	return record, nil
}

func identityOf(info os.FileInfo) (FileIdentity, error) {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return FileIdentity{}, errors.New("target lifecycle slot is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, errors.New("target lifecycle slot identity is unavailable")
	}
	return FileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func validate(record Record, key []byte, requireMAC bool) error {
	if len(key) != 32 || record.Schema != Schema || record.DurationNS < 0 || record.DurationNS > 9_007_199_254_740_991 || (record.ExitStatus == nil) == (record.Signal == nil) {
		return errors.New("target lifecycle authority is invalid")
	}
	start, err1 := time.Parse(time.RFC3339Nano, record.StartedAt)
	finish, err2 := time.Parse(time.RFC3339Nano, record.FinishedAt)
	if err1 != nil || err2 != nil || finish.Before(start) {
		return errors.New("target lifecycle interval is invalid")
	}
	if record.ExitStatus != nil && (*record.ExitStatus < 0 || *record.ExitStatus > 255) {
		return errors.New("target lifecycle exit status is invalid")
	}
	if record.Signal != nil && (*record.Signal < 1 || *record.Signal > 255) {
		return errors.New("target lifecycle signal is invalid")
	}
	if requireMAC {
		if len(record.MAC) != 64 {
			return errors.New("target lifecycle MAC is invalid")
		}
		if _, err := hex.DecodeString(record.MAC); err != nil {
			return fmt.Errorf("target lifecycle MAC: %w", err)
		}
	}
	return nil
}

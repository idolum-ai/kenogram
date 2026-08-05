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
	sealed, err := Seal(record, key)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func Read(path string, key []byte) (Record, error) {
	file, err := os.Open(path)
	if err != nil {
		return Record{}, err
	}
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

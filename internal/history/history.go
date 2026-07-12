// Package history owns the fsync'd, hash-chained per-world history.
package history

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Record struct {
	Timestamp         string   `json:"timestamp"`
	Action            string   `json:"action"`
	PlanDigest        string   `json:"plan_digest,omitempty"`
	DeclarationDigest string   `json:"declaration_digest,omitempty"`
	ImageDigests      []string `json:"image_digests,omitempty"`
	WorkspaceDigest   string   `json:"workspace_digest,omitempty"`
	Outcome           string   `json:"outcome"`
	Detail            string   `json:"detail,omitempty"`
	PreviousHash      string   `json:"previous_hash,omitempty"`
	Hash              string   `json:"hash"`
}
type unsigned struct {
	Timestamp         string   `json:"timestamp"`
	Action            string   `json:"action"`
	PlanDigest        string   `json:"plan_digest,omitempty"`
	DeclarationDigest string   `json:"declaration_digest,omitempty"`
	ImageDigests      []string `json:"image_digests,omitempty"`
	WorkspaceDigest   string   `json:"workspace_digest,omitempty"`
	Outcome           string   `json:"outcome"`
	Detail            string   `json:"detail,omitempty"`
	PreviousHash      string   `json:"previous_hash,omitempty"`
}

func Append(path string, record Record, now time.Time) (Record, error) {
	record.Timestamp = now.UTC().Format(time.RFC3339Nano)
	prior, err := Verify(path)
	if err != nil && !os.IsNotExist(err) {
		return Record{}, err
	}
	if len(prior) > 0 {
		record.PreviousHash = prior[len(prior)-1].Hash
	}
	record.Hash, err = calculate(record)
	if err != nil {
		return Record{}, err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return Record{}, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return Record{}, err
	}
	if err := f.Sync(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// AppendOnce makes recovery idempotent. It suppresses only an immediately
// repeated semantic record; a later, genuinely distinct operation is still
// appended even when it happens to use the same declaration.
func AppendOnce(path string, record Record, now time.Time) (Record, error) {
	prior, err := Verify(path)
	if err != nil && !os.IsNotExist(err) {
		return Record{}, err
	}
	if len(prior) > 0 {
		last := prior[len(prior)-1]
		if last.Action == record.Action &&
			last.PlanDigest == record.PlanDigest &&
			last.DeclarationDigest == record.DeclarationDigest &&
			last.WorkspaceDigest == record.WorkspaceDigest &&
			last.Outcome == record.Outcome &&
			last.Detail == record.Detail {
			return last, nil
		}
	}
	return Append(path, record, now)
}
func Verify(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > 0 {
		last := []byte{0}
		if _, err := f.ReadAt(last, info.Size()-1); err != nil {
			return nil, err
		}
		if last[0] != '\n' {
			return nil, fmt.Errorf("history has a truncated final record")
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	records := []Record{}
	previous := ""
	line := 0
	for scanner.Scan() {
		line++
		var r Record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("history line %d: %w", line, err)
		}
		if r.PreviousHash != previous {
			return nil, fmt.Errorf("history line %d: previous hash mismatch", line)
		}
		expected, err := calculate(r)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(expected, r.Hash) {
			return nil, fmt.Errorf("history line %d: hash mismatch", line)
		}
		records = append(records, r)
		previous = r.Hash
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// RepairTruncatedTail removes only a malformed, non-newline-terminated final
// fragment after proving that every preceding record is a valid hash chain. It
// never heals a complete record, hash mismatch, or interior corruption.
func RepairTruncatedTail(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] == '\n' {
		return fmt.Errorf("history has no truncated final fragment")
	}
	if _, err := Verify(path); err == nil {
		return fmt.Errorf("history final record is complete")
	}
	cut := strings.LastIndexByte(string(raw), '\n') + 1
	prefix := raw[:cut]
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-repair-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(prefix); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if _, err := Verify(tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("history before truncated tail is invalid: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func calculate(r Record) (string, error) {
	u := unsigned{r.Timestamp, r.Action, r.PlanDigest, r.DeclarationDigest, r.ImageDigests, r.WorkspaceDigest, r.Outcome, r.Detail, r.PreviousHash}
	raw, err := json.Marshal(u)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

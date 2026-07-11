// Package history owns the fsync'd, hash-chained per-world history.
package history

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
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
func Verify(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
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
func calculate(r Record) (string, error) {
	u := unsigned{r.Timestamp, r.Action, r.PlanDigest, r.DeclarationDigest, r.ImageDigests, r.WorkspaceDigest, r.Outcome, r.Detail, r.PreviousHash}
	raw, err := json.Marshal(u)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

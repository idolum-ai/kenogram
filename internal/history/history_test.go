package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendVerifyAndTamper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1, 0)
	if _, err := Append(path, Record{Action: "up", Outcome: "applied"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := Append(path, Record{Action: "down", Outcome: "stopped"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	records, err := Verify(path)
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[20] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err == nil {
		t.Fatal("tamper accepted")
	}
}

func TestAppendOnceSuppressesOnlyAdjacentDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1, 0)
	record := Record{Action: "up", PlanDigest: "plan", Outcome: "applied"}
	if _, err := AppendOnce(path, record, now); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendOnce(path, record, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	record.Outcome = "adopted"
	if _, err := AppendOnce(path, record, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	record.Outcome = "applied"
	if _, err := AppendOnce(path, record, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	records, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
}

func TestRepairTruncatedTailPreservesValidPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if _, err := Append(path, Record{Action: "up", Outcome: "applied"}, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"timestamp":"interrupted"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RepairTruncatedTail(path); err != nil {
		t.Fatal(err)
	}
	records, err := Verify(path)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
}

func TestRepairRefusesCompleteHashMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RepairTruncatedTail(path); err == nil {
		t.Fatal("complete corrupt record was repaired")
	}
}

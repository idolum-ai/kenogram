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

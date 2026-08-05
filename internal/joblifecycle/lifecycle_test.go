package joblifecycle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLifecycleRoundTripAndAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	status := int64(7)
	record := Record{Schema: Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}
	path := filepath.Join(t.TempDir(), FileName)
	if err := Write(path, record, key); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path, key)
	if err != nil || got.ExitStatus == nil || *got.ExitStatus != 7 {
		t.Fatalf("got=%#v error=%v", got, err)
	}
	raw, _ := os.ReadFile(path)
	raw = bytes.Replace(raw, []byte(`"duration_ns":1000000000`), []byte(`"duration_ns":1000000001`), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, key); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("tamper error=%v", err)
	}
}

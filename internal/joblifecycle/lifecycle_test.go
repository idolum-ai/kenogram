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

func TestPreparedLifecycleSlotRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	status := int64(0)
	record := Record{Schema: Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}
	path := filepath.Join(t.TempDir(), FileName)
	identity, err := Prepare(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o622 || info.Size() != 0 {
		t.Fatalf("slot mode=%v size=%d", info.Mode().Perm(), info.Size())
	}
	if err := WriteSlot(path, record, key); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSlot(path, key, identity); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedLifecycleSlotRejectsPrewriteAndSubstitution(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	status := int64(0)
	record := Record{Schema: Schema, StartedAt: "2026-08-05T12:00:00Z", FinishedAt: "2026-08-05T12:00:01Z", DurationNS: int64(time.Second), ExitStatus: &status}

	t.Run("prewrite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		if _, err := Prepare(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("attacker"), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := WriteSlot(path, record, key); err == nil || !strings.Contains(err.Error(), "empty regular file") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, nil, 0o666); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, FileName)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if err := WriteSlot(path, record, key); err == nil {
			t.Fatal("symlink lifecycle slot accepted")
		}
	})

	t.Run("inode replacement", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		identity, err := Prepare(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := Write(path, record, key); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSlot(path, key, identity); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("error=%v", err)
		}
	})
}

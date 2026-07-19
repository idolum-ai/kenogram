package lockfile

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireExclusiveAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("second acquisition succeeded")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireReclaimsStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world.lock")
	if err := os.WriteFile(path, []byte("999999 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
}

func TestAcquireSharedDoesNotRewriteMetadataAndExcludesMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world.lock")
	want := []byte("retained lock metadata\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := AcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := AcquireShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if _, err := Acquire(path); err == nil {
		t.Fatal("exclusive mutation lock overlapped observations")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("shared lock rewrote metadata: %q", got)
	}
}

func TestProcessStartTreatsZombieAsDead(t *testing.T) {
	fields := "Z 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 4242"
	if got := ProcessStartFromStat("99 (proxy worker) " + fields); got != "" {
		t.Fatalf("zombie start = %q", got)
	}
	fields = "S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 4242"
	if got := ProcessStartFromStat("99 (proxy worker) " + fields); got != "19" {
		t.Fatalf("live start = %q", got)
	}
}

func TestKernelLockSerializesProcessesAfterStaleMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world.lock")
	if err := os.WriteFile(path, []byte("999999 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 8)
	outputs := make([]*bytes.Buffer, len(commands))
	for index := range commands {
		command := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
		command.Env = append(os.Environ(), "KENOGRAM_LOCK_HELPER=1", "KENOGRAM_LOCK_PATH="+path)
		outputs[index] = &bytes.Buffer{}
		command.Stdout = outputs[index]
		command.Stderr = outputs[index]
		commands[index] = command
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("lock contender failed: %v\n%s", err, outputs[index].String())
		}
	}
}

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv("KENOGRAM_LOCK_HELPER") != "1" {
		return
	}
	path := os.Getenv("KENOGRAM_LOCK_PATH")
	deadline := time.Now().Add(5 * time.Second)
	var lock *Lock
	for time.Now().Before(deadline) {
		acquired, err := Acquire(path)
		if err == nil {
			lock = acquired
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lock == nil {
		t.Fatal("timed out acquiring kernel lock")
	}
	guard := path + ".critical"
	file, err := os.OpenFile(guard, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("overlapping critical section: %v", err)
	}
	_ = file.Close()
	time.Sleep(20 * time.Millisecond)
	if err := os.Remove(guard); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

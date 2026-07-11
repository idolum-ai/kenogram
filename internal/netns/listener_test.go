package netns

import "testing"

func TestParseHelperArgs(t *testing.T) {
	fd, address, err := ParseHelperArgs([]string{"--control-fd", "3", "--address", "127.0.0.1:3128"})
	if err != nil || fd != 3 || address != "127.0.0.1:3128" {
		t.Fatalf("fd=%d address=%q err=%v", fd, address, err)
	}
	if _, _, err := ParseHelperArgs(nil); err == nil {
		t.Fatal("empty accepted")
	}
}

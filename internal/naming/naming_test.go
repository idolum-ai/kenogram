package naming

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationalNames(t *testing.T) {
	for _, value := range []string{"a", "engineering", "agent-1", "a.b_c", strings.Repeat("a", 63)} {
		if err := World(value); err != nil {
			t.Errorf("World(%q): %v", value, err)
		}
		if err := Service(value); err != nil {
			t.Errorf("Service(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", ".", "..", "A", "-a", "a/b", `a\b`, "a b", " a", "a\n", strings.Repeat("a", 64)} {
		if err := World(value); err == nil {
			t.Errorf("World(%q) accepted", value)
		}
		if err := Service(value); err == nil {
			t.Errorf("Service(%q) accepted", value)
		}
	}
}

func TestExactHosts(t *testing.T) {
	for _, value := range []string{"example.com", "LOCALHOST.", "127.0.0.1", "2001:db8::1"} {
		if err := Host(value); err != nil {
			t.Errorf("Host(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "*", "bad host", "bad\thost", "bad\nhost", "user@example.com", "host/path", "[2001:db8::1]", "not:ipv6"} {
		if err := Host(value); err == nil {
			t.Errorf("Host(%q) accepted", value)
		}
	}
}

func TestJoinUnder(t *testing.T) {
	root := t.TempDir()
	path, err := JoinUnder(root, "service.sh")
	if err != nil || path != filepath.Join(root, "service.sh") {
		t.Fatalf("path=%q err=%v", path, err)
	}
	for _, value := range []string{".", "..", "../escape", filepath.Join("x", "..", "..", "escape")} {
		if _, err := JoinUnder(root, value); err == nil {
			t.Errorf("JoinUnder(%q) accepted", value)
		}
	}
}

package decl

import (
	"strings"
	"testing"
)

const validDeclaration = `
version = 1
name = "engineering" # comment
[world]
hostname = "engineering"
base = "ubuntu:24.04@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
workdir = "/workspace"
user = "agent"
[resources]
cpus = +4
memory_bytes = 8_589_934_592
pids = 512
[workspace]
paths = ["/workspace", "/data",]
[[copies]]
source = "./secret"
target = "/home/agent/.token"
mode = "0600"
secret = true
[[mounts]]
source = "./repo"
target = "/workspace/repo"
mode = "rw"
[[network.allow]]
host = "api.example.com"
port = 443
[[network.allow]]
host = "registry.example.com"
port = 443
[[interfaces]]
name = "ssh"
address = "127.0.0.1:2222"
[[services]]
name = "session"
command = ["/usr/bin/tmux", "new-session", "-d"]
autostart = true
restart = "on-failure"
`

func TestParseCompleteDeclaration(t *testing.T) {
	t.Parallel()
	d, err := Parse([]byte(validDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "engineering" || d.Resources.MemoryBytes != 8589934592 {
		t.Fatalf("unexpected declaration: %#v", d)
	}
	if len(d.Network.Allow) != 2 || d.Network.Allow[1].Host != "registry.example.com" {
		t.Fatalf("unexpected network: %#v", d.Network)
	}
	if len(d.Services) != 1 || len(d.Services[0].Command) != 3 {
		t.Fatalf("unexpected services: %#v", d.Services)
	}
	if len(d.Interfaces) != 1 || d.Interfaces[0].Address != "127.0.0.1:2222" {
		t.Fatalf("unexpected interfaces: %#v", d.Interfaces)
	}
}

func TestParseRejectsUnknownKeysEverywhere(t *testing.T) {
	t.Parallel()
	input := strings.Replace(validDeclaration, "mode = \"0600\"", "mode = \"0600\"\nmdoe = \"0600\"", 1)
	_, err := Parse([]byte(input))
	if err == nil || !strings.Contains(err.Error(), "unknown key or table root.copies[0].mdoe") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsDuplicateKey(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("version = 1\nversion = 1\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsMixedArray(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("paths = [\"x\", 1]\n"))
	if err == nil || !strings.Contains(err.Error(), "one type") {
		t.Fatalf("got %v", err)
	}
}

func TestParseRejectsUnsupportedSyntax(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"value = 1.5\n",
		"value = 1979-05-27\n",
		"value = { x = 1 }\n",
		"\"quoted\" = 1\n",
		"a.b = 1\n",
		"value = [[1]]\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestParseErrorHasLine(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("# ok\nversion = nope\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("got %v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(validDeclaration))
	f.Add([]byte("version = 1\n[world\n"))
	f.Add([]byte{0xff, 0xfe})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = Parse(data) })
}

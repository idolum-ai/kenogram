package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type resolver struct {
	mu    sync.Mutex
	hosts []string
}

func (r *resolver) LookupIPAddr(_ context.Context, h string) ([]net.IPAddr, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, h)
	r.mu.Unlock()
	return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
}

type pipeDialer struct{}

func (pipeDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		_, _ = io.Copy(server, server)
	}()
	return client, nil
}

func TestCONNECTExactAllowanceAndProxyResolution(t *testing.T) {
	port := 8443
	res := &resolver{}
	p := New([]Destination{{"allowed.example", port}}, Options{Resolver: res, Dialer: pipeDialer{}})
	conn, server := net.Pipe()
	go p.handle(server)
	fmt.Fprintf(conn, "CONNECT allowed.example:%d HTTP/1.1\r\nHost: allowed.example:%d\r\n\r\n", port, port)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, "200") {
		t.Fatalf("line=%q err=%v", line, err)
	}
	conn.Close()
	res.mu.Lock()
	defer res.mu.Unlock()
	if len(res.hosts) != 1 || res.hosts[0] != "allowed.example" {
		t.Fatalf("lookups=%v", res.hosts)
	}
}
func TestRefusedDestinationNeverResolves(t *testing.T) {
	res := &resolver{}
	p := New(nil, Options{Resolver: res})
	client, server := net.Pipe()
	go p.handle(server)
	fmt.Fprint(client, "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n")
	line, _ := bufio.NewReader(client).ReadString('\n')
	client.Close()
	if !strings.Contains(line, "403") {
		t.Fatalf("line=%q", line)
	}
	if len(res.hosts) != 0 {
		t.Fatal("denied host resolved")
	}
}
func TestGrantExpires(t *testing.T) {
	p := New(nil, Options{})
	d := Destination{"x", 443}
	if err := p.Grant(d, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.allowed(d); !ok {
		t.Fatal("grant absent")
	}
	time.Sleep(20 * time.Millisecond)
	if _, ok := p.allowed(d); ok {
		t.Fatal("grant survived")
	}
}

func TestGrantExpiryClosesAdmittedConnection(t *testing.T) {
	p := New(nil, Options{})
	d := Destination{"x", 443}
	if err := p.Grant(d, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	admission, ok := p.allowed(d)
	if !ok {
		t.Fatal("not allowed")
	}
	client, server := net.Pipe()
	defer client.Close()
	p.track(server, admission)
	client.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := client.Read(buffer); err == nil {
		t.Fatal("connection remained open")
	}
}

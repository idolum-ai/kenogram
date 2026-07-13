package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlPingProvesRoundTrip(t *testing.T) {
	p := New(nil, Options{})
	client, server := net.Pipe()
	go p.control(server)
	defer client.Close()
	if err := json.NewEncoder(client).Encode(ControlRequest{Operation: "ping"}); err != nil {
		t.Fatal(err)
	}
	var response ControlResponse
	if err := json.NewDecoder(client).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Error != "" {
		t.Fatalf("ping response = %#v", response)
	}
}

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

type blockingDialer struct {
	entered chan struct{}
	release chan struct{}
	peer    chan net.Conn
}

func (d *blockingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	close(d.entered)
	<-d.release
	client, server := net.Pipe()
	d.peer <- server
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

func TestCONNECTCarriesPayloadLargerThanRequestLimit(t *testing.T) {
	p := New([]Destination{{"allowed.example", 8443}}, Options{Resolver: &resolver{}, Dialer: pipeDialer{}})
	client, server := net.Pipe()
	go p.handle(server)
	defer client.Close()
	if _, err := fmt.Fprint(client, "CONNECT allowed.example:8443 HTTP/1.1\r\nHost: allowed.example:8443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	payload := []byte(strings.Repeat("kenogram", 256<<10))
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := client.Write(payload)
		writeDone <- err
	}()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("tunneled payload changed")
	}
}

func TestProxyRejectsHeaderLargerThan64KiB(t *testing.T) {
	p := New([]Destination{{"allowed.example", 8443}}, Options{Resolver: &resolver{}, Dialer: pipeDialer{}})
	client, server := net.Pipe()
	go p.handle(server)
	defer client.Close()
	writeDone := make(chan struct{})
	go func() {
		_, _ = fmt.Fprintf(client, "CONNECT allowed.example:8443 HTTP/1.1\r\nX-Fill: %s\r\n\r\n", strings.Repeat("x", 70<<10))
		close(writeDone)
	}()
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil || !strings.Contains(line, "400") {
		t.Fatalf("line=%q err=%v", line, err)
	}
	<-writeDone
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
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, current := p.trackIfCurrent(server, admission); !current {
		t.Fatal("admission unexpectedly stale")
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	if _, err := client.Read(buffer); err == nil {
		t.Fatal("connection remained open")
	}
}

func TestRemoveClosesAdmittedConnection(t *testing.T) {
	p := New([]Destination{{"x", 443}}, Options{})
	d := Destination{"x", 443}
	admission, ok := p.allowed(d)
	if !ok {
		t.Fatal("not allowed")
	}
	client, server := net.Pipe()
	defer client.Close()
	if _, current := p.trackIfCurrent(server, admission); !current {
		t.Fatal("admission unexpectedly stale")
	}
	p.Remove(d)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection remained open")
	}
	if _, ok := p.allowed(d); ok {
		t.Fatal("destination remained allowed")
	}
}

func TestPolicyChangeDuringDialCannotCreateConnection(t *testing.T) {
	for _, test := range []struct {
		name      string
		temporary bool
		change    func(*Proxy, Destination)
	}{
		{name: "remove", change: func(p *Proxy, d Destination) { p.Remove(d) }},
		{name: "reconcile", change: func(p *Proxy, _ Destination) { _ = p.Reconcile(nil) }},
		{name: "expiry", temporary: true, change: func(p *Proxy, d Destination) {
			p.mu.Lock()
			id := p.grants[d.key()].id
			p.mu.Unlock()
			p.expireGrant(d.key(), id)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination := Destination{"allowed.example", 8443}
			dialer := &blockingDialer{entered: make(chan struct{}), release: make(chan struct{}), peer: make(chan net.Conn, 1)}
			declared := []Destination{destination}
			if test.temporary {
				declared = nil
			}
			p := New(declared, Options{Resolver: &resolver{}, Dialer: dialer})
			if test.temporary {
				if err := p.Grant(destination, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			client, server := net.Pipe()
			go p.handle(server)
			if _, err := fmt.Fprint(client, "CONNECT allowed.example:8443 HTTP/1.1\r\nHost: allowed.example:8443\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			<-dialer.entered
			test.change(p, destination)
			close(dialer.release)
			outboundPeer := <-dialer.peer
			defer outboundPeer.Close()
			if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(client).ReadString('\n')
			if err != nil || !strings.Contains(line, "403") {
				t.Fatalf("line=%q err=%v", line, err)
			}
			_ = outboundPeer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := outboundPeer.Read(make([]byte, 1)); err == nil {
				t.Fatal("stale outbound connection remained open")
			}
		})
	}
}

func TestReconcileRestoresDeclarationAndClearsTemporaryPolicy(t *testing.T) {
	declared := Destination{"declared.example", 443}
	temporary := Destination{"temporary.example", 8443}
	p := New([]Destination{declared}, Options{})
	p.Remove(declared)
	if _, ok := p.allowed(declared); ok {
		t.Fatal("removed declaration remained allowed")
	}
	if err := p.Grant(temporary, time.Minute); err != nil {
		t.Fatal(err)
	}
	admission, ok := p.allowed(temporary)
	if !ok {
		t.Fatal("temporary grant absent")
	}
	client, server := net.Pipe()
	defer client.Close()
	if _, current := p.trackIfCurrent(server, admission); !current {
		t.Fatal("admission unexpectedly stale")
	}
	if err := p.Reconcile([]Destination{declared}); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.allowed(declared); !ok {
		t.Fatal("declaration was not restored")
	}
	if _, ok := p.allowed(temporary); ok {
		t.Fatal("temporary grant survived declaration reconciliation")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection admitted by temporary policy survived reconciliation")
	}
}

func TestReconcileRejectsInvalidPolicyWithoutChangingCurrentPolicy(t *testing.T) {
	declared := Destination{"declared.example", 443}
	p := New([]Destination{declared}, Options{})
	if err := p.Reconcile([]Destination{{Host: "", Port: 443}}); err == nil {
		t.Fatal("invalid reconciliation succeeded")
	}
	if _, ok := p.allowed(declared); !ok {
		t.Fatal("invalid reconciliation changed current policy")
	}
}

func TestParseDestinationRequiresCanonicalAuthority(t *testing.T) {
	for _, test := range []struct {
		raw  string
		host string
		port int
	}{
		{raw: "example.com:443", host: "example.com", port: 443},
		{raw: "LOCALHOST.:80", host: "LOCALHOST.", port: 80},
		{raw: "[2001:db8::1]:8443", host: "2001:db8::1", port: 8443},
	} {
		got, err := ParseDestination(test.raw)
		if err != nil || got.Host != test.host || got.Port != test.port {
			t.Fatalf("ParseDestination(%q) = %#v, %v", test.raw, got, err)
		}
	}
	for _, raw := range []string{
		"", " example.com:443", "example.com:443 ", "example.com", "example.com:0",
		"example.com:65536", "example.com:0443", "user@example.com:443",
		"example.com:443/path", "example.com:443?query", "example.com:443#fragment",
		"https://example.com:443", "2001:db8::1:443", "*:443", "bad host:443",
		"bad\thost:443", "[not:ipv6]:443",
	} {
		if got, err := ParseDestination(raw); err == nil {
			t.Fatalf("ParseDestination(%q) = %#v", raw, got)
		}
	}
}

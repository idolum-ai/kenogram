package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type failingDialer struct{}

func (failingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("upstream unavailable")
}

type emptyResolver struct{}

func (emptyResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return nil, nil
}

func TestNetworkDiagnosticsDistinguishRefusedFromDialFailure(t *testing.T) {
	now := time.Date(2026, 7, 18, 1, 2, 3, 4, time.FixedZone("test", 3600))
	p := New([]Destination{{Host: "allowed.example", Port: 443}}, Options{
		Generation: 7,
		Now:        func() time.Time { return now },
		Resolver:   &resolver{},
		Dialer:     failingDialer{},
	})
	proxyRequest(t, p, "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n", "403")
	proxyRequest(t, p, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n", "502")

	snapshot := p.diagnostics.snapshot(10, MaxDiagnosticBytes)
	if snapshot.Generation != 7 || snapshot.Truncated || snapshot.Omitted != 0 || len(snapshot.Events) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Events[0].Outcome != "refused" || snapshot.Events[1].Outcome != "dial_failed" {
		t.Fatalf("events = %#v", snapshot.Events)
	}
	for _, event := range snapshot.Events {
		if event.Timestamp != "2026-07-18T00:02:03.000000004Z" || event.Generation != 7 {
			t.Fatalf("event identity = %#v", event)
		}
	}
}

func TestNetworkDiagnosticsClassifyEmptyResolutionAsDialFailure(t *testing.T) {
	p := New([]Destination{{Host: "empty.example", Port: 443}}, Options{Generation: 1, Resolver: emptyResolver{}})
	proxyRequest(t, p, "CONNECT empty.example:443 HTTP/1.1\r\nHost: empty.example:443\r\n\r\n", "502")
	snapshot := p.diagnostics.snapshot(1, MaxDiagnosticBytes)
	if len(snapshot.Events) != 1 || snapshot.Events[0].Outcome != "dial_failed" {
		t.Fatalf("diagnostic = %#v", snapshot)
	}
}

func TestNetworkDiagnosticsPrivacyCanary(t *testing.T) {
	p := New(nil, Options{Generation: 3})
	request := "GET http://USERINFO-CANARY@denied.example:8443/private/attachment?token=QUERY-CANARY HTTP/1.1\r\n" +
		"Host: denied.example:8443\r\nAuthorization: Bearer AUTH-CANARY\r\n" +
		"Proxy-Authorization: Basic PROXY-CANARY\r\nX-Secret: HEADER-CANARY\r\n\r\nBODY-CANARY"
	proxyRequest(t, p, request, "403")
	snapshot := p.diagnostics.snapshot(10, MaxDiagnosticBytes)
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"host":"denied.example"`)) || !bytes.Contains(raw, []byte(`"port":8443`)) {
		t.Fatalf("destination metadata absent: %s", raw)
	}
	for _, canary := range []string{"USERINFO-CANARY", "private", "attachment", "QUERY-CANARY", "AUTH-CANARY", "PROXY-CANARY", "HEADER-CANARY", "BODY-CANARY", "Authorization", "token"} {
		if bytes.Contains(raw, []byte(canary)) {
			t.Fatalf("diagnostic leaked %q: %s", canary, raw)
		}
	}
}

func TestNetworkDiagnosticsRejectInvalidUTF8AuthoritiesBeforeRecording(t *testing.T) {
	for _, value := range []byte{0xff, 0xfe} {
		p := New(nil, Options{Generation: 3})
		host := string([]byte{value}) + ".example"
		request := "CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n"
		proxyRequest(t, p, request, "400")
		if snapshot := p.diagnostics.snapshot(10, MaxDiagnosticBytes); len(snapshot.Events) != 0 {
			t.Fatalf("invalid byte %x produced evidence: %#v", value, snapshot)
		}
	}
}

func TestNetworkDiagnosticsAcceptGenuineReplacementRuneHost(t *testing.T) {
	p := New(nil, Options{Generation: 3})
	host := "\uFFFD.example"
	request := "CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n"
	proxyRequest(t, p, request, "403")
	snapshot := p.diagnostics.snapshot(10, MaxDiagnosticBytes)
	if len(snapshot.Events) != 1 || snapshot.Events[0].Host != host {
		t.Fatalf("replacement-rune evidence = %#v", snapshot)
	}
}

func TestNetworkDiagnosticsBoundsAndHonestOmission(t *testing.T) {
	p := New(nil, Options{Generation: 2})
	for index := 0; index < 6; index++ {
		p.diagnostics.record("refused", fmt.Sprintf("host-%d.example", index), 443)
	}
	snapshot := p.diagnostics.snapshot(2, MaxDiagnosticBytes)
	if len(snapshot.Events) != 2 || snapshot.Events[0].Host != "host-4.example" || snapshot.Events[1].Host != "host-5.example" {
		t.Fatalf("recent events = %#v", snapshot.Events)
	}
	if !snapshot.Truncated || snapshot.Omitted != 4 || snapshot.EncodedBytes <= 0 {
		t.Fatalf("bounded snapshot = %#v", snapshot)
	}

	tiny := p.diagnostics.snapshot(MaxDiagnosticLimit, 1)
	if len(tiny.Events) != 0 || !tiny.Truncated || tiny.Omitted != 6 || tiny.EncodedBytes != 0 {
		t.Fatalf("byte-bounded snapshot = %#v", tiny)
	}
}

func TestNetworkDiagnosticRecordingNeverWaitsForReader(t *testing.T) {
	p := New(nil, Options{Generation: 1})
	p.diagnostics.mu.Lock()
	done := make(chan struct{})
	go func() {
		p.diagnostics.record("refused", "denied.example", 443)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("traffic observation blocked behind diagnostic reader")
	}
	p.diagnostics.mu.Unlock()
	snapshot := p.diagnostics.snapshot(1, MaxDiagnosticBytes)
	if !snapshot.Truncated || snapshot.Omitted != 1 {
		t.Fatalf("contention was not reported: %#v", snapshot)
	}
}

type blockingLogWriter struct{ release chan struct{} }

func (w blockingLogWriter) Write(buffer []byte) (int, error) {
	<-w.release
	return len(buffer), nil
}

func TestMetadataLogBackpressureNeverBlocksProxy(t *testing.T) {
	release := make(chan struct{})
	p := New(nil, Options{Logger: log.New(blockingLogWriter{release: release}, "", 0)})
	done := make(chan struct{})
	go func() {
		for index := 0; index < MaxDiagnosticLimit*4; index++ {
			p.logf("message %d", index)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("proxy blocked behind metadata log writer")
	}
	close(release)
}

type formattingCanary struct{ calls atomic.Int32 }

func (c *formattingCanary) String() string {
	c.calls.Add(1)
	return "formatted"
}

func TestDroppedMetadataLogDoesNotFormat(t *testing.T) {
	p := &Proxy{logMessages: make(chan logMessage, 1)}
	p.logMessages <- logMessage{format: "occupied"}
	canary := &formattingCanary{}
	p.logf("%s", canary)
	if got := canary.calls.Load(); got != 0 {
		t.Fatalf("dropped log formatted %d times", got)
	}
}

func TestDiagnosticControlResponseIsOneSnapshot(t *testing.T) {
	p := New(nil, Options{Generation: 9})
	p.diagnostics.record("refused", "denied.example", 443)
	client, server := net.Pipe()
	go p.control(server)
	response, err := exchangeControlContext(context.Background(), client, ControlRequest{Operation: "network-diagnostics", Limit: 1, MaxBytes: MaxDiagnosticBytes})
	if err != nil {
		t.Fatal(err)
	}
	if response.Diagnostics == nil || response.Diagnostics.Generation != 9 || len(response.Diagnostics.Events) != 1 {
		t.Fatalf("control response = %#v", response)
	}
}

func proxyRequest(t *testing.T, p *Proxy, request, wantStatus string) {
	t.Helper()
	client, server := net.Pipe()
	go p.handle(server)
	defer client.Close()
	if _, err := fmt.Fprint(client, request); err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil || !strings.Contains(line, wantStatus) {
		t.Fatalf("response = %q, %v; want %s", line, err, wantStatus)
	}
}

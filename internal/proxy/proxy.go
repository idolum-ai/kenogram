// Package proxy implements Kenogram's host-side, exact-destination door.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Destination struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (d Destination) key() string {
	return strings.ToLower(strings.TrimSuffix(d.Host, ".")) + ":" + strconv.Itoa(d.Port)
}

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}
type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}
type Options struct {
	MaxConnections       int
	ConnectionsPerSecond int
	Resolver             Resolver
	Dialer               Dialer
	Logger               *log.Logger
}
type grant struct {
	destination Destination
	expires     time.Time
	id          string
}
type tracked struct {
	conn      net.Conn
	admission string
}
type Proxy struct {
	mu         sync.Mutex
	durable    map[string]string
	grants     map[string]grant
	active     map[uint64]tracked
	next       uint64
	opts       Options
	sem        chan struct{}
	rateMu     sync.Mutex
	rateWindow time.Time
	rateCount  int
}

func New(destinations []Destination, opts Options) *Proxy {
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = 128
	}
	if opts.ConnectionsPerSecond <= 0 {
		opts.ConnectionsPerSecond = 64
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.Dialer == nil {
		opts.Dialer = &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}
	p := &Proxy{durable: map[string]string{}, grants: map[string]grant{}, active: map[uint64]tracked{}, opts: opts, sem: make(chan struct{}, opts.MaxConnections)}
	for _, d := range destinations {
		p.durable[d.key()] = "declaration:" + d.key()
	}
	return p
}

func (p *Proxy) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		if !p.admitRate() {
			conn.Close()
			continue
		}
		select {
		case p.sem <- struct{}{}:
			go func() { defer func() { <-p.sem }(); p.handle(conn) }()
		default:
			conn.Close()
		}
	}
}
func (p *Proxy) admitRate() bool {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	now := time.Now()
	if p.rateWindow.IsZero() || now.Sub(p.rateWindow) >= time.Second {
		p.rateWindow = now
		p.rateCount = 0
	}
	if p.rateCount >= p.opts.ConnectionsPerSecond {
		return false
	}
	p.rateCount++
	return true
}
func (p *Proxy) handle(client net.Conn) {
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(io.LimitReader(client, 64<<10))
	request, err := http.ReadRequest(reader)
	if err != nil {
		writeError(client, http.StatusBadRequest)
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	host, port, err := requestDestination(request)
	if err != nil {
		writeError(client, http.StatusBadRequest)
		return
	}
	admission, ok := p.allowed(Destination{host, port})
	if !ok {
		p.opts.Logger.Printf("outcome=refused host=%q port=%d", host, port)
		writeError(client, http.StatusForbidden)
		return
	}
	outbound, address, err := p.dialResolved(request.Context(), host, port)
	if err != nil {
		p.opts.Logger.Printf("outcome=dial_failed host=%q port=%d", host, port)
		writeError(client, http.StatusBadGateway)
		return
	}
	defer outbound.Close()
	id := p.track(client, admission)
	defer p.untrack(id)
	p.opts.Logger.Printf("outcome=connected host=%q port=%d address=%q", host, port, address)
	if request.Method == http.MethodConnect {
		if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		relay(&bufferedConn{Conn: client, reader: reader}, outbound)
		return
	}
	request.RequestURI = ""
	request.URL.Scheme = ""
	request.URL.Host = ""
	request.Header.Del("Proxy-Authorization")
	request.Header.Del("Proxy-Connection")
	request.Close = true
	if err := request.Write(outbound); err != nil {
		return
	}
	relay(client, outbound)
}

type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(buffer []byte) (int, error) { return c.reader.Read(buffer) }
func requestDestination(r *http.Request) (string, int, error) {
	var authority string
	if r.Method == http.MethodConnect {
		authority = r.Host
	} else {
		if r.URL == nil || !r.URL.IsAbs() {
			return "", 0, fmt.Errorf("absolute URI required")
		}
		authority = r.URL.Host
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil {
		return "", 0, fmt.Errorf("destination must include port: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return "", 0, fmt.Errorf("empty host")
	}
	return host, port, nil
}
func (p *Proxy) allowed(d Destination) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, g := range p.grants {
		if !g.expires.After(now) {
			delete(p.grants, key)
		}
	}
	if id, ok := p.durable[d.key()]; ok {
		return id, true
	}
	g, ok := p.grants[d.key()]
	return g.id, ok
}
func (p *Proxy) dialResolved(ctx context.Context, host string, port int) (net.Conn, string, error) {
	ips, err := p.opts.Resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, "", err
	}
	var errs []error
	for _, ip := range ips {
		address := net.JoinHostPort(ip.IP.String(), strconv.Itoa(port))
		conn, err := p.opts.Dialer.DialContext(ctx, "tcp", address)
		if err == nil {
			return conn, address, nil
		}
		errs = append(errs, err)
	}
	return nil, "", errors.Join(errs...)
}
func (p *Proxy) track(conn net.Conn, admission string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	p.active[p.next] = tracked{conn, admission}
	return p.next
}
func (p *Proxy) untrack(id uint64) { p.mu.Lock(); delete(p.active, id); p.mu.Unlock() }
func (p *Proxy) Grant(d Destination, duration time.Duration) error {
	if d.Host == "" || d.Port < 1 || d.Port > 65535 || duration <= 0 {
		return fmt.Errorf("invalid grant")
	}
	p.mu.Lock()
	key := d.key()
	id := "grant:" + key + ":" + strconv.FormatInt(time.Now().UnixNano(), 10)
	p.grants[key] = grant{d, time.Now().Add(duration), id}
	p.mu.Unlock()
	time.AfterFunc(duration, func() { p.expireGrant(key, id) })
	return nil
}

func (p *Proxy) expireGrant(key, id string) {
	p.mu.Lock()
	grant, ok := p.grants[key]
	if !ok || grant.id != id {
		p.mu.Unlock()
		return
	}
	delete(p.grants, key)
	for _, active := range p.active {
		if active.admission == id {
			_ = active.conn.Close()
		}
	}
	p.mu.Unlock()
}
func (p *Proxy) Remove(d Destination) {
	key := d.key()
	p.mu.Lock()
	ids := map[string]bool{}
	if id, ok := p.durable[key]; ok {
		ids[id] = true
		delete(p.durable, key)
	}
	if g, ok := p.grants[key]; ok {
		ids[g.id] = true
		delete(p.grants, key)
	}
	for _, active := range p.active {
		if ids[active.admission] {
			active.conn.Close()
		}
	}
	p.mu.Unlock()
}
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(b, a)
		if tcp, ok := b.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(a, b)
		if tcp, ok := a.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
}
func writeError(w io.Writer, status int) {
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status))
}
func ParseDestination(raw string) (Destination, error) {
	parsed, err := url.Parse("door://" + raw)
	if err != nil {
		return Destination{}, err
	}
	host := parsed.Hostname()
	port, err := strconv.Atoi(parsed.Port())
	if host == "" || err != nil || port < 1 || port > 65535 {
		return Destination{}, fmt.Errorf("destination must be host:port")
	}
	return Destination{host, port}, nil
}

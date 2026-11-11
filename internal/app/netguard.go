package app

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Network modes.
const (
	ModeAirlock = "airlock" // nothing leaves the machine
	ModeHub     = "hub"     // only model registries + engine downloads
	ModeWeb     = "web"     // hub + privacy web search
)

var hubHosts = []string{
	"ollama.com", "ollama.ai", "registry.ollama.ai", // model registry
	"r2.cloudflarestorage.com", "cloudflarestorage.com", // registry blob storage
	"github.com", "api.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com", // engine downloads
}

var webHosts = []string{"html.duckduckgo.com", "duckduckgo.com"}

// NetEvent is one entry in the visible network log.
type NetEvent struct {
	Time    string `json:"time"`
	Host    string `json:"host"`
	Allowed bool   `json:"allowed"`
	Source  string `json:"source"`
}

// NetGuard enforces the network mode for the launcher AND for Ollama
// (Ollama is forced to use our filtering proxy via HTTPS_PROXY).
type NetGuard struct {
	mu     sync.Mutex
	mode   string
	log    []NetEvent
	proxy  net.Listener
	client *http.Client
}

func NewNetGuard() *NetGuard {
	g := &NetGuard{mode: ModeAirlock}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment // respect corporate/host proxies upstream
	g.client = &http.Client{Transport: &guardTransport{g: g, next: tr}, Timeout: 0}
	return g
}

func (g *NetGuard) Mode() string { g.mu.Lock(); defer g.mu.Unlock(); return g.mode }

func (g *NetGuard) SetMode(m string) error {
	if m != ModeAirlock && m != ModeHub && m != ModeWeb {
		return fmt.Errorf("unknown network mode %q", m)
	}
	g.mu.Lock()
	g.mode = m
	g.mu.Unlock()
	return nil
}

func (g *NetGuard) Log() []NetEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]NetEvent, len(g.log))
	copy(out, g.log)
	return out
}

func hostMatches(host string, list []string) bool {
	host = strings.ToLower(host)
	for _, h := range list {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// Allow decides whether a connection to host may happen and records it.
func (g *NetGuard) Allow(host, source string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ok := false
	switch g.mode {
	case ModeHub:
		ok = hostMatches(host, hubHosts)
	case ModeWeb:
		ok = hostMatches(host, hubHosts) || hostMatches(host, webHosts)
	}
	ev := NetEvent{Time: time.Now().Format("15:04:05"), Host: host, Allowed: ok, Source: source}
	// collapse repeated identical events
	if n := len(g.log); n > 0 && g.log[n-1].Host == host && g.log[n-1].Allowed == ok && g.log[n-1].Source == source {
		g.log[n-1].Time = ev.Time
	} else {
		g.log = append(g.log, ev)
		if len(g.log) > 200 {
			g.log = g.log[len(g.log)-200:]
		}
	}
	return ok
}

// Client returns an HTTP client whose every request is checked by the guard.
func (g *NetGuard) Client() *http.Client { return g.client }

type guardTransport struct {
	g    *NetGuard
	next http.RoundTripper
}

var ErrBlocked = errors.New("blocked by network mode")

func (t *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !t.g.Allow(r.URL.Host, "launcher") {
		return nil, fmt.Errorf("%w: %s (current mode: %s)", ErrBlocked, r.URL.Hostname(), t.g.Mode())
	}
	return t.next.RoundTrip(r)
}

// StartProxy runs a tiny HTTP/CONNECT proxy on localhost that Ollama is forced
// to use. Requests are allowed only when the mode permits the host.
func (g *NetGuard) StartProxy() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	g.proxy = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go g.handleProxyConn(c)
		}
	}()
	return "http://" + ln.Addr().String(), nil
}

func (g *NetGuard) Close() {
	if g.proxy != nil {
		g.proxy.Close()
	}
}

func (g *NetGuard) handleProxyConn(c net.Conn) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	host := req.Host
	if req.Method != http.MethodConnect && req.URL != nil && req.URL.Host != "" {
		host = req.URL.Host
	}
	if !g.Allow(host, "ollama") {
		fmt.Fprintf(c, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\nUSB-AI network mode (%s) blocks %s\n", g.Mode(), host)
		return
	}
	if req.Method == http.MethodConnect {
		up, err := dialUpstream(req.Host)
		if err != nil {
			fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
			return
		}
		defer up.Close()
		fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		pipe(c, br, up)
		return
	}
	// plain HTTP forward
	req.RequestURI = ""
	resp, err := g.client.Transport.(*guardTransport).next.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer resp.Body.Close()
	_ = resp.Write(c)
}

// dialUpstream connects to target, chaining through the host's own proxy
// (HTTPS_PROXY) if one is configured.
func dialUpstream(target string) (net.Conn, error) {
	pu, _ := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: target}})
	if pu == nil {
		return net.DialTimeout("tcp", target, 20*time.Second)
	}
	conn, err := net.DialTimeout("tcp", pu.Host, 20*time.Second)
	if err != nil {
		return nil, err
	}
	auth := ""
	if pu.User != nil {
		auth = "Proxy-Authorization: Basic " + basicAuth(pu.User) + "\r\n"
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, auth)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("upstream proxy refused CONNECT")
	}
	if br.Buffered() > 0 {
		return &bufConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func basicAuth(u *url.Userinfo) string {
	p, _ := u.Password()
	return encodeBase64(u.Username() + ":" + p)
}

func pipe(client net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
}

func encodeBase64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

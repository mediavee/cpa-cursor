package cursor

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// userAgent is what connect-es 1.6.1 sends, as the official CLI does.
const userAgent = "connect-es/1.6.1"

// Client talks to Cursor. It is safe for concurrent use and meant to be
// shared: it owns the HTTP/2 connection pool of the agent stream.
type Client struct {
	cfg   Config
	clock Clock
	tls   *tls.Config
	// proxy picks the HTTP proxy for a request; http.ProxyFromEnvironment by default.
	proxy func(*http.Request) (*url.URL, error)

	h2    *http2.Transport
	unary *http.Client

	mu          sync.Mutex
	serverCfg   *ServerConfig
	serverCfgAt time.Time
}

// Option customises a Client.
type Option func(*Client)

// WithClock replaces the system clock.
func WithClock(clock Clock) Option { return func(c *Client) { c.clock = clock } }

// WithTLSConfig sets the TLS configuration of every connection, for example
// to trust a test upstream's certificate.
func WithTLSConfig(cfg *tls.Config) Option { return func(c *Client) { c.tls = cfg } }

// NewClient validates cfg and builds a client.
func NewClient(cfg Config, opts ...Option) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("cursor: invalid config: %w", err)
	}
	c := &Client{cfg: cfg, clock: SystemClock{}, proxy: http.ProxyFromEnvironment}
	for _, opt := range opts {
		opt(c)
	}
	// The CLI keeps agent connections alive with a ping every 10 s and gives
	// up on a connection that does not answer within 20 s.
	c.h2 = &http2.Transport{
		DialTLSContext:     c.dialTLS,
		ReadIdleTimeout:    10 * time.Second,
		PingTimeout:        20 * time.Second,
		DisableCompression: true,
	}
	// The CLI sends its unary RPCs over HTTP/1.1.
	c.unary = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:              func(r *http.Request) (*url.URL, error) { return c.proxy(r) },
			TLSClientConfig:    c.tls,
			TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
			DisableCompression: true,
			IdleConnTimeout:    90 * time.Second,
		},
	}
	return c, nil
}

func (c *Client) Config() Config { return c.cfg }

// CloseIdleConnections drops pooled connections.
func (c *Client) CloseIdleConnections() {
	c.h2.CloseIdleConnections()
	c.unary.CloseIdleConnections()
}

// agentURL is the host of the Run stream: the one GetServerConfig announced
// if it was fetched, the configured one otherwise.
func (c *Client) agentURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.serverCfg == nil {
		return c.cfg.AgentURL
	}
	if u := c.serverCfg.AgentURL(c.cfg.GhostMode); u != "" {
		return u
	}
	return c.cfg.AgentURL
}

// request builds the Run request. The body stays open for the whole session.
func (s *Session) request(token string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.c.agentURL()+"/agent.v1.AgentService/Run", body)
	if err != nil {
		return nil, fmt.Errorf("cursor: build run request: %w", err)
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/connect+proto")
	h.Set("Connect-Protocol-Version", "1")
	h.Set("Connect-Content-Encoding", "gzip")
	h.Set("Connect-Accept-Encoding", "gzip")
	h.Set("X-Ghost-Mode", fmt.Sprint(s.c.cfg.GhostMode))
	h.Set("X-Cursor-Client-Version", s.c.cfg.ClientVersion)
	h.Set("X-Cursor-Client-Type", "cli")
	h.Set("X-Request-Id", s.params.RequestID)
	h.Set("X-Original-Request-Id", s.params.OriginalRequestID)
	h.Set("X-Blob-Encryption-Key", s.params.BlobEncryptionKey)
	h.Set("User-Agent", userAgent)
	return req, nil
}

// dialTLS dials addr over TLS, through the environment's HTTPS proxy if any.
func (c *Client) dialTLS(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
	if c.tls != nil {
		merged := c.tls.Clone()
		merged.NextProtos = cfg.NextProtos
		merged.ServerName = cfg.ServerName
		cfg = merged
	}
	proxyURL, err := c.proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil {
		return nil, fmt.Errorf("cursor: proxy lookup: %w", err)
	}
	var raw net.Conn
	if proxyURL == nil {
		raw, err = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	} else {
		raw, err = dialViaProxy(ctx, proxyURL, addr)
	}
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// dialViaProxy opens a CONNECT tunnel to addr through an HTTP proxy.
func dialViaProxy(ctx context.Context, proxyURL *url.URL, addr string) (net.Conn, error) {
	if proxyURL.Scheme != "http" {
		return nil, fmt.Errorf("cursor: unsupported proxy scheme %q", proxyURL.Scheme)
	}
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), "80")
	}
	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("cursor: dial proxy: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if u := proxyURL.User; u != nil {
		pass, _ := u.Password()
		req.SetBasicAuth(u.Username(), pass)
		req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
		req.Header.Del("Authorization")
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cursor: proxy CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req) //nolint:bodyclose // a CONNECT response has no body
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("cursor: proxy CONNECT: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("cursor: proxy CONNECT: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return nil, errors.New("cursor: proxy sent data before the tunnel was used")
	}
	return conn, nil
}

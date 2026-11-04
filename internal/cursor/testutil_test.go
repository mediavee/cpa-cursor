package cursor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

const waitTimeout = 5 * time.Second

// fakeClock is a manual Clock. Timers fire only when Advance moves time.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c      *fakeClock
	when   time.Time
	ch     chan time.Time
	f      func()
	active bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) add(d time.Duration, f func()) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, when: c.now.Add(d), ch: make(chan time.Time, 1), f: f, active: true}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) NewTimer(d time.Duration) Timer            { return c.add(d, nil) }
func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer { return c.add(d, f) }

func (t *fakeTimer) C() <-chan time.Time {
	if t.f != nil {
		return nil
	}
	return t.ch
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.when, t.active = t.c.now.Add(d), true
	select {
	case <-t.ch:
	default:
	}
	return was
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	select {
	case <-t.ch:
	default:
	}
	return was
}

// Advance moves time forward, firing the timers that come due in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.active && !t.when.After(target) && (next == nil || t.when.Before(next.when)) {
				next = t
			}
		}
		if next == nil {
			break
		}
		c.now = next.when
		next.active = false
		if next.f != nil {
			go next.f()
		} else {
			select {
			case next.ch <- c.now:
			default:
			}
		}
	}
	c.now = target
	c.mu.Unlock()
}

// active counts the timers waiting to fire.
func (c *fakeClock) active() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// clientFrame is one decoded client message with the flags of its frame.
type clientFrame struct {
	flags byte
	msg   *agentv1.AgentClientMessage
}

// fakeStream is the server side of one Run stream.
type fakeStream struct {
	t   *testing.T
	req *http.Request
	w   http.ResponseWriter
	in  chan clientFrame
	wmu sync.Mutex
	// done is set, under wmu, once the handler has returned: net/http forbids touching w after that.
	done    bool
	release chan struct{}
	once    sync.Once
	// ctxDone is closed when the request context ends, that is when the client closes the stream.
	ctxDone chan struct{}
}

func (f *fakeStream) writeFrame(flags byte, payload []byte) {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if f.done {
		return
	}
	hdr := make([]byte, 5)
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	_, _ = f.w.Write(append(hdr, payload...))
	f.w.(http.Flusher).Flush()
}

// send writes a server message; gz forces a gzip frame.
func (f *fakeStream) send(m *agentv1.AgentServerMessage, gz bool) {
	f.t.Helper()
	payload, err := proto.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	if gz {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		f.writeFrame(0x01, buf.Bytes())
		return
	}
	f.writeFrame(0, payload)
}

// end writes the end-of-stream frame with the given JSON and finishes the response.
func (f *fakeStream) end(js string) {
	f.writeFrame(0x02, []byte(js))
	f.finish()
}

func (f *fakeStream) finish() { f.once.Do(func() { close(f.release) }) }

func (f *fakeStream) recv() clientFrame {
	f.t.Helper()
	select {
	case cf, ok := <-f.in:
		if !ok {
			f.t.Fatal("client stream closed while waiting for a message")
		}
		return cf
	case <-time.After(waitTimeout):
		f.t.Fatal("timed out waiting for a client message")
	}
	return clientFrame{}
}

// recvNonHeartbeat skips client heartbeats.
func (f *fakeStream) recvNonHeartbeat() clientFrame {
	f.t.Helper()
	for {
		cf := f.recv()
		if cf.msg.GetClientHeartbeat() == nil {
			return cf
		}
	}
}

// expectNone asserts that no non-heartbeat message arrives within a short real delay.
func (f *fakeStream) expectNone() {
	f.t.Helper()
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case cf := <-f.in:
			if cf.msg.GetClientHeartbeat() == nil {
				f.t.Fatalf("unexpected client message: %v", cf.msg)
			}
		case <-deadline:
			return
		}
	}
}

type fakeUpstream struct {
	t       *testing.T
	srv     *httptest.Server
	streams chan *fakeStream
	pool    *x509.CertPool
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{t: t, streams: make(chan *fakeStream, 8)}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(u.serve))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u.srv = srv
	u.pool = x509.NewCertPool()
	u.pool.AddCert(srv.Certificate())
	return u
}

func (u *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agent.v1.AgentService/Run" {
		http.NotFound(w, r)
		return
	}
	if r.ProtoMajor != 2 {
		u.t.Errorf("run request over HTTP/%d, want HTTP/2", r.ProtoMajor)
	}
	fs := &fakeStream{
		t: u.t, req: r, w: w,
		in:      make(chan clientFrame, 256),
		release: make(chan struct{}),
		ctxDone: make(chan struct{}),
	}
	go func() {
		<-r.Context().Done()
		close(fs.ctxDone)
	}()
	go func() {
		defer close(fs.in)
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(r.Body, hdr[:]); err != nil {
				return
			}
			payload := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
			if _, err := io.ReadFull(r.Body, payload); err != nil {
				return
			}
			if hdr[0]&0x01 != 0 {
				zr, err := gzip.NewReader(bytes.NewReader(payload))
				if err != nil {
					u.t.Errorf("client sent a bad gzip frame: %v", err)
					return
				}
				payload, _ = io.ReadAll(zr)
			}
			var m agentv1.AgentClientMessage
			if err := proto.Unmarshal(payload, &m); err != nil {
				u.t.Errorf("client sent a bad message: %v", err)
				return
			}
			fs.in <- clientFrame{flags: hdr[0], msg: &m}
		}
	}()
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	defer func() {
		fs.wmu.Lock()
		fs.done = true
		fs.wmu.Unlock()
	}()
	u.streams <- fs
	select {
	case <-fs.release:
	case <-r.Context().Done():
	}
}

// accept waits for the next Run stream.
func (u *fakeUpstream) accept() *fakeStream {
	u.t.Helper()
	select {
	case fs := <-u.streams:
		return fs
	case <-time.After(waitTimeout):
		u.t.Fatal("no Run stream reached the fake upstream")
	}
	return nil
}

func clientTLS(u *fakeUpstream) *tls.Config { return &tls.Config{RootCAs: u.pool} }

func (u *fakeUpstream) config() Config {
	cfg := DefaultConfig()
	cfg.APIURL, cfg.AgentURL, cfg.SiteURL = u.srv.URL, u.srv.URL, u.srv.URL
	return cfg
}

func (u *fakeUpstream) client(t *testing.T, clk Clock, opts ...Option) *Client {
	t.Helper()
	opts = append([]Option{WithClock(clk), WithTLSConfig(&tls.Config{RootCAs: u.pool})}, opts...)
	c, err := NewClient(u.config(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// Message builders.

func updateMsg(u *agentv1.InteractionUpdate) *agentv1.AgentServerMessage {
	return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionUpdate{InteractionUpdate: u}}
}

func textMsg(s string) *agentv1.AgentServerMessage {
	return updateMsg(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: s}}})
}

func turnEndedMsg(in, out int64) *agentv1.AgentServerMessage {
	return updateMsg(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{InputTokens: &in, OutputTokens: &out}}})
}

func listedMsg(n uint32) *agentv1.AgentServerMessage {
	return updateMsg(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_ToolRequestsListed{ToolRequestsListed: &agentv1.ToolRequestsListedUpdate{CallCount: n}}})
}

func execMsg(id uint32, execID string, set func(*agentv1.ExecServerMessage)) *agentv1.AgentServerMessage {
	m := &agentv1.ExecServerMessage{Id: id, ExecId: execID}
	set(m)
	return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_ExecServerMessage{ExecServerMessage: m}}
}

func mcpArgsMsg(id uint32, callID, tool string) *agentv1.AgentServerMessage {
	return execMsg(id, "exec-"+callID, func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_McpArgs{McpArgs: &agentv1.McpArgs{
			Name: "client-" + tool, ToolName: tool, ProviderIdentifier: "client", ToolCallId: callID,
		}}
	})
}

func testTool(name string) *agentv1.McpToolDefinition {
	return &agentv1.McpToolDefinition{Name: "client-" + name, ProviderIdentifier: "client", ToolName: name, Description: "test tool " + name}
}

func runRequest(text string) *agentv1.AgentRunRequest {
	return &agentv1.AgentRunRequest{
		Action: &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: &agentv1.UserMessageAction{
			UserMessage: &agentv1.UserMessage{Text: text, MessageId: "m-1"},
		}}},
		ModelDetails: &agentv1.ModelDetails{ModelId: "test-model"},
	}
}

// nextEvent reads one event with a real-time bound.
func nextEvent(t *testing.T, s *Session) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

func nextErr(t *testing.T, s *Session) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	ev, err := s.Next(ctx)
	if err == nil {
		t.Fatalf("Next returned event %T, want an error", ev)
	}
	return err
}

// drain reads events until the terminal error.
func drain(t *testing.T, s *Session) ([]Event, error) {
	t.Helper()
	var evs []Event
	for {
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		ev, err := s.Next(ctx)
		cancel()
		if err != nil {
			return evs, err
		}
		evs = append(evs, ev)
	}
}

func lowerKeys(h http.Header) []string {
	var keys []string
	for k := range h {
		keys = append(keys, strings.ToLower(k))
	}
	slices.Sort(keys)
	return keys
}

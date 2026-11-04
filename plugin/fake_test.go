package plugin

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/mediavee/cpa-cursor/internal/cursor"
	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	aiserverv1 "github.com/mediavee/cpa-cursor/internal/gen/aiserver/v1"
)

const waitTimeout = 5 * time.Second

// Every value below is invented: no token, account or answer of a real Cursor account.

const (
	usableModelsJSON = `{"models":[
		{"modelId":"default","displayModelId":"auto","displayName":"Auto"},
		{"modelId":"composer-x","displayName":"Composer X"},
		{"modelId":"composer-x-fast","displayName":"Composer X Fast"},
		{"modelId":"claude-t-low","displayName":"Claude T Low"},
		{"modelId":"claude-t-high","displayName":"Claude T High","maxMode":true}]}`

	availableModelsJSON = `{"models":[
		{"name":"composer-x","contextTokenLimit":200000,"contextTokenLimitForMaxMode":200000,
		 "parameterDefinitions":[{"id":"fast","parameterType":{"booleanParameter":{"values":[{"value":"false"},{"value":"true"}]}}}],
		 "variants":[{"parameterValues":[{"id":"fast","value":"true"}],"legacySlug":"composer-x-fast"},
		             {"parameterValues":[{"id":"fast","value":"false"}],"legacySlug":"composer-x"}]},
		{"name":"claude-t","supportsImages":true,"contextTokenLimit":300000,"contextTokenLimitForMaxMode":1000000,
		 "parameterDefinitions":[{"id":"effort","parameterType":{"enumParameter":{"values":[{"value":"low"},{"value":"high"}]}}}],
		 "variants":[{"parameterValues":[{"id":"effort","value":"low"}],"legacySlug":"claude-t-low"},
		             {"parameterValues":[{"id":"effort","value":"high"}],"legacySlug":"claude-t-high","isMaxMode":true}]}]}`

	periodUsageJSON = `{"billingCycleStart":"1791810920000","billingCycleEnd":"1794489320000",
		"planUsage":{"totalSpend":12000,"includedSpend":10000,"bonusSpend":2000,"limit":20000,
		             "autoPercentUsed":60,"apiPercentUsed":25,"totalPercentUsed":50},
		"autoBucketModels":["default","composer-x","composer-x-fast"]}`
	planInfoJSON = `{"planInfo":{"planName":"Ultra","price":"$200/mo","billingCycleEnd":"1794489320000"}}`
	noLimitJSON  = `{"usageLimitPolicyStatus":{}}`
)

// makeJWT builds an unsigned token with the claims the plugin reads.
func makeJWT(exp time.Time, sub string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(map[string]any{"sub": sub, "exp": exp.Unix()}) + ".sig"
}

// fakeCursor plays api2 and the agent host: unary Connect JSON over HTTP/1.1 and the Run bidi stream
// over HTTP/2.
type fakeCursor struct {
	t    *testing.T
	srv  *httptest.Server
	pool *x509.CertPool

	mu      sync.Mutex
	unary   map[string]func(w http.ResponseWriter, r *http.Request)
	counts  map[string]int
	script  func(*fakeRun)
	runs    []*fakeRun
	runSeen chan *fakeRun
}

func newFakeCursor(t *testing.T) *fakeCursor {
	t.Helper()
	f := &fakeCursor{t: t, counts: map[string]int{}, unary: map[string]func(http.ResponseWriter, *http.Request){}, runSeen: make(chan *fakeRun, 16)}
	f.reply("/agent.v1.AgentService/GetUsableModels", usableModelsJSON)
	f.reply("/aiserver.v1.AiService/AvailableModels", availableModelsJSON)
	f.reply("/aiserver.v1.DashboardService/GetCurrentPeriodUsage", periodUsageJSON)
	f.reply("/aiserver.v1.DashboardService/GetPlanInfo", planInfoJSON)
	f.reply("/aiserver.v1.DashboardService/GetUsageLimitStatusAndActiveGrants", noLimitJSON)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.srv = srv
	f.pool = x509.NewCertPool()
	f.pool.AddCert(srv.Certificate())
	return f
}

// reply sets a fixed JSON answer for a path.
func (f *fakeCursor) reply(path, js string) { f.replyStatus(path, http.StatusOK, js) }

func (f *fakeCursor) replyStatus(path string, status int, js string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unary[path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, js)
	}
}

func (f *fakeCursor) handle(path string, h func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unary[path] = h
}

func (f *fakeCursor) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}

func (f *fakeCursor) setScript(s func(*fakeRun)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = s
}

func (f *fakeCursor) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeCursor) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.counts[r.URL.Path]++
	h := f.unary[r.URL.Path]
	f.mu.Unlock()
	if r.URL.Path == "/agent.v1.AgentService/Run" {
		f.serveRun(w, r)
		return
	}
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

// url is the https address the plugin is configured with for api, agent and site.
func (f *fakeCursor) url() string { return f.srv.URL }

// configYAML is plugins.configs.cursor pointing at the fake.
func (f *fakeCursor) configYAML(extra ...string) []byte {
	lines := []string{"api_url: " + f.url(), "agent_url: " + f.url(), "site_url: " + f.url()}
	return []byte(strings.Join(append(lines, extra...), "\n"))
}

type fakeRun struct {
	t       *testing.T
	f       *fakeCursor
	w       http.ResponseWriter
	req     *http.Request
	header  http.Header
	request *agentv1.AgentRunRequest
	in      chan *agentv1.AgentClientMessage
	wmu     sync.Mutex
	gone    chan struct{}
}

func (f *fakeCursor) serveRun(w http.ResponseWriter, r *http.Request) {
	run := &fakeRun{t: f.t, f: f, w: w, req: r, header: r.Header.Clone(), in: make(chan *agentv1.AgentClientMessage, 64), gone: make(chan struct{})}
	go func() {
		<-r.Context().Done()
		close(run.gone)
	}()
	go func() {
		defer close(run.in)
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(r.Body, hdr[:]); err != nil {
				return
			}
			payload := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
			if _, err := io.ReadFull(r.Body, payload); err != nil {
				return
			}
			if hdr[0]&1 != 0 {
				zr, err := gzip.NewReader(bytes.NewReader(payload))
				if err != nil {
					return
				}
				payload, _ = io.ReadAll(zr)
			}
			var m agentv1.AgentClientMessage
			if proto.Unmarshal(payload, &m) != nil {
				return
			}
			run.in <- &m
		}
	}()
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()

	// The first message is the run request.
	first := run.recv()
	run.request = first.GetRunRequest()
	f.mu.Lock()
	f.runs = append(f.runs, run)
	script := f.script
	f.mu.Unlock()
	f.runSeen <- run
	if script == nil {
		script = func(r *fakeRun) { r.text("ok"); r.turnEnded(3, 2) }
	}
	script(run)
	run.frame(0x02, []byte(`{}`))
}

func (r *fakeRun) frame(flags byte, payload []byte) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	hdr := make([]byte, 5)
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	_, _ = r.w.Write(append(hdr, payload...))
	r.w.(http.Flusher).Flush()
}

func (r *fakeRun) send(m *agentv1.AgentServerMessage) {
	b, err := proto.Marshal(m)
	if err != nil {
		r.t.Error(err)
		return
	}
	r.frame(0, b)
}

func (r *fakeRun) update(u *agentv1.InteractionUpdate) {
	r.send(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionUpdate{InteractionUpdate: u}})
}

func (r *fakeRun) text(s string) {
	r.update(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: s}}})
}

func (r *fakeRun) turnEnded(in, out int64) {
	r.update(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{InputTokens: &in, OutputTokens: &out}}})
}

// toolCall asks the client for one tool call, as a step with a single call.
func (r *fakeRun) toolCall(id uint32, callID, tool, city string) {
	r.update(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_ToolRequestsListed{ToolRequestsListed: &agentv1.ToolRequestsListedUpdate{CallCount: 1}}})
	args, _ := structpb.NewValue(city)
	r.send(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_ExecServerMessage{ExecServerMessage: &agentv1.ExecServerMessage{
		Id: id, ExecId: "exec-" + callID,
		Message: &agentv1.ExecServerMessage_McpArgs{McpArgs: &agentv1.McpArgs{
			Name: "client-" + tool, ToolName: tool, ProviderIdentifier: "client", ToolCallId: callID,
			Args: map[string]*structpb.Value{"city": args},
		}},
	}}})
}

func (r *fakeRun) recv() *agentv1.AgentClientMessage {
	select {
	case m, ok := <-r.in:
		if !ok {
			return nil
		}
		return m
	case <-time.After(waitTimeout):
		r.t.Error("timed out waiting for a client message")
		return nil
	}
}

// waitMcpResult returns the text of the next tool result the client sends.
func (r *fakeRun) waitMcpResult() (callExecID, text string) {
	for {
		m := r.recv()
		if m == nil {
			return "", ""
		}
		if ex := m.GetExecClientMessage(); ex != nil && ex.GetMcpResult() != nil {
			var b strings.Builder
			for _, c := range ex.GetMcpResult().GetSuccess().GetContent() {
				b.WriteString(c.GetText().GetText())
			}
			return ex.GetExecId(), b.String()
		}
	}
}

func (r *fakeRun) endError(js string) { r.frame(0x02, []byte(js)) }

// connectError is the end-of-stream JSON of a failed run.
func connectError(t *testing.T, code, message string, det *aiserverv1.ErrorDetails) string {
	t.Helper()
	body := map[string]any{"code": code, "message": message}
	if det != nil {
		raw, err := proto.Marshal(det)
		if err != nil {
			t.Fatal(err)
		}
		body["details"] = []map[string]any{{"type": "aiserver.v1.ErrorDetails", "value": base64.RawStdEncoding.EncodeToString(raw)}}
	}
	js, _ := json.Marshal(map[string]any{"error": body})
	return string(js)
}

func errDetails(code aiserverv1.ErrorDetails_Error, title, action string) *aiserverv1.ErrorDetails {
	c := &aiserverv1.CustomErrorDetails{Title: title}
	if action != "" {
		c.AnalyticsMetadata = &aiserverv1.ErrorAnalyticsMetadata{ActionRequired: proto.String(action)}
	}
	return &aiserverv1.ErrorDetails{Error: code, Details: c}
}

// fakeHost records what the plugin sends to CPA.
type fakeHost struct {
	mu       sync.Mutex
	logs     []string
	emits    map[string][][]byte
	closes   map[string]map[string]any
	closedCh map[string]chan struct{}
	// emitErr makes host.stream.emit fail, as it does once the client left.
	emitErr atomic.Pointer[error]
	// emitPanic makes host.stream.emit panic, to exercise recovery on the stream goroutine.
	emitPanic atomic.Bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{emits: map[string][][]byte{}, closes: map[string]map[string]any{}, closedCh: map[string]chan struct{}{}}
}

func (h *fakeHost) closedChan(id string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.closedCh[id]
	if !ok {
		ch = make(chan struct{})
		h.closedCh[id] = ch
	}
	return ch
}

func (h *fakeHost) call(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	switch method {
	case pluginabi.MethodHostLog:
		var l struct{ Level, Message string }
		_ = json.Unmarshal(raw, &l)
		h.mu.Lock()
		h.logs = append(h.logs, l.Level+" "+l.Message)
		h.mu.Unlock()
	case pluginabi.MethodHostStreamEmit:
		if h.emitPanic.Load() {
			panic("injected emit panic")
		}
		if e := h.emitErr.Load(); e != nil {
			return nil, *e
		}
		var e struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
		}
		_ = json.Unmarshal(raw, &e)
		h.mu.Lock()
		h.emits[e.StreamID] = append(h.emits[e.StreamID], e.Payload)
		h.mu.Unlock()
	case pluginabi.MethodHostStreamClose:
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		id, _ := c["stream_id"].(string)
		h.mu.Lock()
		h.closes[id] = c
		h.mu.Unlock()
		close(h.closedChan(id))
	}
	return json.RawMessage(`{}`), nil
}

// waitClosed waits for host.stream.close and returns its request.
func (h *fakeHost) waitClosed(t *testing.T, id string) map[string]any {
	t.Helper()
	select {
	case <-h.closedChan(id):
	case <-time.After(waitTimeout):
		t.Fatalf("stream %s was not closed", id)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closes[id]
}

// stream returns what was emitted on a stream, split into its SSE data lines.
func (h *fakeHost) stream(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, p := range h.emits[id] {
		for _, line := range strings.Split(string(p), "\n") {
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				out = append(out, rest)
			}
		}
	}
	return out
}

func (h *fakeHost) logged(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// harness wires a plugin to the fakes.
type harness struct {
	t    *testing.T
	p    *Plugin
	host *fakeHost
	up   *fakeCursor
	now  atomic.Int64
}

func newHarness(t *testing.T, extraConfig ...string) *harness {
	t.Helper()
	h := &harness{t: t, host: newFakeHost(), up: newFakeCursor(t)}
	h.now.Store(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC).UnixNano())
	h.p = New(h.host.call)
	h.p.now = func() time.Time { return time.Unix(0, h.now.Load()).UTC() }
	h.p.newClient = func(c cursor.Config) (*cursor.Client, error) {
		return cursor.NewClient(c, cursor.WithTLSConfig(&tls.Config{RootCAs: h.up.pool}))
	}
	t.Cleanup(h.p.Shutdown)
	h.register(extraConfig...)
	return h
}

func (h *harness) register(extraConfig ...string) {
	h.t.Helper()
	req, _ := json.Marshal(map[string]any{"config_yaml": h.up.configYAML(extraConfig...), "schema_version": 10})
	if _, e := h.call(pluginabi.MethodPluginRegister, req); e != nil {
		h.t.Fatalf("register: %v", e)
	}
}

func (h *harness) clock() time.Time { return time.Unix(0, h.now.Load()).UTC() }

func (h *harness) advance(d time.Duration) { h.now.Add(int64(d)) }

// call sends one ABI message and decodes the envelope.
func (h *harness) call(method string, req []byte) (json.RawMessage, *pluginabi.Error) {
	h.t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(h.p.Call(method, req), &env); err != nil {
		h.t.Fatalf("%s: unreadable envelope: %v", method, err)
	}
	if !env.OK {
		if env.Error == nil {
			h.t.Fatalf("%s: failed without an error", method)
		}
		return nil, env.Error
	}
	return env.Result, nil
}

func (h *harness) mustCall(method string, req any, out any) {
	h.t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res, e := h.call(method, raw)
	if e != nil {
		h.t.Fatalf("%s: %d %s: %s", method, e.HTTPStatus, e.Code, e.Message)
	}
	if out != nil {
		if err := json.Unmarshal(res, out); err != nil {
			h.t.Fatalf("%s: decode result: %v", method, err)
		}
	}
}

func (h *harness) token(validFor time.Duration, sub string) string {
	return makeJWT(h.clock().Add(validFor), sub)
}

func eventuallyTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

var _ = fmt.Sprintf

package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mediavee/cpa-cursor/internal/cursor"
)

const (
	exchangePath = "/auth/exchange_user_api_key"
	healthPath   = "/healthz"
	agentPrefix  = "/agent.v1."
	publicPrefix = "/v1/"

	// maxReplayBody is the largest unary request body kept in memory so it can be sent again after a renewal.
	maxReplayBody = 16 << 20
)

// Relay is the HTTP handler of the relay. It is safe for concurrent use.
type Relay struct {
	cfg      Config
	log      *slog.Logger
	tokens   *tokenSource
	sealer   *sealer
	verifier *verifier
	hosts    hostBook

	api, agent, public *httputil.ReverseProxy
	transport          http.RoundTripper
}

// hostBook remembers the agent hosts Cursor announced.
type hostBook struct{ v atomic.Pointer[agentHosts] }

func (b *hostBook) remember(h agentHosts) {
	if h != (agentHosts{}) {
		b.v.Store(&h)
	}
}

// pick returns the run host for a request: agentUrl in ghost mode, agentnUrl otherwise, else fallback.
func (b *hostBook) pick(ghostMode bool, fallback string) string {
	h := b.v.Load()
	if h == nil {
		return fallback
	}
	want := h.agentn
	if ghostMode {
		want = h.agent
	}
	// An announced host must keep the account token on TLS.
	if u, err := url.Parse(want); err != nil || u.Scheme != "https" || u.Host == "" {
		return fallback
	}
	return want
}

// New builds a relay. It does not contact Cursor: the first token is exchanged on demand, see Warm.
func New(cfg Config, log *slog.Logger) (*Relay, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("relay: invalid config: %w", err)
	}
	seal, err := newSealer(cfg.SealKey)
	if err != nil {
		return nil, err
	}
	ccfg := cursor.DefaultConfig()
	ccfg.APIURL = cfg.APIURL
	var copts []cursor.Option
	if cfg.UpstreamTLS != nil {
		copts = append(copts, cursor.WithTLSConfig(cfg.UpstreamTLS.Clone()))
	}
	client, err := cursor.NewClient(ccfg, copts...)
	if err != nil {
		return nil, err
	}
	r := &Relay{
		cfg:      cfg,
		log:      log,
		sealer:   seal,
		tokens:   &tokenSource{client: client, apiKey: cfg.UserAPIKey, now: cfg.Now},
		verifier: newVerifier(cfg.VerifierURL, cfg.VerifyTTL, cfg.VerifyNegativeTTL, cfg.Now),
		transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			TLSClientConfig:     cfg.UpstreamTLS.Clone(), // nil stays nil; a transport edits the config it is given
			DisableCompression:  true,                    // bodies and Accept-Encoding pass exactly as the client sent them
			TLSHandshakeTimeout: 15 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 16,
			// Same liveness as the official CLI: a ping after 10 s of silence, a dead peer after 20 s more.
			HTTP2: &http.HTTP2Config{SendPingTimeout: 10 * time.Second, PingTimeout: 20 * time.Second},
		},
	}
	staticURL := func(raw string) func(*http.Request) *url.URL {
		u, _ := url.Parse(raw) // validated
		return func(*http.Request) *url.URL { return u }
	}
	r.api = r.newProxy(staticURL(cfg.APIURL), r.tokens.get, r.tokens)
	r.api.ModifyResponse = func(resp *http.Response) error {
		if resp.Request.URL.Path == serverConfigPath {
			return r.rewriteServerConfigResponse(resp)
		}
		return nil
	}
	r.agent = r.newProxy(func(in *http.Request) *url.URL {
		u, _ := url.Parse(r.hosts.pick(in.Header.Get("X-Ghost-Mode") == "true", cfg.AgentURL))
		return u
	}, r.tokens.get, r.tokens)
	r.public = r.newProxy(staticURL(cfg.PublicAPIURL), func(context.Context) (string, error) { return cfg.UserAPIKey, nil }, nil)
	return r, nil
}

// Warm fetches the first upstream token so /healthz is green before the first client; a failure is not fatal,
// the next request tries again.
func (r *Relay) Warm(ctx context.Context) error {
	_, err := r.tokens.get(ctx)
	return err
}

func (r *Relay) newProxy(target func(*http.Request) *url.URL, credential func(context.Context) (string, error), renewer *tokenSource) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// Rewrite (not Director) drops the X-Forwarded-* headers a reverse proxy in front added.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target(pr.In))
			pr.Out.Header.Del("Authorization")
			if pr.In.URL.Path == serverConfigPath {
				// The relay rewrites this body, and only decodes gzip.
				pr.Out.Header.Set("Accept-Encoding", "gzip")
			}
		},
		Transport:     &credentialTransport{base: r.transport, credential: credential, renewer: renewer},
		FlushInterval: -1,
		ErrorHandler:  r.proxyError,
		ErrorLog:      slog.NewLogLogger(r.log.Handler(), slog.LevelDebug),
	}
}

func (r *Relay) proxyError(w http.ResponseWriter, req *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		w.WriteHeader(499)
		return
	}
	r.log.Warn("upstream error", "path", req.URL.Path, "error", err.Error())
	writeError(w, http.StatusBadGateway, "unavailable", "the upstream is unavailable")
}

// ServeHTTP implements http.Handler.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	var client string
	defer func() {
		level := slog.LevelInfo
		if req.URL.Path == healthPath {
			level = slog.LevelDebug
		}
		r.log.Log(req.Context(), level, "request", "method", req.Method, "path", req.URL.Path,
			"status", rec.status, "duration", time.Since(start).Round(time.Millisecond), "client", client)
	}()

	switch req.URL.Path {
	case healthPath:
		r.health(rec, req)
	case exchangePath:
		client = r.exchange(rec, req)
	default:
		client = r.forward(rec, req)
	}
}

func (r *Relay) health(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "unimplemented", "use GET")
		return
	}
	held := r.tokens.held()
	w.Header().Set("Content-Type", "application/json")
	if !held {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]bool{"upstreamToken": held})
}

// authenticate checks the client key against the verifier and answers the failure itself. It returns the
// key's tag for the logs and whether the request may go on.
func (r *Relay) authenticate(w http.ResponseWriter, req *http.Request, key string) (string, bool) {
	if key == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid credential")
		return "", false
	}
	tag := keyTag(key)
	ok, err := r.verifier.valid(req.Context(), key)
	switch {
	case err != nil:
		r.log.Warn("verifier failed", "client", tag, "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the key cannot be verified now")
	case !ok:
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid credential")
	default:
		return tag, true
	}
	return tag, false
}

// exchange answers POST /auth/exchange_user_api_key: the client key becomes a relay token.
func (r *Relay) exchange(w http.ResponseWriter, req *http.Request) string {
	if req.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "unimplemented", "use POST")
		return ""
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(req.Body, 1<<20))
	key := bearer(req)
	tag, ok := r.authenticate(w, req, key)
	if !ok {
		return tag
	}
	upstream, err := r.tokens.get(req.Context())
	if err != nil {
		r.log.Warn("no upstream token", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the relay holds no upstream token")
		return tag
	}
	token, err := r.sealer.mint(upstream, key)
	if err != nil {
		r.log.Warn("mint failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "cannot mint a token")
		return tag
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cursor.Tokens{AccessToken: token, RefreshToken: token})
	return tag
}

// forward authenticates a call and sends it to the upstream its path belongs to.
func (r *Relay) forward(w http.ResponseWriter, req *http.Request) string {
	path := req.URL.Path
	proxy, key := r.api, ""
	switch {
	case strings.HasPrefix(path, publicPrefix):
		// The SDK sends its raw key to the public API.
		proxy, key = r.public, bearer(req)
	case strings.HasPrefix(path, agentPrefix):
		proxy, key = r.agent, r.unseal(req)
	default:
		key = r.unseal(req)
	}
	tag, ok := r.authenticate(w, req, key)
	if !ok {
		return tag
	}
	if err := bufferForReplay(req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_argument", "cannot read the request body")
		return tag
	}
	proxy.ServeHTTP(w, req)
	return tag
}

// unseal returns the client key sealed in the request's relay token, "" when there is none.
func (r *Relay) unseal(req *http.Request) string {
	tok := bearer(req)
	if tok == "" {
		return ""
	}
	key, err := r.sealer.open(tok, r.cfg.Now())
	if err != nil {
		return ""
	}
	return key
}

func bearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if len(h) > len("Bearer ") && strings.EqualFold(h[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// bufferForReplay keeps a unary request's body in memory and gives it a GetBody, so credentialTransport can
// send it again after renewing the token. Streams, and bodies over maxReplayBody, are left alone.
func bufferForReplay(req *http.Request) error {
	if req.Body == nil || req.Body == http.NoBody || len(req.Trailer) > 0 || isStream(req) {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(req.Body, maxReplayBody+1))
	if err != nil {
		return err
	}
	if len(buf) > maxReplayBody {
		req.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), req.Body), req.Body}
		return nil
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(buf))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
	req.ContentLength = int64(len(buf))
	return nil
}

// isStream tells Connect streaming and gRPC calls, whose body must flow as it comes, from unary ones.
func isStream(req *http.Request) bool {
	ct := strings.ToLower(req.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "application/connect+") || strings.HasPrefix(ct, "application/grpc")
}

// credentialTransport sets the account's credential on every request it sends. On a 401 it renews the
// token once and, when the request body can be replayed, sends the request again.
type credentialTransport struct {
	base       http.RoundTripper
	credential func(context.Context) (string, error)
	renewer    *tokenSource
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cred, err := t.credential(req.Context())
	if err != nil {
		closeBody(req)
		return nil, err
	}
	resp, err := t.base.RoundTrip(withBearer(req, cred))
	if err != nil || resp.StatusCode != http.StatusUnauthorized || t.renewer == nil {
		return resp, err
	}
	body, fresh, ok := t.prepareRetry(req, cred)
	if !ok {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	retry := req.Clone(req.Context())
	retry.Body = body
	return t.base.RoundTrip(withBearer(retry, fresh))
}

// prepareRetry renews the token Cursor just refused, so the next call uses a fresh one even when this one
// cannot be sent again. ok is true when the request can: its body is replayable and a fresh token is held.
func (t *credentialTransport) prepareRetry(req *http.Request, stale string) (body io.ReadCloser, fresh string, ok bool) {
	if t.renewer.renew(req.Context(), stale) != nil || req.GetBody == nil {
		return nil, "", false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, "", false
	}
	fresh, err = t.credential(req.Context())
	if err != nil {
		_ = body.Close()
		return nil, "", false
	}
	return body, fresh, true
}

func withBearer(req *http.Request, token string) *http.Request {
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+token)
	return out
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// statusRecorder keeps the status for the log and stays transparent to http.ResponseController.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// writeError answers in Connect's error format, which every Cursor client understands.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

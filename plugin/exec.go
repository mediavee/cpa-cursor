package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/mediavee/cpa-cursor/internal/cursor"
	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

const (
	// defaultProbeEvery is how often an idle stream checks that CPA still reads it: CPA does not tell a
	// plugin that the client left, but an emit on a closed stream fails.
	defaultProbeEvery = 5 * time.Second
	// catalogRetryAge is how old a model list must be before an unknown model asks Cursor again.
	catalogRetryAge = time.Minute
	// discoveryTimeout bounds the lookup of the agent host that precedes a new run.
	discoveryTimeout = 5 * time.Second
)

type execRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// job is one request ready to run: the credential, the model catalog and the translated run.
type job struct {
	p      *Plugin
	rt     *runtime
	authID string
	token  string
	chat   translate.ChatRequest
	run    *translate.Run
}

// prepare checks the credential and translates the request. The returned error is already an ABI
// error.
func (p *Plugin) prepare(ctx context.Context, rt *runtime, req pluginapi.ExecutorRequest) (*job, *pluginabi.Error) {
	c, err := parseCredential(req.StorageJSON, req.AuthMetadata)
	if err != nil {
		return nil, newError("bad_auth_file", 400, "%v", err)
	}
	if !c.usable(p.now()) {
		if c.apiKey() != "" {
			return nil, newError("login_required", 401, "the Cursor session token has expired and is being renewed from the API key: retry shortly")
		}
		return nil, newError("login_required", 401, "the Cursor session token is missing or has expired and the credential has no API key: reconnection required (log in again)")
	}
	j := &job{p: p, rt: rt, authID: req.AuthID, token: c.accessToken()}
	if err := json.Unmarshal(req.Payload, &j.chat); err != nil {
		return nil, newError("bad_request", 400, "the request is not an OpenAI chat completion: %v", err)
	}
	j.chat.Model = req.Model

	cat, err := p.catalogFor(ctx, rt, req.AuthID, j.token, 0)
	if err != nil {
		return nil, p.upstreamError(ctx, rt, j.token, err)
	}
	j.run, err = translate.Translate(&j.chat, translate.Options{Catalog: cat})
	var unknown *translate.UnknownModelError
	if errors.As(err, &unknown) {
		// The account may have gained the model since the list was fetched.
		if cat, ferr := p.catalogFor(ctx, rt, req.AuthID, j.token, catalogRetryAge); ferr == nil {
			j.run, err = translate.Translate(&j.chat, translate.Options{Catalog: cat})
		}
	}
	if err != nil {
		if errors.As(err, &unknown) {
			return nil, p.upstreamError(ctx, rt, j.token, err)
		}
		return nil, newError("bad_request", 400, "cannot send this request to Cursor: %v", err)
	}
	return j, nil
}

// open starts the Cursor run of the job: the parked run that waits for these tool results when there
// is one, a new run replaying the conversation otherwise.
func (j *job) open(ctx context.Context) (*cursor.Session, error) {
	if len(j.run.Resolves) > 0 {
		ids := make([]string, len(j.run.Resolves))
		for i, r := range j.run.Resolves {
			ids[i] = r.CallID
		}
		if sess, ok := j.rt.registry(j.authID).Take(ids); ok {
			sess.Bind(ctx)
			if err := sess.SubmitToolResults(pendingResults(sess, j.run.Resolves)...); err == nil {
				j.p.logf("info", "tool results delivered to the parked Cursor run (%d call(s))", len(ids))
				return sess, nil
			}
			sess.Close()
		}
		j.p.logf("info", "no parked Cursor run for %d tool result(s) on this credential: replaying the conversation", len(ids))
		// The conversation is replayed from its messages: the run parked for these calls, wherever it
		// is (another credential, or waiting for more results), is superseded.
		j.rt.dropParked(ids)
	}
	j.discoverAgentHost(ctx)
	blobs := cursor.NewBlobs()
	for _, b := range j.run.Blobs {
		blobs.Set(b.ID, b.Data)
	}
	return j.rt.client.Run(ctx, j.token, cursor.RunParams{
		Request: j.run.Request, Blobs: blobs,
		Tools: j.run.RequestContext.Tools, Rules: j.run.RequestContext.Rules, Env: j.run.RequestContext.Env,
	})
}

// discoverAgentHost asks Cursor which host serves the Run stream before a new run opens. The client
// caches the answer; when Cursor does not give one, the run goes to the configured agent_url.
func (j *job) discoverAgentHost(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	if _, err := j.rt.client.GetServerConfig(ctx, j.token); err != nil {
		j.p.logf("warn", "cannot discover Cursor's agent host, using agent_url: %v", err)
	}
}

// pendingResults converts the client's tool results to the session's, keeping the calls it waits on.
func pendingResults(sess *cursor.Session, resolves []translate.ToolResult) []cursor.ToolResult {
	pending := map[string]bool{}
	for _, id := range sess.PendingToolCalls() {
		pending[id] = true
	}
	var out []cursor.ToolResult
	for _, r := range resolves {
		if !pending[r.CallID] {
			continue
		}
		res := cursor.ToolResult{ToolCallID: r.CallID}
		if s := r.Result.GetSuccess(); s != nil {
			res.IsError = s.GetIsError()
			for _, item := range s.GetContent() {
				switch c := item.GetContent().(type) {
				case *agentv1.McpToolResultContentItem_Text:
					res.Content = append(res.Content, cursor.ToolContent{Text: c.Text.GetText()})
				case *agentv1.McpToolResultContentItem_Image:
					res.Content = append(res.Content, cursor.ToolContent{Image: c.Image.GetData(), MimeType: c.Image.GetMimeType()})
				}
			}
		}
		out = append(out, res)
	}
	return out
}

// runner drains the events of a session into an encoder.
type runner struct {
	job    *job
	sess   *cursor.Session
	enc    *translate.Encoder
	finish translate.FinishReason
	done   bool
}

func (j *job) newRunner(p *Plugin, sess *cursor.Session) *runner {
	return &runner{job: j, sess: sess, enc: translate.NewEncoder("chatcmpl-"+uuid.NewString(), j.chat.Model, p.now().Unix())}
}

// next handles the next event of the session and returns the chunks it yields. When the run is over
// (done), finish says how; a failure comes back as the error and the session is closed.
func (r *runner) next(ctx context.Context) ([]translate.ChatChunk, error) {
	ev, err := r.sess.Next(ctx)
	if err != nil {
		r.done = true
		if errors.Is(err, io.EOF) {
			r.finish = translate.FinishStop
			return nil, nil
		}
		r.sess.Close()
		if isMaxTokens(err) {
			r.finish = translate.FinishLength
			return nil, nil
		}
		return nil, err
	}
	switch ev := ev.(type) {
	case cursor.UpdateEvent:
		return r.enc.Update(ev.Update), nil
	case cursor.ToolCallsEvent:
		r.done = true
		chunks := make([]translate.ChatChunk, 0, len(ev.Calls))
		for _, call := range ev.Calls {
			chunk, err := r.enc.ToolCall(call)
			if err != nil {
				r.sess.Close()
				return nil, err
			}
			chunks = append(chunks, chunk)
		}
		// Parked before the client can see the calls, so that its answer finds the run.
		if err := r.job.rt.registry(r.job.authID).Park(r.sess); err != nil {
			r.sess.Close()
			if !errors.Is(err, cursor.ErrClosed) {
				return nil, err
			}
			// The runtime was retired while the request ran: the calls are still answered, and the
			// client's results will replay the conversation.
			r.job.p.logf("info", "runtime retired during the request: tool calls returned without parking their run")
		}
		r.finish = translate.FinishToolCalls
		return chunks, nil
	}
	return nil, nil
}

// execute answers executor.execute: the whole response in one payload.
func (p *Plugin) execute(raw []byte) (any, error) {
	var req execRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "executor.execute: %v", err)
	}
	rt, err := p.runtime()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rt.cfg.RequestTimeout)
	defer cancel()
	j, perr := p.prepare(ctx, rt, req.ExecutorRequest)
	if perr != nil {
		return nil, perr
	}
	sess, err := j.open(ctx)
	if err != nil {
		return nil, p.upstreamError(ctx, rt, j.token, err)
	}
	r := j.newRunner(p, sess)
	for !r.done {
		if _, err := r.next(ctx); err != nil {
			return nil, p.upstreamError(ctx, rt, j.token, err)
		}
	}
	r.enc.Finish(r.finish)
	body, err := json.Marshal(r.enc.Response())
	if err != nil {
		return nil, err
	}
	return pluginapi.ExecutorResponse{Payload: body, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}

// executeStream answers executor.execute_stream. It waits for the first output (or the failure) of
// the run before answering, so that an error such as a usage limit reaches CPA as the failure of the
// call, which is what makes it cool the credential down and try the next one. The rest of the
// response goes through host.stream.emit from a goroutine.
func (p *Plugin) executeStream(raw []byte) (any, error) {
	var req execRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "executor.execute_stream: %v", err)
	}
	if req.StreamID == "" {
		return nil, newError("bad_request", 400, "executor.execute_stream needs a stream_id")
	}
	rt, err := p.runtime()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rt.cfg.RequestTimeout)
	// CPA does not say when the client leaves; the stream is watched from here on so that a client
	// that goes while the request is prepared or waits for its first output stops the run too.
	watch := p.watchStream(ctx, cancel, req.StreamID)
	fail := func(err error) (any, error) {
		watch.stop()
		cancel()
		return nil, err
	}
	j, perr := p.prepare(ctx, rt, req.ExecutorRequest)
	if perr != nil {
		return fail(perr)
	}
	sess, err := j.open(ctx)
	if err != nil {
		return fail(p.upstreamError(ctx, rt, j.token, err))
	}
	r := j.newRunner(p, sess)

	var first []translate.ChatChunk
	for len(first) == 0 && !r.done {
		chunks, err := r.next(ctx)
		if err != nil {
			return fail(p.upstreamError(ctx, rt, j.token, err))
		}
		first = chunks
	}
	p.goSafe("execute_stream", func() {
		defer cancel()
		defer watch.stop()
		var once sync.Once
		closeStream := func(e *pluginabi.Error) { once.Do(func() { p.closeStream(req.StreamID, e) }) }
		defer func() {
			// goSafe recovers the panic so CPA survives; the stream must still end, or CPA's client waits on it.
			if v := recover(); v != nil {
				closeStream(pluginabi.NewError("plugin_panic", fmt.Sprintf("cursor plugin panic while streaming: %v", v), 500))
				panic(v)
			}
		}()
		p.pumpStream(ctx, req.StreamID, watch, r, first, closeStream)
	})
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

// pumpStream sends the chunks of a run to CPA until the run ends, then closes the stream.
func (p *Plugin) pumpStream(ctx context.Context, streamID string, watch *streamWatch, r *runner, chunks []translate.ChatChunk, closeStream func(*pluginabi.Error)) {
	emit := func(payload []byte) error { return watch.emit(payload) }
	emitChunk := func(c translate.ChatChunk) error {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		return emit(append(append([]byte("data: "), b...), "\n\n"...))
	}
	emitAll := func(cs []translate.ChatChunk) error {
		for _, c := range cs {
			if err := emitChunk(c); err != nil {
				return err
			}
		}
		return nil
	}
	gone := func(err error) {
		// CPA closed the stream: its client left. Returning cancels the run's context.
		p.logf("debug", "stream %s closed by the host: %v", streamID, err)
	}

	if err := emitAll(chunks); err != nil {
		gone(err)
		return
	}
	for !r.done {
		cs, err := r.next(ctx)
		if err != nil {
			e := p.upstreamError(ctx, r.job.rt, r.job.token, err)
			p.logf("warn", "stream failed after its first output: %s: %s", describe(e), e.Message)
			closeStream(e)
			return
		}
		if err := emitAll(cs); err != nil {
			gone(err)
			return
		}
	}
	if err := emitChunk(r.enc.Finish(r.finish)); err != nil {
		gone(err)
		return
	}
	if err := emit([]byte("data: [DONE]\n\n")); err != nil {
		gone(err)
		return
	}
	closeStream(nil)
}

// streamWatch emits to one CPA stream and notices when CPA stops reading it. An idle stream is
// tested with an empty emit, which fails once the stream is closed.
type streamWatch struct {
	p         *Plugin
	streamID  string
	lastEmit  atomic.Int64
	stopProbe context.CancelFunc
}

// watchStream starts watching the stream: cancel is called when the host no longer reads it.
func (p *Plugin) watchStream(ctx context.Context, cancel context.CancelFunc, streamID string) *streamWatch {
	w := &streamWatch{p: p, streamID: streamID}
	w.lastEmit.Store(time.Now().UnixNano())
	pctx, stop := context.WithCancel(ctx)
	w.stopProbe = stop
	p.goSafe("stream_probe", func() { w.probe(pctx, cancel) })
	return w
}

func (w *streamWatch) stop() { w.stopProbe() }

func (w *streamWatch) emit(payload []byte) error {
	if err := w.p.emit(w.streamID, payload); err != nil {
		return err
	}
	w.lastEmit.Store(time.Now().UnixNano())
	return nil
}

func (w *streamWatch) probe(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(w.p.probeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Now().UnixNano()-w.lastEmit.Load() < int64(w.p.probeEvery) {
				continue
			}
			if w.emit(nil) != nil {
				cancel()
				return
			}
		}
	}
}

func (p *Plugin) emit(streamID string, payload []byte) error {
	_, err := p.host(pluginabi.MethodHostStreamEmit, struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload,omitempty"`
	}{streamID, payload})
	return err
}

// closeStream ends a stream; a non-nil e fails it with its status and retry_after.
func (p *Plugin) closeStream(streamID string, e *pluginabi.Error) {
	req := map[string]any{"stream_id": streamID}
	if e != nil {
		req["error"] = e.Message
		req["error_detail"] = e
	}
	if _, err := p.host(pluginabi.MethodHostStreamClose, req); err != nil {
		p.logf("debug", "closing stream %s: %v", streamID, err)
	}
}

// countTokens answers executor.count_tokens with an estimate of the prompt: Cursor has no counting
// endpoint. See estimateTokens for what it covers.
func (p *Plugin) countTokens(raw []byte) (any, error) {
	var req execRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, newError("bad_request", 400, "executor.count_tokens: %v", err)
	}
	var chat translate.ChatRequest
	if err := json.Unmarshal(req.Payload, &chat); err != nil {
		return nil, newError("bad_request", 400, "the request is not an OpenAI chat completion: %v", err)
	}
	n := estimateTokens(&chat)
	body, err := json.Marshal(map[string]any{"usage": map[string]int64{"prompt_tokens": n, "total_tokens": n}})
	if err != nil {
		return nil, err
	}
	return pluginapi.ExecutorResponse{Payload: body, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}

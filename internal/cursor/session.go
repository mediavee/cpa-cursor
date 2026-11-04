package cursor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// Protocol timings of the official CLI (D3).
const (
	heartbeatInterval = 5 * time.Second
	stallTimeout      = 30 * time.Second
	// toolCallQuiet is how long the server may stay silent after a client tool
	// call before we hand the calls so far to the consumer. Cursor can run the
	// calls of a step one at a time and only list the step afterwards.
	toolCallQuiet = time.Second
)

var (
	// ErrStalled ends a session that received nothing for 30 seconds.
	ErrStalled = errors.New("cursor: no traffic from the server for 30s")
	// ErrClosed is the terminal error of a session closed by its owner.
	ErrClosed = errors.New("cursor: session closed")
	// ErrUnknownToolCall is returned for a tool_call_id the session is not waiting on.
	ErrUnknownToolCall = errors.New("cursor: no pending tool call with this id")
	// ErrTruncated is returned when the stream ends without an end-of-stream frame.
	ErrTruncated = errors.New("cursor: stream ended without an end-of-stream frame")

	errEnded = errors.New("cursor: session ended")
)

// PanicError is the terminal error of a session whose goroutine panicked.
type PanicError struct {
	Where string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("cursor: panic in %s: %v", e.Where, e.Value) }

// RunParams describes one agent run.
type RunParams struct {
	// Request is the first message of the stream. The caller builds it,
	// including the model, the conversation state and the client tools.
	Request *agentv1.AgentRunRequest
	// Blobs backs the server's KV requests. Optional; an empty store is used otherwise.
	Blobs *Blobs
	// Tools, Rules and Env answer request_context_args and mcp_state. Tools
	// must mirror Request.McpTools: they are also the allow-list for mcp_args.
	Tools []*agentv1.McpToolDefinition
	Rules []*agentv1.CursorRule
	Env   *agentv1.RequestContextEnv

	// RequestID defaults to a fresh UUID. OriginalRequestID defaults to
	// RequestID; a retry of a failed attempt passes the first attempt's id.
	RequestID         string
	OriginalRequestID string
	// BlobEncryptionKey is 64 hex characters, one per conversation. Random when empty.
	BlobEncryptionKey string
}

type execRef struct {
	id     uint32
	execID string
	// handed is set when Next returned the call to the consumer. A call that
	// arrives after that belongs to the next step.
	handed bool
}

// Session is one open Run stream. Consume it with Next from a single
// goroutine; every other method is safe for concurrent use.
//
// The session heartbeats, answers KV and exec requests and watches for
// stalls on its own goroutines, whether or not Next is being called, which is
// what lets a session wait parked for tool results.
type Session struct {
	c      *Client
	params RunParams
	blobs  *Blobs
	reqID  string

	ctx    context.Context
	cancel context.CancelCauseFunc
	pw     *io.PipeWriter
	out    chan []byte
	q      *eventQueue
	stall  Timer
	wg     sync.WaitGroup
	done   chan struct{}
	once   sync.Once

	mu        sync.Mutex
	unbind    func() bool
	header    http.Header
	pending   map[string]execRef
	batch     []*agentv1.McpArgs
	quiet     Timer
	listed    int
	received  int
	served    int
	turnEnded bool
}

// Run opens a session. It does not block on the network: connection and HTTP
// errors surface from Next. ctx bounds the session until Unbind is called
// (the parked-run registry does) or Bind replaces it.
func (c *Client) Run(ctx context.Context, token string, p RunParams) (*Session, error) {
	if token == "" {
		return nil, errors.New("cursor: Run needs a token")
	}
	if p.Request == nil {
		return nil, errors.New("cursor: Run needs a run request")
	}
	if err := checkForbiddenFields(p); err != nil {
		return nil, err
	}
	if p.RequestID == "" {
		p.RequestID = uuid.NewString()
	}
	if p.OriginalRequestID == "" {
		p.OriginalRequestID = p.RequestID
	}
	if p.BlobEncryptionKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("cursor: blob encryption key: %w", err)
		}
		p.BlobEncryptionKey = hex.EncodeToString(key)
	}
	first, err := marshalFrame(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: p.Request}})
	if err != nil {
		return nil, err
	}
	blobs := p.Blobs
	if blobs == nil {
		blobs = NewBlobs()
	}

	pr, pw := io.Pipe()
	sctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	s := &Session{
		c: c, params: p, blobs: blobs, reqID: p.RequestID,
		ctx: sctx, cancel: cancel, pw: pw,
		out:     make(chan []byte, 64),
		q:       newEventQueue(),
		done:    make(chan struct{}),
		pending: map[string]execRef{},
	}
	s.out <- first
	s.stall = c.clock.NewTimer(stallTimeout)
	s.Bind(ctx)

	s.goSafe("writer", s.writeLoop)
	s.goSafe("heartbeat", s.heartbeatLoop)
	s.goSafe("watchdog", s.watchdogLoop)
	s.goSafe("reader", func() { s.readLoop(token, pr) })
	go func() {
		s.wg.Wait()
		close(s.done)
	}()
	return s, nil
}

// checkForbiddenFields refuses the prompt fields the server rejects or
// ignores on ordinary accounts and that mark a client as internal. The system
// prompt travels as a rule in the request context instead.
func checkForbiddenFields(p RunParams) error {
	r := p.Request
	switch {
	case r.CustomSystemPrompt != nil:
		return errors.New("cursor: custom_system_prompt must not be sent")
	case r.ExcludeWorkspaceContext != nil:
		return errors.New("cursor: exclude_workspace_context must not be sent")
	case r.SystemPromptSpec != nil:
		return errors.New("cursor: system_prompt_spec must not be sent")
	}
	return nil
}

// RequestID is the x-request-id of this attempt.
func (s *Session) RequestID() string { return s.reqID }

// Header returns the response headers once the server answered, nil before.
func (s *Session) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header
}

// Done is closed when the session is over and all its goroutines have exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Next returns the next event. After the last event it returns io.EOF for a
// clean end of run, or the terminal error: *Error for a Cursor error,
// ErrStalled, ErrClosed, a context error or a transport error. Cancelling
// ctx only abandons this call; it does not close the session.
func (s *Session) Next(ctx context.Context) (Event, error) {
	ev, err := s.q.pop(ctx)
	if tc, ok := ev.(ToolCallsEvent); ok {
		s.mu.Lock()
		for _, a := range tc.Calls {
			if ref, ok := s.pending[a.GetToolCallId()]; ok {
				ref.handed = true
				s.pending[a.GetToolCallId()] = ref
			}
		}
		s.mu.Unlock()
	}
	return ev, err
}

// Close ends the session and its HTTP stream. It is idempotent.
func (s *Session) Close() { s.finish(ErrClosed) }

// Bind ties the session to ctx: when ctx ends, the session is closed with
// its cause. It replaces any previous binding.
func (s *Session) Bind(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { s.finish(context.Cause(ctx)) })
	s.mu.Lock()
	old := s.unbind
	s.unbind = stop
	s.mu.Unlock()
	if old != nil {
		old()
	}
}

// Unbind detaches the session from the context it was bound to, so that it
// outlives the request that created it. Close or the registry's TTL then
// remain the only ways to end it.
func (s *Session) Unbind() {
	s.mu.Lock()
	old := s.unbind
	s.unbind = nil
	s.mu.Unlock()
	if old != nil {
		old()
	}
}

// PendingToolCalls lists the tool_call_ids that Next handed out and that are
// not answered yet. A call that arrived after the last ToolCallsEvent was
// popped is not listed: it comes with the next one.
func (s *Session) PendingToolCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.pending))
	for id, ref := range s.pending {
		if ref.handed {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// ToolContent is one piece of a tool result: text, or an image when Image is set.
type ToolContent struct {
	Text     string
	Image    []byte
	MimeType string
}

// ToolResult answers one client tool call.
type ToolResult struct {
	ToolCallID string
	Content    []ToolContent
	IsError    bool
}

// SubmitToolResults answers pending tool calls on the open stream; the model
// then resumes and Next yields the rest of the turn. Results may cover only
// some of the pending calls. It sends nothing if any id is unknown.
func (s *Session) SubmitToolResults(results ...ToolResult) error {
	s.mu.Lock()
	refs := make([]execRef, len(results))
	for i, r := range results {
		ref, ok := s.pending[r.ToolCallID]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrUnknownToolCall, r.ToolCallID)
		}
		refs[i] = ref
	}
	for _, r := range results {
		delete(s.pending, r.ToolCallID)
	}
	s.mu.Unlock()
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	for i, r := range results {
		s.send(clientExec(refs[i].id, refs[i].execID, func(m *agentv1.ExecClientMessage) {
			m.Message = &agentv1.ExecClientMessage_McpResult{McpResult: mcpResult(r)}
		}))
		s.send(streamClose(refs[i].id))
	}
	return nil
}

func mcpResult(r ToolResult) *agentv1.McpResult {
	content := make([]*agentv1.McpToolResultContentItem, 0, len(r.Content))
	for _, c := range r.Content {
		if len(c.Image) > 0 {
			content = append(content, &agentv1.McpToolResultContentItem{Content: &agentv1.McpToolResultContentItem_Image{
				Image: &agentv1.McpImageContent{Data: c.Image, MimeType: c.MimeType},
			}})
			continue
		}
		content = append(content, &agentv1.McpToolResultContentItem{Content: &agentv1.McpToolResultContentItem_Text{
			Text: &agentv1.McpTextContent{Text: c.Text},
		}})
	}
	return &agentv1.McpResult{Result: &agentv1.McpResult_Success{Success: &agentv1.McpSuccess{Content: content, IsError: r.IsError}}}
}

// send queues one client message. After the session ended it drops it.
func (s *Session) send(m *agentv1.AgentClientMessage) {
	frame, err := marshalFrame(m)
	if err != nil {
		s.finish(err)
		return
	}
	select {
	case s.out <- frame:
	case <-s.ctx.Done():
	}
}

// finish ends the session once. A nil err is a clean end.
func (s *Session) finish(err error) {
	s.once.Do(func() {
		if err == nil {
			s.q.finish(io.EOF)
			s.cancel(errEnded)
			s.pw.CloseWithError(errEnded)
		} else {
			s.q.finish(err)
			s.cancel(err)
			s.pw.CloseWithError(err)
		}
		s.stall.Stop()
		s.mu.Lock()
		if s.quiet != nil {
			s.quiet.Stop()
		}
		unbind := s.unbind
		s.unbind = nil
		s.mu.Unlock()
		if unbind != nil {
			unbind()
		}
	})
}

// goSafe runs fn on a tracked goroutine. A panic becomes the session's
// terminal error instead of killing the process.
func (s *Session) goSafe(where string, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.guard(where, fn)
	}()
}

func (s *Session) guard(where string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.finish(&PanicError{Where: where, Value: r, Stack: debug.Stack()})
		}
	}()
	fn()
}

func (s *Session) writeLoop() {
	for {
		select {
		case frame := <-s.out:
			if _, err := s.pw.Write(frame); err != nil {
				// The transport closed the request body: the read side reports why.
				if !errors.Is(err, io.ErrClosedPipe) {
					s.finish(fmt.Errorf("cursor: write: %w", err))
				}
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Session) heartbeatLoop() {
	t := s.c.clock.NewTimer(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C():
			t.Reset(heartbeatInterval)
			s.send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &agentv1.ClientHeartbeat{}}})
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Session) watchdogLoop() {
	select {
	case <-s.stall.C():
		s.finish(ErrStalled)
	case <-s.ctx.Done():
	}
}

func (s *Session) readLoop(token string, body *io.PipeReader) {
	req, err := s.request(token, body)
	if err != nil {
		s.finish(err)
		return
	}
	resp, err := s.c.h2.RoundTrip(req)
	if err != nil {
		s.finish(s.transportErr(err))
		return
	}
	defer resp.Body.Close()
	s.mu.Lock()
	s.header = resp.Header
	s.mu.Unlock()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		s.finish(parseHTTPError(resp.StatusCode, msg))
		return
	}
	s.touch()

	fr := frameReader{r: resp.Body}
	for {
		flags, payload, err := fr.read()
		if err != nil {
			s.mu.Lock()
			ended := s.turnEnded
			s.mu.Unlock()
			switch {
			case errors.Is(err, io.EOF) && ended:
				s.finish(nil)
			case errors.Is(err, io.EOF):
				s.finish(ErrTruncated)
			default:
				s.finish(s.transportErr(err))
			}
			return
		}
		s.touch()
		if flags&flagEndStream != 0 {
			s.finish(parseEndStream(payload))
			return
		}
		var m agentv1.AgentServerMessage
		if err := proto.Unmarshal(payload, &m); err != nil {
			s.finish(fmt.Errorf("cursor: decode server message: %w", err))
			return
		}
		s.handle(&m)
	}
}

// transportErr prefers the cancellation cause over the transport's wording of it.
func (s *Session) transportErr(err error) error {
	if cause := context.Cause(s.ctx); cause != nil && !errors.Is(cause, errEnded) {
		return cause
	}
	return fmt.Errorf("cursor: transport: %w", err)
}

func (s *Session) touch() { s.stall.Reset(stallTimeout) }

func (s *Session) handle(m *agentv1.AgentServerMessage) {
	if t := m.GetTtftBreakdown(); t != nil {
		s.q.push(TtftEvent{Breakdown: t})
	}
	switch v := m.GetMessage().(type) {
	case *agentv1.AgentServerMessage_InteractionUpdate:
		s.onUpdate(v.InteractionUpdate)
	case *agentv1.AgentServerMessage_ExecServerMessage:
		s.handleExec(v.ExecServerMessage)
	case *agentv1.AgentServerMessage_ExecServerControlMessage:
		if abort := v.ExecServerControlMessage.GetAbort(); abort != nil {
			s.onAbort(abort.GetId())
		}
	case *agentv1.AgentServerMessage_ConversationCheckpointUpdate:
		s.q.push(CheckpointEvent{State: v.ConversationCheckpointUpdate})
	case *agentv1.AgentServerMessage_KvServerMessage:
		s.handleKV(v.KvServerMessage)
	case *agentv1.AgentServerMessage_InteractionQuery:
		s.send(refuseQuery(v.InteractionQuery))
	}
}

func (s *Session) handleKV(kv *agentv1.KvServerMessage) {
	out := &agentv1.KvClientMessage{Id: kv.GetId()}
	switch a := kv.GetMessage().(type) {
	case *agentv1.KvServerMessage_GetBlobArgs:
		res := &agentv1.GetBlobResult{}
		if data, ok := s.blobs.Get(a.GetBlobArgs.GetBlobId()); ok {
			res.BlobData = data
		}
		out.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: res}
	case *agentv1.KvServerMessage_SetBlobArgs:
		s.blobs.Set(a.SetBlobArgs.GetBlobId(), a.SetBlobArgs.GetBlobData())
		out.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
	default:
		return
	}
	s.send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: out}})
}

func (s *Session) onUpdate(u *agentv1.InteractionUpdate) {
	if _, ok := u.GetMessage().(*agentv1.InteractionUpdate_Heartbeat); ok {
		return
	}
	s.mu.Lock()
	switch v := u.GetMessage().(type) {
	case *agentv1.InteractionUpdate_ToolRequestsListed:
		s.listed = int(v.ToolRequestsListed.GetCallCount())
	case *agentv1.InteractionUpdate_ToolCallStarted:
		if v.ToolCallStarted.GetToolCall().GetMcpToolCall() == nil {
			s.served++
		}
	case *agentv1.InteractionUpdate_TurnEnded:
		s.turnEnded = true
	}
	s.mu.Unlock()
	s.q.push(UpdateEvent{Update: u})
	s.mu.Lock()
	s.stepProgressLocked()
	s.mu.Unlock()
}

func (s *Session) onMcpArgs(ex *agentv1.ExecServerMessage, a *agentv1.McpArgs) {
	reply := func(r *agentv1.McpResult) {
		s.send(clientExec(ex.GetId(), ex.GetExecId(), func(m *agentv1.ExecClientMessage) {
			m.Message = &agentv1.ExecClientMessage_McpResult{McpResult: r}
		}))
		s.send(streamClose(ex.GetId()))
	}
	switch {
	case a.GetSmartModeApprovalOnly():
		// A permission probe, not a call: approving runs nothing, and the
		// real call follows as its own exec.
		reply(&agentv1.McpResult{Result: &agentv1.McpResult_Approved{Approved: &agentv1.McpApproved{}}})
		return
	case !s.knowsTool(a):
		reply(&agentv1.McpResult{Result: &agentv1.McpResult_ToolNotFound{ToolNotFound: &agentv1.McpToolNotFound{
			Name: a.GetName(), AvailableTools: s.toolNames(),
		}}})
		return
	}
	callID := a.GetToolCallId()
	if callID == "" {
		callID = ex.GetExecId()
	}
	a.ToolCallId = callID

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[callID] = execRef{id: ex.GetId(), execID: ex.GetExecId()}
	s.batch = append(s.batch, a)
	s.received++
	s.stepProgressLocked()
}

// onAbort forgets the pending call whose exec the server cancelled.
func (s *Session) onAbort(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for callID, ref := range s.pending {
		if ref.id == id {
			delete(s.pending, callID)
		}
	}
	s.batch = slices.DeleteFunc(s.batch, func(a *agentv1.McpArgs) bool {
		_, ok := s.pending[a.GetToolCallId()]
		return !ok
	})
}

// stepProgressLocked emits the collected tool calls once the step is
// complete, that is when the server listed its calls and all have arrived, or
// when it went quiet without listing them.
func (s *Session) stepProgressLocked() {
	if s.listed > 0 && s.received+s.served >= s.listed {
		if len(s.batch) > 0 {
			s.flushLocked()
		} else {
			// The server served the whole step itself (its own tools): the
			// next step starts from zero.
			s.listed, s.received, s.served = 0, 0, 0
		}
		return
	}
	if len(s.batch) == 0 {
		return
	}
	if s.quiet == nil {
		s.quiet = s.c.clock.AfterFunc(toolCallQuiet, func() {
			s.guard("tool-call-quiet", func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.flushLocked()
			})
		})
		return
	}
	s.quiet.Reset(toolCallQuiet)
}

func (s *Session) flushLocked() {
	if len(s.batch) == 0 {
		return
	}
	calls := s.batch
	s.batch = nil
	if s.quiet != nil {
		s.quiet.Stop()
	}
	s.q.push(ToolCallsEvent{Calls: calls})
	if s.listed > 0 && s.received+s.served >= s.listed {
		s.listed, s.received, s.served = 0, 0, 0
	}
}

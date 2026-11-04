package cursor

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

func startSession(t *testing.T, up *fakeUpstream, clk Clock, p RunParams) (*Session, *fakeStream) {
	t.Helper()
	c := up.client(t, clk)
	if p.Request == nil {
		p.Request = runRequest("hello")
	}
	s, err := c.Run(context.Background(), "test-token", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, up.accept()
}

func TestSimpleTextUntilTurnEnded(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Request: runRequest("say pong")})

	first := fs.recv()
	rr := first.msg.GetRunRequest()
	if got := rr.GetAction().GetUserMessageAction().GetUserMessage().GetText(); got != "say pong" {
		t.Fatalf("first message is not the caller's run request: %v", first.msg)
	}

	fs.send(textMsg("po"), false)
	fs.send(textMsg("ng"), false)
	fs.send(updateMsg(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_Heartbeat{Heartbeat: &agentv1.HeartbeatUpdate{}}}), false)
	fs.send(updateMsg(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_RoutedModel{RoutedModel: &agentv1.RoutedModelUpdate{DisplayName: "Test Auto"}}}), false)
	fs.send(&agentv1.AgentServerMessage{TtftBreakdown: &agentv1.TtftBreakdown{}}, false)
	fs.send(turnEndedMsg(11, 2), false)
	fs.end(`{}`)

	evs, err := drain(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v, want io.EOF", err)
	}
	var text strings.Builder
	var routed string
	var ended *agentv1.TurnEndedUpdate
	var ttft bool
	for _, ev := range evs {
		switch e := ev.(type) {
		case UpdateEvent:
			switch u := e.Update.GetMessage().(type) {
			case *agentv1.InteractionUpdate_TextDelta:
				text.WriteString(u.TextDelta.GetText())
			case *agentv1.InteractionUpdate_RoutedModel:
				routed = u.RoutedModel.GetDisplayName()
			case *agentv1.InteractionUpdate_TurnEnded:
				ended = u.TurnEnded
			case *agentv1.InteractionUpdate_Heartbeat:
				t.Error("heartbeat leaked to the consumer")
			}
		case TtftEvent:
			ttft = true
		}
	}
	if text.String() != "pong" || routed != "Test Auto" || !ttft {
		t.Errorf("text=%q routed=%q ttft=%v", text.String(), routed, ttft)
	}
	if ended.GetInputTokens() != 11 || ended.GetOutputTokens() != 2 {
		t.Errorf("turn_ended = %v", ended)
	}
	select {
	case <-s.Done():
	case <-time.After(waitTimeout):
		t.Fatal("session goroutines did not exit after a clean end")
	}
}

func TestKVStoreGetAndSet(t *testing.T) {
	up := newFakeUpstream(t)
	blobs := NewBlobs()
	known := blobs.Put([]byte(`{"role":"system","content":"be brief"}`))
	s, fs := startSession(t, up, SystemClock{}, RunParams{Blobs: blobs})
	fs.recv()

	kvGet := func(id uint32, blobID []byte) *agentv1.AgentServerMessage {
		return &agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_KvServerMessage{KvServerMessage: &agentv1.KvServerMessage{
			Id: id, Message: &agentv1.KvServerMessage_GetBlobArgs{GetBlobArgs: &agentv1.GetBlobArgs{BlobId: blobID}},
		}}}
	}
	fs.send(kvGet(1, known), false)
	got := fs.recvNonHeartbeat().msg.GetKvClientMessage()
	if got.GetId() != 1 || string(got.GetGetBlobResult().GetBlobData()) != `{"role":"system","content":"be brief"}` {
		t.Errorf("get of a known blob answered %v", got)
	}

	fs.send(kvGet(2, []byte("missing")), false)
	got = fs.recvNonHeartbeat().msg.GetKvClientMessage()
	if got.GetId() != 2 || got.GetGetBlobResult() == nil || got.GetGetBlobResult().BlobData != nil {
		t.Errorf("get of an unknown blob answered %v", got)
	}

	fs.send(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_KvServerMessage{KvServerMessage: &agentv1.KvServerMessage{
		Id: 3, Message: &agentv1.KvServerMessage_SetBlobArgs{SetBlobArgs: &agentv1.SetBlobArgs{BlobId: []byte("k"), BlobData: []byte("v")}},
	}}}, false)
	got = fs.recvNonHeartbeat().msg.GetKvClientMessage()
	if got.GetId() != 3 || got.GetSetBlobResult() == nil {
		t.Errorf("set answered %v", got)
	}
	if v, ok := blobs.Get([]byte("k")); !ok || string(v) != "v" {
		t.Errorf("server-written blob not stored: %q %v", v, ok)
	}

	fs.send(kvGet(4, []byte("k")), false)
	got = fs.recvNonHeartbeat().msg.GetKvClientMessage()
	if string(got.GetGetBlobResult().GetBlobData()) != "v" {
		t.Errorf("blob written by the server is not served back: %v", got)
	}
	s.Close()
}

func TestRequestContextCarriesToolsRulesAndEnv(t *testing.T) {
	up := newFakeUpstream(t)
	rule := &agentv1.CursorRule{FullPath: "/client/system-prompt/0.mdc", Content: "be brief"}
	_, fs := startSession(t, up, SystemClock{}, RunParams{
		Tools: []*agentv1.McpToolDefinition{testTool("get_weather")},
		Rules: []*agentv1.CursorRule{rule},
	})
	fs.recv()

	fs.send(execMsg(7, "e7", func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_RequestContextArgs{RequestContextArgs: &agentv1.RequestContextArgs{}}
	}), false)
	m := fs.recvNonHeartbeat().msg.GetExecClientMessage()
	if m.GetId() != 7 || m.GetExecId() != "e7" {
		t.Fatalf("answer is not tied to the exec: %v", m)
	}
	rc := m.GetRequestContextResult().GetSuccess().GetRequestContext()
	if rc == nil {
		t.Fatalf("no request context in %v", m)
	}
	if len(rc.GetTools()) != 1 || rc.GetTools()[0].GetToolName() != "get_weather" {
		t.Errorf("tools = %v", rc.GetTools())
	}
	if len(rc.GetRules()) != 1 || rc.GetRules()[0].GetContent() != "be brief" {
		t.Errorf("rules = %v", rc.GetRules())
	}
	if rc.GetEnv() == nil || len(rc.GetEnv().GetWorkspacePaths()) != 0 {
		t.Errorf("env should be present and empty, got %v", rc.GetEnv())
	}
}

func TestBuiltInAndUnknownExecsAreRefused(t *testing.T) {
	up := newFakeUpstream(t)
	_, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("get_weather")}})
	fs.recv()

	fs.send(execMsg(10, "shell", func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_ShellArgs{ShellArgs: &agentv1.ShellArgs{Command: "ls"}}
	}), false)
	expectRefusal(t, fs, 10, "tool_unavailable")

	fs.send(execMsg(11, "grep", func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_GrepArgs{GrepArgs: &agentv1.GrepArgs{}}
	}), false)
	expectRefusal(t, fs, 11, "tool_unavailable")

	// An exec variant this build's protos do not model decodes with no case.
	unknown := execMsg(12, "future", func(*agentv1.ExecServerMessage) {})
	raw := unknown.GetExecServerMessage().ProtoReflect()
	raw.SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 999, protowire.BytesType), []byte("x")))
	fs.send(unknown, false)
	expectRefusal(t, fs, 12, "unknown_exec_variant")
}

func expectRefusal(t *testing.T, fs *fakeStream, id uint32, code string) {
	t.Helper()
	th := fs.recvNonHeartbeat().msg.GetExecClientControlMessage().GetThrow()
	if th.GetId() != id || th.GetErrorCode() != code || th.GetError() == "" {
		t.Fatalf("throw for exec %d = %v, want code %q", id, th, code)
	}
	sc := fs.recvNonHeartbeat().msg.GetExecClientControlMessage().GetStreamClose()
	if sc == nil || sc.GetId() != id {
		t.Fatalf("no stream_close for exec %d after the throw, got %v", id, sc)
	}
}

func TestMcpStateListsOurTools(t *testing.T) {
	up := newFakeUpstream(t)
	_, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("get_weather")}})
	fs.recv()

	fs.send(execMsg(20, "mcp-state", func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_McpStateExecArgs{McpStateExecArgs: &agentv1.McpStateExecArgs{}}
	}), false)
	res := fs.recvNonHeartbeat().msg.GetExecClientMessage().GetMcpStateExecResult().GetSuccess()
	if len(res.GetServers()) != 1 || len(res.GetServers()[0].GetTools()) != 1 || res.GetServers()[0].GetTools()[0].GetToolName() != "get_weather" {
		t.Fatalf("mcp_state = %v", res)
	}

	fs.send(execMsg(21, "list", func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_ListMcpResourcesExecArgs{ListMcpResourcesExecArgs: &agentv1.ListMcpResourcesExecArgs{}}
	}), false)
	if fs.recvNonHeartbeat().msg.GetExecClientMessage().GetListMcpResourcesExecResult().GetSuccess() == nil {
		t.Error("list_mcp_resources should answer an empty success")
	}
}

func TestInteractionQueryIsRefused(t *testing.T) {
	up := newFakeUpstream(t)
	_, fs := startSession(t, up, SystemClock{}, RunParams{})
	fs.recv()

	fs.send(&agentv1.AgentServerMessage{Message: &agentv1.AgentServerMessage_InteractionQuery{InteractionQuery: &agentv1.InteractionQuery{
		Id: 5, Query: &agentv1.InteractionQuery_WebSearchRequestQuery{WebSearchRequestQuery: &agentv1.WebSearchRequestQuery{}},
	}}}, false)
	r := fs.recvNonHeartbeat().msg.GetInteractionResponse()
	if r.GetId() != 5 || r.GetWebSearchRequestResponse().GetRejected() == nil {
		t.Fatalf("interaction response = %v", r)
	}
}

func TestParkedToolCallResumesWithResult(t *testing.T) {
	up := newFakeUpstream(t)
	clk := newFakeClock()
	s, fs := startSession(t, up, clk, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("get_weather")}})
	reg := NewRegistry(RegistryConfig{Clock: clk})
	t.Cleanup(reg.Close)
	fs.recv()

	fs.send(textMsg("Let me check. "), false)
	fs.send(mcpArgsMsg(30, "call_1", "get_weather"), false)
	fs.send(listedMsg(1), false)

	var calls []*agentv1.McpArgs
	for calls == nil {
		if tc, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			calls = tc.Calls
		}
	}
	if len(calls) != 1 || calls[0].GetToolCallId() != "call_1" {
		t.Fatalf("tool calls = %v", calls)
	}
	if got := s.PendingToolCalls(); !slices.Equal(got, []string{"call_1"}) {
		t.Fatalf("pending = %v", got)
	}

	if err := reg.Park(s); err != nil {
		t.Fatal(err)
	}
	// Parked: the request context is gone but the stream stays open, the
	// server gets no answer yet, and heartbeats keep coming.
	fs.expectNone()
	clk.Advance(5 * time.Second)
	if fs.recv().msg.GetClientHeartbeat() == nil {
		t.Fatal("no heartbeat while parked")
	}

	resumed, ok := reg.Take([]string{"call_1"})
	if !ok || resumed != s {
		t.Fatalf("Take = %v, %v", resumed, ok)
	}
	if err := resumed.SubmitToolResults(ToolResult{
		ToolCallID: "call_1",
		Content:    []ToolContent{{Text: "sunny, 21C"}, {Image: []byte{1, 2, 3}, MimeType: "image/png"}},
	}); err != nil {
		t.Fatal(err)
	}

	res := fs.recvNonHeartbeat().msg.GetExecClientMessage()
	if res.GetId() != 30 || res.GetExecId() != "exec-call_1" {
		t.Fatalf("mcp_result is not tied to the tool call's exec: %v", res)
	}
	content := res.GetMcpResult().GetSuccess().GetContent()
	if len(content) != 2 || content[0].GetText().GetText() != "sunny, 21C" ||
		string(content[1].GetImage().GetData()) != "\x01\x02\x03" || content[1].GetImage().GetMimeType() != "image/png" {
		t.Fatalf("mcp_result content = %v", content)
	}
	if res.GetMcpResult().GetSuccess().GetIsError() {
		t.Error("result flagged as an error")
	}
	if sc := fs.recvNonHeartbeat().msg.GetExecClientControlMessage().GetStreamClose(); sc.GetId() != 30 {
		t.Fatalf("no stream_close after the mcp_result: %v", sc)
	}
	if len(s.PendingToolCalls()) != 0 {
		t.Error("tool call still pending after its result")
	}

	fs.send(textMsg("It is sunny."), false)
	fs.send(turnEndedMsg(5, 5), false)
	fs.end(`{}`)
	evs, err := drain(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v", err)
	}
	if len(evs) < 2 {
		t.Fatalf("events after resume = %v", evs)
	}
}

func TestToolResultErrorFlag(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("t")}})
	fs.recv()
	fs.send(mcpArgsMsg(1, "c1", "t"), false)
	fs.send(listedMsg(1), false)
	for {
		if _, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			break
		}
	}
	if err := s.SubmitToolResults(ToolResult{ToolCallID: "c1", Content: []ToolContent{{Text: "boom"}}, IsError: true}); err != nil {
		t.Fatal(err)
	}
	if !fs.recvNonHeartbeat().msg.GetExecClientMessage().GetMcpResult().GetSuccess().GetIsError() {
		t.Error("error result not flagged")
	}
}

func TestSubmitUnknownToolCallSendsNothing(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("t")}})
	fs.recv()
	fs.send(mcpArgsMsg(1, "c1", "t"), false)
	fs.send(listedMsg(1), false)
	for {
		if _, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			break
		}
	}
	err := s.SubmitToolResults(ToolResult{ToolCallID: "c1"}, ToolResult{ToolCallID: "nope"})
	if !errors.Is(err, ErrUnknownToolCall) {
		t.Fatalf("err = %v", err)
	}
	fs.expectNone()
	if !slices.Equal(s.PendingToolCalls(), []string{"c1"}) {
		t.Errorf("a failed submit consumed the pending call: %v", s.PendingToolCalls())
	}
}

func TestParallelToolCallsAreOneStep(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("a"), testTool("b")}})
	fs.recv()
	fs.send(mcpArgsMsg(1, "c1", "a"), false)
	fs.send(mcpArgsMsg(2, "c2", "b"), false)
	fs.send(listedMsg(2), false)
	for {
		if tc, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			if len(tc.Calls) != 2 {
				t.Fatalf("calls = %v, want both of the step in one event", tc.Calls)
			}
			return
		}
	}
}

// Seen live: the model first calls a Cursor tool (get_mcp_tools), the server
// lists that one-call step, and only then calls our tool and lists it.
func TestServerOnlyStepDoesNotLeakIntoTheNextStep(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("a"), testTool("b")}})
	fs.recv()

	fs.send(updateMsg(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_ToolCallStarted{ToolCallStarted: &agentv1.ToolCallStartedUpdate{
		CallId: "own", ToolCall: &agentv1.ToolCall{Tool: &agentv1.ToolCall_GetMcpToolsToolCall{GetMcpToolsToolCall: &agentv1.GetMcpToolsToolCall{}}},
	}}}), false)
	fs.send(listedMsg(1), false)

	// Step 2 has two calls. If step 1's counts leaked, the first call alone
	// would already satisfy "listed" and go out by itself.
	fs.send(mcpArgsMsg(1, "c1", "a"), false)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	for {
		ev, err := s.Next(ctx)
		if err != nil {
			break
		}
		if _, ok := ev.(ToolCallsEvent); ok {
			t.Fatal("first call of a two-call step was handed over alone")
		}
	}
	fs.send(mcpArgsMsg(2, "c2", "b"), false)
	fs.send(listedMsg(2), false)
	for {
		if tc, ok := nextEvent(t, s).(ToolCallsEvent); ok {
			if len(tc.Calls) != 2 {
				t.Fatalf("calls = %v", tc.Calls)
			}
			return
		}
	}
}

func TestToolCallsAreHandedOverWhenServerGoesQuiet(t *testing.T) {
	up := newFakeUpstream(t)
	clk := newFakeClock()
	s, fs := startSession(t, up, clk, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("a")}})
	fs.recv()
	fs.send(mcpArgsMsg(1, "c1", "a"), false)
	// No tool_requests_listed: the server may be waiting on this very call.
	eventually(t, "the quiet timer", func() bool { return clk.active() >= 3 })

	clk.Advance(toolCallQuiet - time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if ev, err := s.Next(ctx); err == nil {
		t.Fatalf("event %T before the quiet period elapsed", ev)
	}
	clk.Advance(time.Millisecond)
	if tc, ok := nextEvent(t, s).(ToolCallsEvent); !ok || len(tc.Calls) != 1 {
		t.Fatal("no ToolCallsEvent once the server went quiet")
	}
}

func TestUndeclaredToolIsNotFoundAndApprovalProbeIsApproved(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{testTool("a")}})
	fs.recv()

	fs.send(mcpArgsMsg(1, "c1", "ghost"), false)
	nf := fs.recvNonHeartbeat().msg.GetExecClientMessage().GetMcpResult().GetToolNotFound()
	if nf == nil || !slices.Equal(nf.GetAvailableTools(), []string{"client-a"}) {
		t.Fatalf("tool_not_found = %v", nf)
	}
	fs.recvNonHeartbeat() // stream_close

	probe := mcpArgsMsg(2, "c2", "a")
	probe.GetExecServerMessage().GetMcpArgs().SmartModeApprovalOnly = true
	fs.send(probe, false)
	if fs.recvNonHeartbeat().msg.GetExecClientMessage().GetMcpResult().GetApproved() == nil {
		t.Fatal("approval probe not approved")
	}
	if len(s.PendingToolCalls()) != 0 {
		t.Error("an undeclared tool or a probe became a pending call")
	}
}

func TestGzipBothWays(t *testing.T) {
	up := newFakeUpstream(t)
	big := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100) // > 1 KiB, compressible
	s, fs := startSession(t, up, SystemClock{}, RunParams{Request: runRequest(big)})

	first := fs.recv()
	if first.flags&0x01 == 0 {
		t.Error("a run request over 1 KiB was not gzip-compressed")
	}
	if first.msg.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText() != big {
		t.Error("compressed run request did not round-trip")
	}

	fs.send(textMsg("small"), false)
	fs.send(textMsg(big), true)
	fs.send(turnEndedMsg(1, 1), true)
	fs.end(`{}`)
	var got strings.Builder
	evs, err := drain(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if u, ok := ev.(UpdateEvent); ok {
			got.WriteString(u.Update.GetTextDelta().GetText())
		}
	}
	if got.String() != "small"+big {
		t.Errorf("gzip frames from the server did not decode: %d bytes", got.Len())
	}
}

func TestSmallMessagesAreNotCompressed(t *testing.T) {
	up := newFakeUpstream(t)
	_, fs := startSession(t, up, SystemClock{}, RunParams{})
	if fs.recv().flags&0x01 != 0 {
		t.Error("a small run request was compressed")
	}
}

func TestHeartbeatEveryFiveSeconds(t *testing.T) {
	up := newFakeUpstream(t)
	clk := newFakeClock()
	_, fs := startSession(t, up, clk, RunParams{})
	fs.recv()
	eventually(t, "timers", func() bool { return clk.active() >= 2 })

	clk.Advance(5*time.Second - time.Millisecond)
	fs.expectNone()
	if len(fs.in) != 0 {
		t.Fatal("heartbeat sent early")
	}
	clk.Advance(time.Millisecond)
	if fs.recv().msg.GetClientHeartbeat() == nil {
		t.Fatal("no heartbeat after 5s")
	}
	clk.Advance(5 * time.Second)
	if fs.recv().msg.GetClientHeartbeat() == nil {
		t.Fatal("no second heartbeat after another 5s")
	}
}

func TestStallAfterThirtySecondsWithoutTraffic(t *testing.T) {
	up := newFakeUpstream(t)
	clk := newFakeClock()
	s, fs := startSession(t, up, clk, RunParams{})
	fs.recv()
	fs.send(textMsg("hi"), false)
	nextEvent(t, s)
	eventually(t, "timers", func() bool { return clk.active() >= 2 })

	clk.Advance(29 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if ev, err := s.Next(ctx); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("session ended or yielded %v / %v before 30s", ev, err)
	}

	// Traffic resets the countdown.
	fs.send(textMsg("still here"), false)
	nextEvent(t, s)
	clk.Advance(29 * time.Second)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := s.Next(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled although traffic arrived 29s ago: %v", err)
	}

	clk.Advance(time.Second)
	if err := nextErr(t, s); !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if ClassOf(ErrStalled) != ClassRetryable {
		t.Error("a stall should be retryable")
	}
	select {
	case <-fs.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("stalled session left its stream open")
	}
}

func TestContextCancellationClosesTheStream(t *testing.T) {
	up := newFakeUpstream(t)
	c := up.client(t, SystemClock{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.Run(ctx, "test-token", RunParams{Request: runRequest("hi")})
	if err != nil {
		t.Fatal(err)
	}
	fs := up.accept()
	fs.recv()

	cancel()
	select {
	case <-fs.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("server never saw the stream close after the context was cancelled")
	}
	if err := nextErr(t, s); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case <-s.Done():
	case <-time.After(waitTimeout):
		t.Fatal("goroutines still running after cancellation")
	}
}

func TestUnbindKeepsTheSessionAliveAfterTheRequestContext(t *testing.T) {
	up := newFakeUpstream(t)
	c := up.client(t, SystemClock{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := c.Run(ctx, "test-token", RunParams{Request: runRequest("hi")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	fs := up.accept()
	fs.recv()

	s.Unbind()
	cancel()
	select {
	case <-fs.ctxDone:
		t.Fatal("an unbound session was closed with its old context")
	case <-time.After(150 * time.Millisecond):
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	s.Bind(ctx2)
	cancel2()
	select {
	case <-fs.ctxDone:
	case <-time.After(waitTimeout):
		t.Fatal("a rebound session ignored its new context")
	}
}

func TestCloseIsIdempotentAndReportsErrClosed(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{})
	fs.recv()
	s.Close()
	s.Close()
	if err := nextErr(t, s); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v", err)
	}
}

func TestGoroutinePanicBecomesAnError(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{})
	fs.recv()

	s.goSafe("test", func() { panic("boom") })
	var pe *PanicError
	if err := nextErr(t, s); !errors.As(err, &pe) || pe.Where != "test" {
		t.Fatalf("err = %v, want a *PanicError", err)
	}
}

func TestTruncatedStreamIsAnError(t *testing.T) {
	up := newFakeUpstream(t)
	s, fs := startSession(t, up, SystemClock{}, RunParams{})
	fs.recv()
	fs.send(textMsg("partial"), false)
	fs.finish() // the response ends with no end-of-stream frame and no turn_ended
	_, err := drain(t, s)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

// Cursor's model gives up on the task after a bare refusal of a built-in tool;
// the refusal must name the client's tools so it switches to them.
func TestBuiltInRefusalNamesTheClientTools(t *testing.T) {
	up := newFakeUpstream(t)
	tool := testTool("Read")
	_, fs := startSession(t, up, SystemClock{}, RunParams{Tools: []*agentv1.McpToolDefinition{tool}})
	fs.recv()

	fs.send(execMsg(30, "read", func(m *agentv1.ExecServerMessage) {
		m.Message = &agentv1.ExecServerMessage_ReadArgs{ReadArgs: &agentv1.ReadArgs{Path: "note.txt"}}
	}), false)
	th := fs.recvNonHeartbeat().msg.GetExecClientControlMessage().GetThrow()
	if th.GetErrorCode() != "tool_unavailable" || !strings.Contains(th.GetError(), tool.GetName()) {
		t.Fatalf("refusal %q (code %q) does not name the client tool %q", th.GetError(), th.GetErrorCode(), tool.GetName())
	}
}

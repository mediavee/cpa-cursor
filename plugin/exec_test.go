package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	aiserverv1 "github.com/mediavee/cpa-cursor/internal/gen/aiserver/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

func userMsg(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

func chatBody(model string, msgs ...map[string]any) map[string]any {
	return map[string]any{"model": model, "messages": msgs}
}

var weatherTool = map[string]any{"type": "function", "function": map[string]any{
	"name": "get_weather", "description": "Get the weather of a city.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
}}

func (h *harness) execRequest(authID, token, model string, body map[string]any, streamID string) []byte {
	return mustJSON(execRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID: authID, AuthProvider: "cursor", Model: model, Stream: streamID != "",
			Payload: mustJSON(body), StorageJSON: authFile(map[string]any{"access_token": token}),
		},
		StreamID: streamID,
	})
}

// execute runs executor.execute and decodes the chat completion.
func (h *harness) execute(authID, token, model string, body map[string]any) (*translate.ChatResponse, *pluginabi.Error) {
	h.t.Helper()
	res, e := h.call(pluginabi.MethodExecutorExecute, h.execRequest(authID, token, model, body, ""))
	if e != nil {
		return nil, e
	}
	var out pluginapi.ExecutorResponse
	if err := json.Unmarshal(res, &out); err != nil {
		h.t.Fatal(err)
	}
	var resp translate.ChatResponse
	if err := json.Unmarshal(out.Payload, &resp); err != nil {
		h.t.Fatalf("payload is not a chat completion: %v\n%s", err, out.Payload)
	}
	return &resp, nil
}

// stream runs executor.execute_stream and returns the SSE data lines once CPA's stream is closed.
func (h *harness) stream(authID, token, model string, body map[string]any, id string) ([]string, map[string]any, *pluginabi.Error) {
	h.t.Helper()
	res, e := h.call(pluginabi.MethodExecutorExecuteStream, h.execRequest(authID, token, model, body, id))
	if e != nil {
		return nil, nil, e
	}
	var head struct{ Headers http.Header }
	if err := json.Unmarshal(res, &head); err != nil || head.Headers.Get("Content-Type") != "text/event-stream" {
		h.t.Fatalf("stream headers = %s", res)
	}
	closed := h.host.waitClosed(h.t, id)
	return h.host.stream(id), closed, nil
}

func content(r *translate.ChatResponse) string {
	if r.Choices[0].Message.Content == nil {
		return ""
	}
	return *r.Choices[0].Message.Content
}

func TestExecuteReturnsAChatCompletionWithUsage(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) { r.text("Hel"); r.text("lo"); r.turnEnded(11000, 4) })
	token := h.token(time.Hour, "s")

	resp, e := h.execute("auth-1", token, "composer-x", chatBody("composer-x", userMsg("say hello")))
	if e != nil {
		t.Fatalf("execute: %+v", e)
	}
	if content(resp) != "Hello" || resp.Choices[0].FinishReason != "stop" || resp.Model != "composer-x" || resp.Object != "chat.completion" {
		t.Errorf("response = %+v", resp)
	}
	if u := resp.Usage; u == nil || u.PromptTokens != 11000 || u.CompletionTokens != 4 || u.TotalTokens != 11004 {
		t.Errorf("usage = %+v: CPA reads it from the response", u)
	}

	run := <-h.up.runSeen
	if run.request.GetModelDetails().GetModelId() != "composer-x" || run.request.GetRequestedModel().GetModelId() != "composer-x" {
		t.Errorf("model = %v / %v", run.request.GetModelDetails(), run.request.GetRequestedModel())
	}
	if got := run.header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("the session token must be the bearer, got %q", got)
	}
	if run.header.Get("X-Cursor-Client-Type") != "cli" || !strings.HasPrefix(run.header.Get("X-Cursor-Client-Version"), "cli-") {
		t.Errorf("headers = %v", run.header)
	}
}

func TestExecuteMapsReasoningEffortThroughTheAccountCatalog(t *testing.T) {
	h := newHarness(t)
	body := chatBody("claude-t-low", userMsg("think"))
	body["reasoning_effort"] = "high"
	if _, e := h.execute("auth-1", h.token(time.Hour, "s"), "claude-t-low", body); e != nil {
		t.Fatalf("execute: %+v", e)
	}
	run := <-h.up.runSeen
	if got := run.request.GetModelDetails().GetModelId(); got != "claude-t-high" {
		t.Errorf("model_details = %q, want the high-effort slug", got)
	}
	rm := run.request.GetRequestedModel()
	if rm.GetModelId() != "claude-t" || len(rm.GetParameters()) != 1 || rm.GetParameters()[0].GetId() != "effort" || rm.GetParameters()[0].GetValue() != "high" {
		t.Errorf("requested_model = %v", rm)
	}
}

func TestExecuteRejectsABadRequestWithoutCallingCursor(t *testing.T) {
	h := newHarness(t)
	token := h.token(time.Hour, "s")
	for name, body := range map[string]map[string]any{
		"no messages":   {"model": "composer-x", "messages": []any{}},
		"last is model": chatBody("composer-x", userMsg("q"), map[string]any{"role": "assistant", "content": "a"}),
	} {
		if _, e := h.execute("auth-1", token, "composer-x", body); e == nil || e.HTTPStatus != 400 {
			t.Errorf("%s: error = %+v, want 400", name, e)
		}
	}
	if _, e := h.execute("auth-1", token, "gpt-nope", chatBody("gpt-nope", userMsg("q"))); e == nil || e.HTTPStatus != 400 || !strings.Contains(e.Message, "gpt-nope") {
		t.Errorf("unknown model: %+v", e)
	} else if strings.Contains(strings.ToLower(e.Message), "not supported") || strings.Contains(strings.ToLower(e.Message), "unavailable") {
		// CPA cools a model down for 12 h on messages like these.
		t.Errorf("the message trips CPA's model-not-supported classifier: %q", e.Message)
	}
	if h.up.runCount() != 0 {
		t.Errorf("%d runs were started for requests that cannot be sent", h.up.runCount())
	}
}

func TestExecuteWithAnExpiredTokenAsksToReconnect(t *testing.T) {
	h := newHarness(t)
	_, e := h.execute("auth-1", h.token(-time.Second, "s"), "composer-x", chatBody("composer-x", userMsg("q")))
	if e == nil || e.HTTPStatus != 401 || !strings.Contains(e.Message, "reconnection required") {
		t.Errorf("error = %+v", e)
	}
}

func TestExecuteStreamSendsSSEWithUsageInTheLastChunk(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) { r.text("Hel"); r.text("lo"); r.turnEnded(7, 2) })
	lines, closed, e := h.stream("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")), "s1")
	if e != nil {
		t.Fatalf("stream: %+v", e)
	}
	if closed["error"] != nil {
		t.Fatalf("stream failed: %v", closed)
	}
	if len(lines) < 4 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("lines = %q", lines)
	}
	var text strings.Builder
	var last translate.ChatChunk
	for _, l := range lines[:len(lines)-1] {
		var c translate.ChatChunk
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatalf("chunk %q: %v", l, err)
		}
		text.WriteString(c.Choices[0].Delta.Content)
		last = c
	}
	if text.String() != "Hello" {
		t.Errorf("streamed text = %q", text.String())
	}
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" || last.Usage == nil || last.Usage.PromptTokens != 7 || last.Usage.CompletionTokens != 2 {
		t.Errorf("last chunk = %+v: finish reason and usage belong to it", last)
	}
	first := lines[0]
	if !strings.Contains(first, `"role":"assistant"`) {
		t.Errorf("first chunk = %s", first)
	}
}

func TestExecuteStreamFailureBeforeTheFirstChunkFailsTheCall(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) {
		r.endError(connectError(t, "resource_exhausted", "limit", errDetails(aiserverv1.ErrorDetails_ERROR_PRO_USER_USAGE_LIMIT, "Usage limit", "upgrade")))
	})
	_, _, e := h.stream("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")), "s-fail")
	if e == nil || e.HTTPStatus != 429 {
		t.Fatalf("error = %+v: an error before any output must fail the call so CPA can cool down and fail over", e)
	}
}

func TestExecuteStreamFailureMidStreamKeepsStatusAndRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) {
		r.text("partial")
		r.endError(connectError(t, "resource_exhausted", "slow down", errDetails(aiserverv1.ErrorDetails_ERROR_RATE_LIMITED, "Rate limit", "upgrade")))
	})
	lines, closed, e := h.stream("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")), "s-mid")
	if e != nil {
		t.Fatalf("stream: %+v", e)
	}
	detail, _ := closed["error_detail"].(map[string]any)
	if detail == nil || detail["http_status"] != float64(429) || detail["retry_after_seconds"] != float64(30) {
		t.Errorf("close = %v: the failure must keep its status and retry_after", closed)
	}
	if len(lines) == 0 || !strings.Contains(lines[0], "partial") {
		t.Errorf("lines = %q", lines)
	}
	if slices.Contains(lines, "[DONE]") {
		t.Error("a failed stream must not end like a finished one")
	}
}

func TestExecuteStreamStopsTheRunWhenTheHostClosesTheStream(t *testing.T) {
	h := newHarness(t)
	h.p.probeEvery = 20 * time.Millisecond
	h.up.setScript(func(r *fakeRun) {
		r.text("first")
		select {
		case <-r.gone:
		case <-time.After(waitTimeout):
			t.Error("the run was not abandoned although the client left")
		}
	})
	// The client has already left when the first chunk is emitted.
	gone := errors.New("stream 7 is not open")
	h.host.emitErr.Store(&gone)
	if _, e := h.call(pluginabi.MethodExecutorExecuteStream, h.execRequest("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")), "s7")); e != nil {
		t.Fatalf("execute_stream: %+v", e)
	}
	run := <-h.up.runSeen
	select {
	case <-run.gone:
	case <-time.After(waitTimeout):
		t.Fatal("the Cursor run keeps going after CPA closed the stream")
	}
}

func TestIdleStreamNoticesThatTheClientLeft(t *testing.T) {
	h := newHarness(t)
	h.p.probeEvery = 20 * time.Millisecond
	h.up.setScript(func(r *fakeRun) {
		r.text("first")
		// Outlives the test's wait, so only the plugin can make the run end.
		select {
		case <-r.gone:
		case <-time.After(4 * waitTimeout):
		}
	})
	if _, e := h.call(pluginabi.MethodExecutorExecuteStream, h.execRequest("auth-1", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("hi")), "s8")); e != nil {
		t.Fatalf("execute_stream: %+v", e)
	}
	run := <-h.up.runSeen
	eventuallyTrue(t, "the first chunk", func() bool { return len(h.host.stream("s8")) > 0 })
	// Nothing flows from Cursor any more; the client leaves.
	gone := errors.New("stream 8 is not open")
	h.host.emitErr.Store(&gone)
	select {
	case <-run.gone:
	case <-time.After(waitTimeout):
		t.Fatal("an idle stream never noticed that CPA closed it")
	}
}

func TestToolLoopResumesTheParkedRun(t *testing.T) {
	h := newHarness(t)
	got := make(chan string, 1)
	h.up.setScript(func(r *fakeRun) {
		r.toolCall(1, "call_1", "get_weather", "Paris")
		_, result := r.waitMcpResult()
		got <- result
		r.text("It is sunny.")
		r.turnEnded(20, 5)
	})
	token := h.token(time.Hour, "s")
	body1 := chatBody("composer-x", userMsg("weather in Paris?"))
	body1["tools"] = []any{weatherTool}

	resp1, e := h.execute("auth-1", token, "composer-x", body1)
	if e != nil {
		t.Fatalf("request 1: %+v", e)
	}
	msg := resp1.Choices[0].Message
	if resp1.Choices[0].FinishReason != "tool_calls" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_1" || msg.ToolCalls[0].Function.Name != "get_weather" || !strings.Contains(msg.ToolCalls[0].Function.Arguments, "Paris") {
		t.Fatalf("response 1 = %+v", resp1.Choices[0])
	}
	if h.up.runCount() != 1 {
		t.Fatalf("runs = %d", h.up.runCount())
	}

	body2 := chatBody("composer-x", userMsg("weather in Paris?"),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": msg.ToolCalls},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "Sunny, 21C"})
	body2["tools"] = []any{weatherTool}
	resp2, e := h.execute("auth-1", token, "composer-x", body2)
	if e != nil {
		t.Fatalf("request 2: %+v", e)
	}
	if content(resp2) != "It is sunny." || resp2.Choices[0].FinishReason != "stop" {
		t.Errorf("response 2 = %+v", resp2.Choices[0])
	}
	if result := <-got; result != "Sunny, 21C" {
		t.Errorf("the parked run received %q", result)
	}
	if h.up.runCount() != 1 {
		t.Errorf("runs = %d: the second request must resume the parked run, not start another", h.up.runCount())
	}
	if !h.host.logged("delivered to the parked Cursor run") || h.host.logged("replaying the conversation") {
		t.Error("the log must say the parked run was resumed")
	}
	if resp2.Usage == nil || resp2.Usage.PromptTokens != 20 {
		t.Errorf("usage of the resumed turn = %+v", resp2.Usage)
	}
}

func TestToolLoopInStreamingParksBeforeTheStreamEnds(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) {
		r.toolCall(1, "call_s", "get_weather", "Rome")
		_, _ = r.waitMcpResult()
		r.text("Done.")
		r.turnEnded(1, 1)
	})
	token := h.token(time.Hour, "s")
	body1 := chatBody("composer-x", userMsg("weather?"))
	body1["tools"] = []any{weatherTool}
	lines, closed, e := h.stream("auth-1", token, "composer-x", body1, "s-tool")
	if e != nil || closed["error"] != nil {
		t.Fatalf("stream: %+v %v", e, closed)
	}
	// By the time CPA's client could answer (the stream is closed), the run must be findable.
	if n := h.p.rt.Load().registry("auth-1").Len(); n != 1 {
		t.Errorf("parked runs = %d when the stream closed, want 1", n)
	}
	var sawCall bool
	for _, l := range lines {
		if strings.Contains(l, `"tool_calls"`) && strings.Contains(l, "call_s") {
			sawCall = true
		}
	}
	last := lines[len(lines)-2]
	if !sawCall || !strings.Contains(last, `"finish_reason":"tool_calls"`) {
		t.Errorf("lines = %q", lines)
	}
}

func TestAToolAnswerOnAnotherCredentialReplaysWithoutState(t *testing.T) {
	h := newHarness(t)
	var runs int
	h.up.setScript(func(r *fakeRun) {
		runs++
		if runs == 1 {
			r.toolCall(1, "call_x", "get_weather", "Oslo")
			select {
			case <-r.gone:
			case <-time.After(waitTimeout):
				t.Error("the superseded parked run was not closed")
			}
			return
		}
		r.text("Replayed.")
		r.turnEnded(9, 1)
	})
	token := h.token(time.Hour, "s")
	body1 := chatBody("composer-x", userMsg("weather?"))
	body1["tools"] = []any{weatherTool}
	resp1, e := h.execute("auth-A", token, "composer-x", body1)
	if e != nil {
		t.Fatalf("request 1: %+v", e)
	}
	body2 := chatBody("composer-x", userMsg("weather?"),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": resp1.Choices[0].Message.ToolCalls},
		map[string]any{"role": "tool", "tool_call_id": "call_x", "content": "Cold"})
	body2["tools"] = []any{weatherTool}
	resp2, e := h.execute("auth-B", token, "composer-x", body2)
	if e != nil {
		t.Fatalf("request 2: %+v", e)
	}
	if content(resp2) != "Replayed." {
		t.Errorf("response 2 = %+v", resp2.Choices[0])
	}
	if h.up.runCount() != 2 {
		t.Fatalf("runs = %d, want a second run for the other credential", h.up.runCount())
	}
	<-h.up.runSeen
	second := <-h.up.runSeen
	if second.request.GetAction().GetResumeAction() == nil || len(second.request.GetConversationState().GetRootPromptMessagesJson()) == 0 {
		t.Errorf("the replay must carry the conversation: %v", second.request.GetAction())
	}
	if !h.host.logged("replaying the conversation") {
		t.Error("the log must say the conversation was replayed")
	}
	if n := h.p.rt.Load().registry("auth-A").Len(); n != 0 {
		t.Errorf("credential A still parks %d runs: a run belongs to one credential and the replay supersedes it", n)
	}
}

func TestAToolAnswerAfterTheTTLReplaysWithoutState(t *testing.T) {
	h := newHarness(t, "park_ttl: 50ms")
	var runs int
	h.up.setScript(func(r *fakeRun) {
		runs++
		if runs == 1 {
			r.toolCall(1, "call_t", "get_weather", "Oslo")
			<-r.gone
			return
		}
		r.text("Replayed.")
		r.turnEnded(9, 1)
	})
	token := h.token(time.Hour, "s")
	body1 := chatBody("composer-x", userMsg("weather?"))
	body1["tools"] = []any{weatherTool}
	resp1, _ := h.execute("auth-1", token, "composer-x", body1)
	eventuallyTrue(t, "the TTL to close the parked run", func() bool { return h.p.rt.Load().registry("auth-1").Len() == 0 })

	body2 := chatBody("composer-x", userMsg("weather?"),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": resp1.Choices[0].Message.ToolCalls},
		map[string]any{"role": "tool", "tool_call_id": "call_t", "content": "Cold"})
	body2["tools"] = []any{weatherTool}
	resp2, e := h.execute("auth-1", token, "composer-x", body2)
	if e != nil || content(resp2) != "Replayed." || h.up.runCount() != 2 {
		t.Errorf("resp = %+v err = %+v runs = %d", resp2, e, h.up.runCount())
	}
}

func TestReconfigureWithTheSameConfigKeepsParkedRuns(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) {
		r.toolCall(1, "call_k", "get_weather", "Oslo")
		r.waitMcpResult()
		r.text("Resumed.")
		r.turnEnded(1, 1)
	})
	token := h.token(time.Hour, "s")
	body1 := chatBody("composer-x", userMsg("weather?"))
	body1["tools"] = []any{weatherTool}
	resp1, _ := h.execute("auth-1", token, "composer-x", body1)

	h.register() // CPA re-sends the unchanged config on every config reload

	body2 := chatBody("composer-x", userMsg("weather?"),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": resp1.Choices[0].Message.ToolCalls},
		map[string]any{"role": "tool", "tool_call_id": "call_k", "content": "Cold"})
	body2["tools"] = []any{weatherTool}
	resp2, e := h.execute("auth-1", token, "composer-x", body2)
	if e != nil || content(resp2) != "Resumed." || h.up.runCount() != 1 {
		t.Errorf("resp = %+v err = %+v runs = %d: an unchanged config must keep the parked run", resp2, e, h.up.runCount())
	}
}

func TestCountTokensIsAnEstimateOfTheClientContent(t *testing.T) {
	h := newHarness(t)
	body := chatBody("composer-x", userMsg(strings.Repeat("a", 400)))
	res, e := h.call(pluginabi.MethodExecutorCountTokens, mustJSON(execRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: "auth-1", Model: "composer-x", Payload: mustJSON(body)}}))
	if e != nil {
		t.Fatal(e)
	}
	var out pluginapi.ExecutorResponse
	_ = json.Unmarshal(res, &out)
	var count struct {
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Payload, &count); err != nil {
		t.Fatal(err)
	}
	// 400 bytes at 4 bytes per token, plus 4 for the message.
	if count.Usage.PromptTokens != 104 || count.Usage.TotalTokens != 104 {
		t.Errorf("count = %+v, want 104", count)
	}
	if h.up.runCount() != 0 {
		t.Error("counting tokens must not start a run")
	}
}

func TestHTTPRequestIsNotSupported(t *testing.T) {
	h := newHarness(t)
	if _, e := h.call(pluginabi.MethodExecutorHTTPRequest, []byte(`{}`)); e == nil || e.HTTPStatus != 501 {
		t.Errorf("error = %+v", e)
	}
}

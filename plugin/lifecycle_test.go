package plugin

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestClientLeavingBeforeTheFirstOutputStopsTheRun(t *testing.T) {
	h := newHarness(t, "request_timeout: 2s")
	h.p.probeEvery = 20 * time.Millisecond
	h.up.setScript(func(r *fakeRun) { <-r.gone })
	returned := make(chan *pluginabi.Error, 1)
	go func() {
		_, e := h.call(pluginabi.MethodExecutorExecuteStream, h.execRequest("a", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("tiny")), "early"))
		returned <- e
	}()
	run := <-h.up.runSeen
	gone := errors.New("stream is not open")
	h.host.emitErr.Store(&gone)
	select {
	case <-run.gone:
	case <-time.After(waitTimeout / 5):
		t.Fatal("the client left before the first output, but the run was not stopped")
	}
	select {
	case e := <-returned:
		if e == nil {
			t.Error("execute_stream reported success for a stream nobody reads")
		}
	case <-time.After(waitTimeout):
		t.Fatal("execute_stream is still waiting for the first output of a cancelled run")
	}
}

func TestClientLeavingWhileTheRequestIsPreparedStopsIt(t *testing.T) {
	h := newHarness(t, "request_timeout: 5s")
	h.p.probeEvery = 20 * time.Millisecond
	gone := errors.New("stream is not open")
	h.host.emitErr.Store(&gone)
	// The model list is slow: the stream is already closed when the run would be opened.
	h.up.handle("/agent.v1.AgentService/GetUsableModels", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(waitTimeout):
		}
	})
	start := time.Now()
	_, e := h.call(pluginabi.MethodExecutorExecuteStream, h.execRequest("a", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("tiny")), "early-prepare"))
	if e == nil || time.Since(start) > waitTimeout/2 {
		t.Errorf("error = %+v after %v: preparing a request nobody waits for must stop at once", e, time.Since(start))
	}
	if h.up.runCount() != 0 {
		t.Errorf("%d run(s) opened for a client that had left", h.up.runCount())
	}
}

func TestInFlightRunDoesNotParkOnARetiredRuntime(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.up.setScript(func(r *fakeRun) {
		<-release
		r.toolCall(1, "late-call", "get_weather", "Paris")
		<-r.gone
	})
	old := h.p.rt.Load()
	type result struct {
		finish string
		err    *pluginabi.Error
	}
	done := make(chan result, 1)
	go func() {
		body := chatBody("composer-x", userMsg("tiny"))
		body["tools"] = []any{weatherTool}
		resp, e := h.execute("a", h.token(time.Hour, "s"), "composer-x", body)
		if e != nil {
			done <- result{err: e}
			return
		}
		done <- result{finish: resp.Choices[0].FinishReason}
	}()
	run := <-h.up.runSeen
	h.register("client_version: cli-other")
	close(release)

	res := <-done
	if res.err != nil || res.finish != "tool_calls" {
		t.Errorf("result = %+v: a request that outlives its runtime still answers, its tool calls are replayed later", res)
	}
	select {
	case <-run.gone:
	case <-time.After(waitTimeout / 5):
		t.Error("the retired runtime left the upstream run open")
	}
	if n := old.registry("a").Len(); n != 0 {
		t.Errorf("the retired runtime parks %d run(s) nobody can resume", n)
	}
}

func TestQuiesceReleasesParkedRunsAndAReRegisterStartsFresh(t *testing.T) {
	h := newHarness(t)
	h.up.setScript(func(r *fakeRun) {
		r.toolCall(1, "parked-call", "get_weather", "Paris")
		<-r.gone
	})
	body := chatBody("composer-x", userMsg("tiny"))
	body["tools"] = []any{weatherTool}
	if _, e := h.execute("a", h.token(time.Hour, "s"), "composer-x", body); e != nil {
		t.Fatal(e)
	}
	run := <-h.up.runSeen
	if _, e := h.call(pluginabi.MethodPluginQuiesce, []byte(`{}`)); e != nil {
		t.Fatal(e)
	}
	select {
	case <-run.gone:
	case <-time.After(waitTimeout / 5):
		t.Fatal("plugin.quiesce left a parked run open in a retired image")
	}
	if _, e := h.execute("a", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("again"))); e == nil || e.HTTPStatus != 503 {
		t.Errorf("a quiesced plugin must refuse work, got %+v", e)
	}

	// CPA rolls a failed replacement back by registering the old image again.
	h.register()
	h.up.setScript(nil)
	if resp, e := h.execute("a", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("again"))); e != nil || content(resp) != "ok" {
		t.Errorf("after the rollback: %v %+v", resp, e)
	}
}

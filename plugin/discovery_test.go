package plugin

import (
	"testing"
	"time"
)

const serverConfigPath = "/aiserver.v1.ServerConfigService/GetServerConfig"

func TestRunsGoToTheAgentHostCursorAnnounces(t *testing.T) {
	h := newHarness(t)
	announced := newFakeCursor(t)
	h.up.reply(serverConfigPath, `{"agentUrlConfig":{"agentUrl":"`+announced.url()+`","agentnUrl":"`+announced.url()+`"}}`)
	h.up.setScript(func(r *fakeRun) {
		r.endError(`{"error":{"code":"unavailable","message":"the configured agent host is not the announced one"}}`)
	})
	token := h.token(time.Hour, "s")

	for range 2 {
		resp, e := h.execute("a", token, "composer-x", chatBody("composer-x", userMsg("tiny")))
		if e != nil || content(resp) != "ok" {
			t.Fatalf("response = %v, error = %+v", resp, e)
		}
	}
	if h.up.runCount() != 0 || announced.runCount() != 2 {
		t.Errorf("runs: configured host %d, announced host %d; want 0 and 2", h.up.runCount(), announced.runCount())
	}
	if n := h.up.count(serverConfigPath); n != 1 {
		t.Errorf("GetServerConfig called %d times for two runs, want 1 (the client caches it)", n)
	}
}

func TestDiscoveryFailureFallsBackToTheConfiguredAgentHost(t *testing.T) {
	h := newHarness(t)
	h.up.replyStatus(serverConfigPath, 500, `{"code":"internal","message":"x"}`)
	resp, e := h.execute("a", h.token(time.Hour, "s"), "composer-x", chatBody("composer-x", userMsg("tiny")))
	if e != nil || content(resp) != "ok" || h.up.runCount() != 1 {
		t.Errorf("response = %v, error = %+v, runs on the configured host = %d", resp, e, h.up.runCount())
	}
}

func TestAResumedRunDoesNotAskForTheServerConfig(t *testing.T) {
	h := newHarness(t)
	// A failed discovery is not cached, so every new run asks again and a resume is the one that must not.
	h.up.replyStatus(serverConfigPath, 500, `{"code":"internal","message":"x"}`)
	h.up.setScript(func(r *fakeRun) {
		r.toolCall(1, "call_d", "get_weather", "Rome")
		r.waitMcpResult()
		r.text("done")
		r.turnEnded(1, 1)
	})
	token := h.token(time.Hour, "s")
	body := chatBody("composer-x", userMsg("weather?"))
	body["tools"] = []any{weatherTool}
	resp1, e := h.execute("a", token, "composer-x", body)
	if e != nil {
		t.Fatal(e)
	}
	before := h.up.count(serverConfigPath)
	body2 := chatBody("composer-x", userMsg("weather?"),
		map[string]any{"role": "assistant", "content": nil, "tool_calls": resp1.Choices[0].Message.ToolCalls},
		map[string]any{"role": "tool", "tool_call_id": "call_d", "content": "Sunny"})
	body2["tools"] = []any{weatherTool}
	if _, e := h.execute("a", token, "composer-x", body2); e != nil {
		t.Fatal(e)
	}
	if h.up.runCount() != 1 || h.up.count(serverConfigPath) != before {
		t.Errorf("runs = %d, GetServerConfig calls %d -> %d", h.up.runCount(), before, h.up.count(serverConfigPath))
	}
}

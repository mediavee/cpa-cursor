package translate_test

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

func text(s string) *agentv1.InteractionUpdate {
	return &agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: s}}}
}

func thinking(s string) *agentv1.InteractionUpdate {
	return &agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_ThinkingDelta{ThinkingDelta: &agentv1.ThinkingDeltaUpdate{Text: s}}}
}

func turnEnded(input, output, cacheRead, cacheWrite, reasoning int64) *agentv1.InteractionUpdate {
	return &agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TurnEnded{TurnEnded: &agentv1.TurnEndedUpdate{
		InputTokens: &input, OutputTokens: &output, CacheReadTokens: &cacheRead, CacheWriteTokens: &cacheWrite, ReasoningTokens: &reasoning,
	}}}
}

func mcpArgs(callID, tool string, args map[string]any) *agentv1.McpArgs {
	m := map[string]*structpb.Value{}
	for k, v := range args {
		pv, err := structpb.NewValue(v)
		if err != nil {
			panic(err)
		}
		m[k] = pv
	}
	return &agentv1.McpArgs{Name: "client-" + tool, ToolName: tool, ProviderIdentifier: "client", ToolCallId: callID, Args: m}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const head = `"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"composer-2.5"`

func TestStreamText(t *testing.T) {
	enc := translate.NewEncoder("chatcmpl-1", "composer-2.5", 1700000000)
	var got []string
	feed := func(u *agentv1.InteractionUpdate) {
		for _, c := range enc.Update(u) {
			got = append(got, marshal(t, c))
		}
	}
	feed(thinking("Let me think."))
	feed(text("Hel"))
	feed(text("lo"))
	feed(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_TextDelta{TextDelta: &agentv1.TextDeltaUpdate{Text: "You hit a limit", IsServerNotice: true}}})
	feed(text(""))
	feed(&agentv1.InteractionUpdate{Message: &agentv1.InteractionUpdate_Heartbeat{Heartbeat: &agentv1.HeartbeatUpdate{}}})
	feed(turnEnded(4242, 177, 20288, 5, 33))
	got = append(got, marshal(t, enc.Finish(translate.FinishStop)))

	want := []string{
		`{` + head + `,"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think."},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":24530,"completion_tokens":177,"total_tokens":24707,` +
			`"prompt_tokens_details":{"cached_tokens":20288},"completion_tokens_details":{"reasoning_tokens":33}}}`,
	}
	if !equalStrings(got, want) {
		t.Errorf("chunks\n got: %q\nwant: %q", got, want)
	}

	resp := marshal(t, enc.Response())
	wantResp := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"composer-2.5",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello","reasoning_content":"Let me think."},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":24530,"completion_tokens":177,"total_tokens":24707,` +
		`"prompt_tokens_details":{"cached_tokens":20288},"completion_tokens_details":{"reasoning_tokens":33}}}`
	if resp != wantResp {
		t.Errorf("response\n got: %s\nwant: %s", resp, wantResp)
	}
}

func TestStreamToolCalls(t *testing.T) {
	enc := translate.NewEncoder("chatcmpl-1", "composer-2.5", 1700000000)
	var got []string
	for _, c := range enc.Update(text("Checking.")) {
		got = append(got, marshal(t, c))
	}
	for _, a := range []*agentv1.McpArgs{
		mcpArgs("tool_a", "get_weather", map[string]any{"city": "Paris", "days": 3, "detail": map[string]any{"hourly": true}}),
		mcpArgs("tool_b", "ping", nil),
	} {
		c, err := enc.ToolCall(a)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, marshal(t, c))
	}
	got = append(got, marshal(t, enc.Finish(translate.FinishToolCalls)))

	want := []string{
		`{` + head + `,"choices":[{"index":0,"delta":{"role":"assistant","content":"Checking."},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"tool_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\",\"days\":3,\"detail\":{\"hourly\":true}}"}}]},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"tool_b","type":"function","function":{"name":"ping","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	if !equalStrings(got, want) {
		t.Errorf("chunks\n got: %q\nwant: %q", got, want)
	}

	resp := marshal(t, enc.Response())
	wantResp := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"composer-2.5","choices":[{"index":0,"message":{"role":"assistant","content":"Checking.","tool_calls":[` +
		`{"id":"tool_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\",\"days\":3,\"detail\":{\"hourly\":true}}"}},` +
		`{"id":"tool_b","type":"function","function":{"name":"ping","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`
	if resp != wantResp {
		t.Errorf("response\n got: %s\nwant: %s", resp, wantResp)
	}
}

func TestResponseShapes(t *testing.T) {
	t.Run("tool calls alone leave the content null", func(t *testing.T) {
		enc := translate.NewEncoder("chatcmpl-1", "m", 1)
		if _, err := enc.ToolCall(mcpArgs("tool_a", "f", nil)); err != nil {
			t.Fatal(err)
		}
		enc.Finish(translate.FinishToolCalls)
		if got := marshal(t, enc.Response().Choices[0].Message.Content); got != "null" {
			t.Errorf("content = %s, want null", got)
		}
	})
	t.Run("a length error finishes with length", func(t *testing.T) {
		enc := translate.NewEncoder("chatcmpl-1", "m", 1)
		enc.Update(text("cut"))
		last := enc.Finish(translate.FinishLength)
		if r := enc.Response().Choices[0].FinishReason; r != "length" || *last.Choices[0].FinishReason != "length" {
			t.Errorf("finish = %q / %q, want length", r, *last.Choices[0].FinishReason)
		}
	})
	t.Run("an empty answer still opens with the role and an empty string content", func(t *testing.T) {
		enc := translate.NewEncoder("chatcmpl-1", "m", 1)
		last := enc.Finish(translate.FinishStop)
		if last.Choices[0].Delta.Role != "assistant" {
			t.Errorf("the only chunk must carry the role, got %+v", last.Choices[0].Delta)
		}
		if got := marshal(t, enc.Response().Choices[0].Message.Content); got != `""` {
			t.Errorf("content = %s, want an empty string", got)
		}
	})
	t.Run("no turn_ended means no usage", func(t *testing.T) {
		enc := translate.NewEncoder("chatcmpl-1", "m", 1)
		enc.Update(text("x"))
		if enc.Finish(translate.FinishStop).Usage != nil || enc.Response().Usage != nil {
			t.Error("usage must be omitted when the turn did not report it")
		}
	})
}

func TestCallName(t *testing.T) {
	tests := []struct {
		name string
		args *agentv1.McpArgs
		want string
	}{
		{"tool_name is the client's name", &agentv1.McpArgs{Name: "client-get_weather", ToolName: "get_weather"}, "get_weather"},
		{"a name with dashes survives the prefix", &agentv1.McpArgs{Name: "client-get-weather", ToolName: "get-weather"}, "get-weather"},
		{"without tool_name the prefix is stripped from name", &agentv1.McpArgs{Name: "client-get_weather"}, "get_weather"},
	}
	for _, tt := range tests {
		if got := translate.CallName(tt.args); got != tt.want {
			t.Errorf("%s: CallName = %q, want %q", tt.name, got, tt.want)
		}
	}
}

package translate_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

// catalog is a one-model catalog for tests that do not care about models.
type catalog map[string]translate.Model

func (c catalog) Lookup(slug string) (translate.Model, bool) { m, ok := c[slug]; return m, ok }

var plainCatalog = catalog{"composer-2.5": {ID: "composer-2.5", Slug: "composer-2.5"}}

// decode parses a request the way a client sends it.
func decode(t *testing.T, body string) *translate.ChatRequest {
	t.Helper()
	var req translate.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("bad test request: %v", err)
	}
	return &req
}

func translateOK(t *testing.T, body string) *translate.Run {
	t.Helper()
	run, err := translate.Translate(decode(t, body), translate.Options{Catalog: plainCatalog})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	return run
}

// root returns the JSON of the root prompt blobs, in order.
func root(t *testing.T, run *translate.Run) []string {
	t.Helper()
	var out []string
	for _, id := range run.Request.ConversationState.RootPromptMessagesJson {
		out = append(out, string(blob(t, run, id)))
	}
	return out
}

func blob(t *testing.T, run *translate.Run, id []byte) []byte {
	t.Helper()
	for _, b := range run.Blobs {
		if bytes.Equal(b.ID, id) {
			return b.Data
		}
	}
	t.Fatalf("blob %x is referenced but not served", id)
	return nil
}

func TestTranslateHistory(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantRoot   []string
		wantAction string // text of the user message action
		wantRules  []string
	}{
		{
			name: "system message goes to a system blob and a global rule",
			body: `{"model":"composer-2.5","messages":[
				{"role":"system","content":"Be terse."},
				{"role":"user","content":"hi"}]}`,
			wantRoot:   []string{`{"role":"system","content":"Be terse."}`},
			wantAction: "hi",
			wantRules:  []string{"Be terse."},
		},
		{
			name: "developer is a system message and several are kept apart",
			body: `{"model":"composer-2.5","messages":[
				{"role":"system","content":"One."},
				{"role":"developer","content":[{"type":"text","text":"Two."}]},
				{"role":"user","content":"hi"}]}`,
			wantRoot:   []string{`{"role":"system","content":"One."}`, `{"role":"system","content":"Two."}`},
			wantAction: "hi",
			wantRules:  []string{"One.", "Two."},
		},
		{
			name:       "no system message means no system blob",
			body:       `{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`,
			wantRoot:   nil,
			wantAction: "hi",
		},
		{
			name: "history after a system prompt gets the rules entry, the last user message rides in the action",
			body: `{"model":"composer-2.5","messages":[
				{"role":"system","content":"Be terse."},
				{"role":"user","content":"My word is AZURE-42."},
				{"role":"assistant","content":"Noted."},
				{"role":"user","content":"What is my word?"}]}`,
			wantRoot: []string{
				`{"role":"system","content":"Be terse."}`,
				`{"role":"user","content":"<rules>\nBe terse.\n</rules>"}`,
				`{"role":"user","content":[{"type":"text","text":"My word is AZURE-42."}]}`,
				`{"role":"assistant","content":[{"type":"text","text":"Noted."}]}`,
			},
			wantAction: "What is my word?",
			wantRules:  []string{"Be terse."},
		},
		{
			name: "history without a system prompt has no rules entry",
			body: `{"model":"composer-2.5","messages":[
				{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`,
			wantRoot: []string{
				`{"role":"user","content":[{"type":"text","text":"a"}]}`,
				`{"role":"assistant","content":[{"type":"text","text":"b"}]}`,
			},
			wantAction: "c",
		},
		{
			name: "assistant reasoning is not replayed and an empty assistant message is dropped",
			body: `{"model":"composer-2.5","messages":[
				{"role":"user","content":"a"},
				{"role":"assistant","content":"b","reasoning_content":"thinking about b"},
				{"role":"assistant","content":null},
				{"role":"user","content":"c"}]}`,
			wantRoot: []string{
				`{"role":"user","content":[{"type":"text","text":"a"}]}`,
				`{"role":"assistant","content":[{"type":"text","text":"b"}]}`,
			},
			wantAction: "c",
		},
		{
			name: "an assistant message with several tool calls is one blob followed by one tool blob per result",
			body: `{"model":"composer-2.5","messages":[
				{"role":"user","content":"weather in Paris and Tokyo?"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"tool_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
					{"id":"tool_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},
				{"role":"tool","tool_call_id":"tool_b","content":"Rainy, 12C"},
				{"role":"tool","tool_call_id":"tool_a","content":[{"type":"text","text":"Sunny, 21C"}]},
				{"role":"assistant","content":"Paris is sunny, Tokyo rainy."},
				{"role":"user","content":"thanks"}]}`,
			wantRoot: []string{
				`{"role":"user","content":[{"type":"text","text":"weather in Paris and Tokyo?"}]}`,
				`{"role":"assistant","content":[` +
					`{"type":"tool-call","toolCallId":"tool_a","toolName":"CallDynamicTool","args":{"namespace":"client","toolName":"get_weather","arguments":{"city":"Paris"}}},` +
					`{"type":"tool-call","toolCallId":"tool_b","toolName":"CallDynamicTool","args":{"namespace":"client","toolName":"get_weather","arguments":{"city":"Tokyo"}}}]}`,
				`{"role":"tool","content":[{"type":"tool-result","toolCallId":"tool_b","toolName":"CallDynamicTool","result":"Rainy, 12C","experimental_content":[{"type":"text","text":"Rainy, 12C"}]}],"id":"tool_b"}`,
				`{"role":"tool","content":[{"type":"tool-result","toolCallId":"tool_a","toolName":"CallDynamicTool","result":"Sunny, 21C","experimental_content":[{"type":"text","text":"Sunny, 21C"}]}],"id":"tool_a"}`,
				`{"role":"assistant","content":[{"type":"text","text":"Paris is sunny, Tokyo rainy."}]}`,
			},
			wantAction: "thanks",
		},
		{
			name: "assistant text next to a tool call stays in the same blob; empty arguments are an empty object",
			body: `{"model":"composer-2.5","messages":[
				{"role":"user","content":"time?"},
				{"role":"assistant","content":"Checking.","tool_calls":[{"id":"c1","type":"function","function":{"name":"now","arguments":""}}]},
				{"role":"tool","tool_call_id":"c1","content":"12:00"},
				{"role":"user","content":"ok"}]}`,
			wantRoot: []string{
				`{"role":"user","content":[{"type":"text","text":"time?"}]}`,
				`{"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool-call","toolCallId":"c1","toolName":"CallDynamicTool","args":{"namespace":"client","toolName":"now","arguments":{}}}]}`,
				`{"role":"tool","content":[{"type":"tool-result","toolCallId":"c1","toolName":"CallDynamicTool","result":"12:00","experimental_content":[{"type":"text","text":"12:00"}]}],"id":"c1"}`,
			},
			wantAction: "ok",
		},
		{
			name: "a call id Cursor would reject is sanitized the same way on the call and the result",
			body: `{"model":"composer-2.5","messages":[
				{"role":"user","content":"go"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"call|1.x","type":"function","function":{"name":"f","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call|1.x","content":"r"},
				{"role":"user","content":"again"}]}`,
			wantRoot: []string{
				`{"role":"user","content":[{"type":"text","text":"go"}]}`,
				`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"call_1_x","toolName":"CallDynamicTool","args":{"namespace":"client","toolName":"f","arguments":{}}}]}`,
				`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_1_x","toolName":"CallDynamicTool","result":"r","experimental_content":[{"type":"text","text":"r"}]}],"id":"call_1_x"}`,
			},
			wantAction: "again",
		},
		{
			name: "an empty user message in the history is skipped",
			body: `{"model":"composer-2.5","messages":[
				{"role":"user","content":""},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`,
			wantRoot:   []string{`{"role":"assistant","content":[{"type":"text","text":"b"}]}`},
			wantAction: "c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := translateOK(t, tt.body)
			if got := root(t, run); !equalStrings(got, tt.wantRoot) {
				t.Errorf("root blobs\n got: %q\nwant: %q", got, tt.wantRoot)
			}
			ua := run.Request.GetAction().GetUserMessageAction()
			if ua == nil {
				t.Fatalf("action = %T, want a user message action", run.Request.GetAction().GetAction())
			}
			if got := ua.GetUserMessage().GetText(); got != tt.wantAction {
				t.Errorf("action text = %q, want %q", got, tt.wantAction)
			}
			if ua.GetUserMessage().GetMode() != agentv1.AgentMode_AGENT_MODE_AGENT {
				t.Errorf("action mode = %v, want AGENT", ua.GetUserMessage().GetMode())
			}
			var rules []string
			for _, r := range run.RequestContext.GetRules() {
				rules = append(rules, r.GetContent())
				if r.GetSource() != agentv1.CursorRuleSource_CURSOR_RULE_SOURCE_USER || r.GetType().GetGlobal() == nil {
					t.Errorf("rule %q must be a global rule from source USER", r.GetFullPath())
				}
			}
			if !equalStrings(rules, tt.wantRules) {
				t.Errorf("rules = %q, want %q", rules, tt.wantRules)
			}
			if len(run.Resolves) != 0 {
				t.Errorf("a request ending on a user message resolves no tool call, got %d", len(run.Resolves))
			}
			if len(run.Request.ConversationState.Turns) != 0 {
				t.Errorf("turns are only needed to resume, got %d", len(run.Request.ConversationState.Turns))
			}
		})
	}
}

func TestTranslateTrailingToolResults(t *testing.T) {
	run := translateOK(t, `{"model":"composer-2.5","messages":[
		{"role":"system","content":"Be terse."},
		{"role":"user","content":"weather in Paris and Tokyo?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call|a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
			{"id":"tool_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}]},
		{"role":"tool","tool_call_id":"tool_b","content":"Rainy, 12C"},
		{"role":"tool","tool_call_id":"call|a","content":"Sunny, 21C"}]}`)

	if _, ok := run.Request.GetAction().GetAction().(*agentv1.ConversationAction_ResumeAction); !ok {
		t.Fatalf("action = %T, want a resume action", run.Request.GetAction().GetAction())
	}
	// Everything, results included, is in the root: the stateless replay.
	wantRoot := []string{
		`{"role":"system","content":"Be terse."}`,
		`{"role":"user","content":"<rules>\nBe terse.\n</rules>"}`,
		`{"role":"user","content":[{"type":"text","text":"weather in Paris and Tokyo?"}]}`,
		`{"role":"assistant","content":[` +
			`{"type":"tool-call","toolCallId":"call_a","toolName":"CallDynamicTool","args":{"namespace":"client","toolName":"get_weather","arguments":{"city":"Paris"}}},` +
			`{"type":"tool-call","toolCallId":"tool_b","toolName":"CallDynamicTool","args":{"namespace":"client","toolName":"get_weather","arguments":{"city":"Tokyo"}}}]}`,
		`{"role":"tool","content":[{"type":"tool-result","toolCallId":"tool_b","toolName":"CallDynamicTool","result":"Rainy, 12C","experimental_content":[{"type":"text","text":"Rainy, 12C"}]}],"id":"tool_b"}`,
		`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_a","toolName":"CallDynamicTool","result":"Sunny, 21C","experimental_content":[{"type":"text","text":"Sunny, 21C"}]}],"id":"call_a"}`,
	}
	if got := root(t, run); !equalStrings(got, wantRoot) {
		t.Errorf("root blobs\n got: %q\nwant: %q", got, wantRoot)
	}

	// The run resolves the calls with the ids the client holds, in message order.
	if len(run.Resolves) != 2 || run.Resolves[0].CallID != "tool_b" || run.Resolves[1].CallID != "call|a" {
		t.Fatalf("Resolves = %+v, want tool_b then call|a", run.Resolves)
	}
	got := run.Resolves[1].Result.GetSuccess().GetContent()
	if len(got) != 1 || got[0].GetText().GetText() != "Sunny, 21C" || run.Resolves[1].Result.GetSuccess().GetIsError() {
		t.Errorf("result for call|a = %v, want one text item \"Sunny, 21C\"", got)
	}

	// One turn: the user message and the steps, the calls carrying their results. The server only
	// resumes a turn it can see.
	turns := run.Request.ConversationState.Turns
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	var turn agentv1.ConversationTurnStructure
	if err := proto.Unmarshal(blob(t, run, turns[0]), &turn); err != nil {
		t.Fatal(err)
	}
	agentTurn := turn.GetAgentConversationTurn()
	var user agentv1.UserMessage
	if err := proto.Unmarshal(blob(t, run, agentTurn.GetUserMessage()), &user); err != nil {
		t.Fatal(err)
	}
	if user.GetText() != "weather in Paris and Tokyo?" {
		t.Errorf("turn user message = %q", user.GetText())
	}
	if len(agentTurn.GetSteps()) != 2 {
		t.Fatalf("steps = %d, want 2 tool calls", len(agentTurn.GetSteps()))
	}
	for i, want := range []struct{ id, tool, result string }{{"call_a", "get_weather", "Sunny, 21C"}, {"tool_b", "get_weather", "Rainy, 12C"}} {
		var step agentv1.ConversationStep
		if err := proto.Unmarshal(blob(t, run, agentTurn.GetSteps()[i]), &step); err != nil {
			t.Fatal(err)
		}
		call := step.GetToolCall().GetMcpToolCall()
		if step.GetToolCall().GetToolCallId() != want.id || call.GetArgs().GetToolCallId() != want.id ||
			call.GetArgs().GetToolName() != want.tool || call.GetArgs().GetProviderIdentifier() != "client" ||
			call.GetResult().GetSuccess().GetContent()[0].GetText().GetText() != want.result {
			t.Errorf("step %d = %v, want call %s with result %q", i, &step, want.id, want.result)
		}
	}
}

func TestTranslateImages(t *testing.T) {
	png := []byte("\x89PNG fake")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	body := `{"model":"composer-2.5","messages":[
		{"role":"user","content":[{"type":"text","text":"old"},{"type":"image_url","image_url":{"url":"` + dataURL + `"}}]},
		{"role":"assistant","content":"seen"},
		{"role":"user","content":[{"type":"text","text":"and this?"},{"type":"image_url","image_url":{"url":"https://example.test/cat.jpg"}}]}]}`

	if _, err := translate.Translate(decode(t, body), translate.Options{Catalog: plainCatalog}); err == nil || !strings.Contains(err.Error(), "no resolver") {
		t.Fatalf("an http image without a resolver must fail, got %v", err)
	}

	var asked []string
	run, err := translate.Translate(decode(t, body), translate.Options{
		Catalog: plainCatalog,
		ResolveImage: func(u string) (string, []byte, error) {
			asked = append(asked, u)
			return "image/jpeg", []byte("JPEG"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "https://example.test/cat.jpg" {
		t.Errorf("resolver asked for %v, want only the http URL", asked)
	}
	wantHistory := `{"role":"user","content":[{"type":"text","text":"old"},{"type":"image","image":"` + dataURL + `","mediaType":"image/png"}]}`
	if got := root(t, run); len(got) != 2 || got[0] != wantHistory {
		t.Errorf("history image blob\n got: %q\nwant: %q first", got, wantHistory)
	}
	imgs := run.Request.GetAction().GetUserMessageAction().GetUserMessage().GetSelectedContext().GetSelectedImages()
	if len(imgs) != 1 || imgs[0].GetMimeType() != "image/jpeg" || string(imgs[0].GetData()) != "JPEG" || imgs[0].GetUuid() == "" {
		t.Errorf("action images = %v, want the resolved JPEG as inline data", imgs)
	}
}

func TestTranslateRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
		wantIs  error
	}{
		{"no messages", `{"model":"composer-2.5","messages":[]}`, "", translate.ErrNoActiveMessage},
		{"only a system message", `{"model":"composer-2.5","messages":[{"role":"system","content":"x"}]}`, "", translate.ErrNoActiveMessage},
		{"ends on an assistant message", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`, "", translate.ErrNoActiveMessage},
		{"ends on an empty user message", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":""}]}`, "", translate.ErrNoActiveMessage},
		{"tool result without a call", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},{"role":"tool","tool_call_id":"x","content":"r"}]}`, "unknown or already answered call", nil},
		{"tool result answered twice", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},
			{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"x","content":"r"},{"role":"tool","tool_call_id":"x","content":"r"}]}`, "unknown or already answered call", nil},
		{"call without a result before the next message", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},
			{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"user","content":"b"}]}`, "have no result", nil},
		{"result that comes after another message", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},
			{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"user","content":"b"},{"role":"tool","tool_call_id":"x","content":"r"}]}`, "x have no result", nil},
		{"call without a result at the end", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},
			{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"y","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"x","content":"r"}]}`, "y have no result", nil},
		{"arguments that are not an object", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},
			{"role":"assistant","tool_calls":[{"id":"x","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`, "not a JSON object", nil},
		{"call ids that collide once sanitized", `{"model":"composer-2.5","messages":[{"role":"user","content":"a"},
			{"role":"assistant","tool_calls":[{"id":"a|b","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"a.b","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, "collides", nil},
		{"unsupported role", `{"model":"composer-2.5","messages":[{"role":"function","content":"a"}]}`, "unsupported role", nil},
		{"unsupported content part", `{"model":"composer-2.5","messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`, "unsupported content part", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translate.Translate(decode(t, tt.body), translate.Options{Catalog: plainCatalog})
			if err == nil {
				t.Fatal("want an error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestContentDecoding(t *testing.T) {
	tests := []struct {
		body      string
		wantTexts []string
	}{
		{`{"role":"user","content":"plain"}`, []string{"plain"}},
		{`{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, []string{"a", "b"}},
		{`{"role":"assistant","content":null}`, nil},
	}
	for _, tt := range tests {
		var m translate.Message
		if err := json.Unmarshal([]byte(tt.body), &m); err != nil {
			t.Fatalf("%s: %v", tt.body, err)
		}
		var texts []string
		for _, p := range m.Content.Parts {
			texts = append(texts, p.Text)
		}
		if !equalStrings(texts, tt.wantTexts) {
			t.Errorf("%s: texts = %q, want %q", tt.body, texts, tt.wantTexts)
		}
	}
	var m translate.Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":3}`), &m); err == nil {
		t.Error("a numeric content must be rejected")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

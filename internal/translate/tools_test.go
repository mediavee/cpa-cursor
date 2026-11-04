package translate_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

func TestToolDefinitions(t *testing.T) {
	run := translateOK(t, `{"model":"composer-2.5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"type":"function","function":{"name":"get_weather","description":"Get the weather.",
				"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},
			{"type":"function","function":{"name":"ping"}}]}`)

	for name, tools := range map[string][]string{
		"mcp_tools":               toolJSON(t, run.Request.GetMcpTools().GetMcpTools()),
		"request_context.tools[]": toolJSON(t, run.RequestContext.GetTools()),
	} {
		want := []string{
			`{"name":"client-get_weather","description":"Get the weather.","inputSchema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]},"providerIdentifier":"client","toolName":"get_weather"}`,
			`{"name":"client-ping","inputSchema":{"type":"object","properties":{}},"providerIdentifier":"client","toolName":"ping"}`,
		}
		if len(tools) != 2 {
			t.Fatalf("%s: %d tools, want 2", name, len(tools))
		}
		for i := range want {
			if !jsonEqual(t, []byte(tools[i]), want[i]) {
				t.Errorf("%s[%d]\n got: %s\nwant: %s", name, i, tools[i], want[i])
			}
		}
	}
}

func toolJSON(t *testing.T, defs []*agentv1.McpToolDefinition) []string {
	t.Helper()
	var out []string
	for _, d := range defs {
		b, err := protojson.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func TestToolChoiceNone(t *testing.T) {
	tools := `"tools":[{"type":"function","function":{"name":"f"}}]`
	run := translateOK(t, `{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],`+tools+`,"tool_choice":"none"}`)
	if run.Request.GetMcpTools() != nil || len(run.RequestContext.GetTools()) != 0 {
		t.Error(`tool_choice "none" must declare no tool`)
	}
	run = translateOK(t, `{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],`+tools+`,"tool_choice":"auto"}`)
	if len(run.Request.GetMcpTools().GetMcpTools()) != 1 {
		t.Error(`tool_choice "auto" must keep the tools`)
	}
	run = translateOK(t, `{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`)
	if run.Request.GetMcpTools() != nil {
		t.Error("a request without tools must not carry an mcp_tools message")
	}
}

func TestToolErrors(t *testing.T) {
	tests := []struct{ name, tools, want string }{
		{"duplicate name", `[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"f"}}]`, "duplicate"},
		{"missing name", `[{"type":"function","function":{}}]`, "name is required"},
		{"not a function", `[{"type":"custom","function":{"name":"f"}}]`, "unsupported tool type"},
		{"schema that is not an object", `[{"type":"function","function":{"name":"f","parameters":[1]}}]`, "JSON Schema object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := decode(t, `{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"tools":`+tt.tools+`}`)
			_, err := translate.Translate(req, translate.Options{Catalog: plainCatalog})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

package translate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// providerIdentifier is the MCP server name client tools are declared under.
const providerIdentifier = "client"

// emptySchema is the input schema of a tool declared without parameters.
const emptySchema = `{"type":"object","properties":{}}`

// toolDefinitions maps the declared Chat Completions tools to MCP tool definitions. The definition's
// name is "client-<function name>" and tool_name is the function name verbatim, so a call coming back
// as McpArgs maps to the client's name through tool_name (see CallName).
func toolDefinitions(tools []Tool) ([]*agentv1.McpToolDefinition, error) {
	defs := make([]*agentv1.McpToolDefinition, 0, len(tools))
	seen := make(map[string]bool, len(tools))
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("tools[%d]: unsupported tool type %q", i, t.Type)
		}
		name := t.Function.Name
		if name == "" {
			return nil, fmt.Errorf("tools[%d]: function name is required", i)
		}
		if seen[name] {
			return nil, fmt.Errorf("tools[%d]: duplicate function name %q", i, name)
		}
		seen[name] = true

		raw := t.Function.Parameters
		if len(raw) == 0 || string(raw) == "null" {
			raw = json.RawMessage(emptySchema)
		}
		var schema any
		if err := json.Unmarshal(raw, &schema); err != nil {
			return nil, fmt.Errorf("tools[%d]: parameters are not valid JSON: %w", i, err)
		}
		if _, ok := schema.(map[string]any); !ok {
			return nil, fmt.Errorf("tools[%d]: parameters must be a JSON Schema object", i)
		}
		value, err := structpb.NewValue(schema)
		if err != nil {
			return nil, fmt.Errorf("tools[%d]: parameters: %w", i, err)
		}
		defs = append(defs, &agentv1.McpToolDefinition{
			Name:               providerIdentifier + "-" + name,
			ProviderIdentifier: providerIdentifier,
			ToolName:           name,
			Description:        t.Function.Description,
			InputSchema:        value,
		})
	}
	return defs, nil
}

// toolsDisabled reports whether tool_choice is "none": the protocol has no such field, so the
// tools are not declared at all.
func toolsDisabled(choice json.RawMessage) bool {
	var s string
	return json.Unmarshal(choice, &s) == nil && s == "none"
}

// CallName returns the client's function name for a tool call Cursor made.
func CallName(args *agentv1.McpArgs) string {
	if args.GetToolName() != "" {
		return args.GetToolName()
	}
	return strings.TrimPrefix(args.GetName(), providerIdentifier+"-")
}

// callArguments renders the arguments of a tool call as the JSON object string Chat Completions uses.
func callArguments(args *agentv1.McpArgs) (string, error) {
	obj := make(map[string]any, len(args.GetArgs()))
	for k, v := range args.GetArgs() {
		obj[k] = v.AsInterface()
	}
	b, err := marshalJSON(obj)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

var invalidIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// wireCallID makes a client tool call id acceptable to Cursor, which rejects ids outside
// [a-zA-Z0-9_-] (observed by third-party clients as an opaque resource_exhausted). The mapping is
// deterministic, so a call and its result stay paired.
func wireCallID(id string) string {
	return invalidIDChars.ReplaceAllString(id, "_")
}

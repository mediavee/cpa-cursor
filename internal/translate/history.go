package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// Root prompt blobs
//
// ConversationStateStructure.root_prompt_messages_json is a list of blob ids; each blob is one JSON
// message in the AI SDK message shape Cursor's server uses. The server fetches them through the KV
// channel (get_blob), keyed by sha256 of the bytes.
//
// System and user messages:
//
//	{"role":"system","content":"<text>"}
//	{"role":"user","content":[{"type":"text","text":"<text>"},
//	                          {"type":"image","image":"data:<mime>;base64,<b64>","mediaType":"<mime>"}]}
//	{"role":"user","content":"<rules>\n<system prompt>\n</rules>"}
//
// Proof (cmd/translate-live -scenario memory, composer-2.5, 2026-11-04): over three requests built
// only from the client's messages, the model recalls a word given in the first, and a system prompt
// ("end every reply with OVER") is followed both on the history-less first request (rules from
// request_context) and on the later ones (rules entry above). Without any system message the root
// simply has no system blob: the server accepts it.
//
// Client tool calls and results. The server wraps every MCP tool in a generic "CallDynamicTool" tool,
// and wrote this (captured from the set_blob messages of a live run in which the model called a
// client tool get_weather{city} and the client answered "Sunny, 21C"):
//
//	{"role":"assistant","content":[
//	  {"type":"tool-call","toolCallId":"tool_0c59615a-...","toolName":"CallDynamicTool",
//	   "args":{"namespace":"client","toolName":"get_weather","arguments":{"city":"Paris"}}}]}
//	{"role":"tool","content":[
//	  {"type":"tool-result","toolCallId":"tool_0c59615a-...","toolName":"CallDynamicTool",
//	   "result":"Sunny, 21C","experimental_content":[{"type":"text","text":"Sunny, 21C"}]}],
//	 "id":"tool_0c59615a-..."}
//
// (the server adds redacted-reasoning parts, an "id" on assistant blobs and providerOptions, which a
// client cannot reproduce and the model does not need). Proof (cmd/translate-live -scenario tool and
// -scenario parallel): a second request holding only the client's messages, with the call and the
// result written in this form, made the model answer from the result. An assistant message with
// several tool calls is one blob with several tool-call parts, followed by one tool blob per result,
// in the order the tool messages arrived (the parallel scenario answers them in reverse).

const dynamicTool = "CallDynamicTool"

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imagePart struct {
	Type      string `json:"type"`
	Image     string `json:"image"`
	MediaType string `json:"mediaType"`
}

type toolCallPart struct {
	Type       string      `json:"type"`
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Args       dynamicArgs `json:"args"`
}

type dynamicArgs struct {
	Namespace string          `json:"namespace"`
	ToolName  string          `json:"toolName"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolResultPart struct {
	Type    string     `json:"type"`
	CallID  string     `json:"toolCallId"`
	Name    string     `json:"toolName"`
	Result  string     `json:"result"`
	Content []textPart `json:"experimental_content"`
}

type rootMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	ID      string `json:"id,omitempty"`
}

// rootEntries returns the ids of the root prompt blobs: system entries, the rules entry when there
// is history, then the history.
func (c *conversation) rootEntries(blobs *blobSet) ([][]byte, error) {
	var ids [][]byte
	add := func(m rootMessage) error {
		id, err := blobs.addJSON(m)
		if err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	}
	for _, s := range c.system {
		if err := add(rootMessage{Role: "system", Content: s}); err != nil {
			return nil, err
		}
	}
	// With history, the server keeps the root verbatim and does not rebuild the rules entry it builds
	// from request_context.rules on a history-less root, so the client carries it.
	if len(c.history) > 0 && len(c.system) > 0 {
		if err := add(rootMessage{Role: "user", Content: rulesEntry(c.system)}); err != nil {
			return nil, err
		}
	}
	for _, it := range c.history {
		var m rootMessage
		switch it.kind {
		case itemUser:
			m = rootMessage{Role: "user", Content: userParts(it.user)}
		case itemAssistant:
			parts := make([]any, 0, 1+len(it.calls))
			if it.text != "" {
				parts = append(parts, textPart{Type: "text", Text: it.text})
			}
			for _, tc := range it.calls {
				parts = append(parts, toolCallPart{
					Type: "tool-call", ToolCallID: tc.id, ToolName: dynamicTool,
					Args: dynamicArgs{Namespace: providerIdentifier, ToolName: tc.name, Arguments: tc.args},
				})
			}
			m = rootMessage{Role: "assistant", Content: parts}
		case itemToolResult:
			m = rootMessage{Role: "tool", ID: it.callID, Content: []toolResultPart{{
				Type: "tool-result", CallID: it.callID, Name: dynamicTool, Result: it.result,
				Content: []textPart{{Type: "text", Text: it.result}},
			}}}
		}
		if err := add(m); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func rulesEntry(system []string) string {
	return "<rules>\n" + strings.Join(system, "\n\n") + "\n</rules>"
}

func userParts(u userMessage) []any {
	parts := make([]any, 0, len(u.parts))
	for _, p := range u.parts {
		if p.image == nil {
			parts = append(parts, textPart{Type: "text", Text: p.text})
			continue
		}
		parts = append(parts, imagePart{
			Type: "image", MediaType: p.image.mime,
			Image: "data:" + p.image.mime + ";base64," + base64.StdEncoding.EncodeToString(p.image.data),
		})
	}
	return parts
}

// Turns
//
// ConversationStateStructure.turns lists the conversation as the server's own checkpoints do: one
// ConversationTurnStructure blob per user message, holding the id of a UserMessage blob and the ids
// of ConversationStep blobs (assistant text, tool calls with their results). A request that ends on
// tool results has no user message to put in the action, so it sends a ResumeAction over the full
// history. Proof (live, composer-2.5): with the complete root but no turns, the resume ended at once
// with an empty turn_ended (0.3 s, no output); an empty user message action instead made the model
// answer "Nothing new in your last message" and ignore the result; with turns, the model continued
// from the tool result (cmd/translate-live -scenario tool).

// turnEntries builds the turn blobs for the whole history.
func (c *conversation) turnEntries(blobs *blobSet) ([][]byte, error) {
	results := map[string]string{}
	for _, it := range c.history {
		if it.kind == itemToolResult {
			results[it.callID] = it.result
		}
	}
	var turns [][]byte
	var user []byte
	var steps [][]byte
	flush := func() error {
		if user == nil {
			return nil
		}
		turn := &agentv1.ConversationTurnStructure{Turn: &agentv1.ConversationTurnStructure_AgentConversationTurn{
			AgentConversationTurn: &agentv1.AgentConversationTurnStructure{UserMessage: user, Steps: steps},
		}}
		b, err := proto.Marshal(turn)
		if err != nil {
			return err
		}
		turns = append(turns, blobs.add(b))
		user, steps = nil, nil
		return nil
	}
	addStep := func(s *agentv1.ConversationStep) error {
		b, err := proto.Marshal(s)
		if err != nil {
			return err
		}
		steps = append(steps, blobs.add(b))
		return nil
	}
	for i, it := range c.history {
		switch it.kind {
		case itemUser:
			if err := flush(); err != nil {
				return nil, err
			}
			b, err := proto.Marshal(newUserMessage(it.user, i))
			if err != nil {
				return nil, err
			}
			user = blobs.add(b)
		case itemAssistant:
			if user == nil {
				continue // the root carries it; a turn needs a user message
			}
			if it.text != "" {
				step := &agentv1.ConversationStep{Message: &agentv1.ConversationStep_AssistantMessage{
					AssistantMessage: &agentv1.AssistantMessage{Text: it.text},
				}}
				if err := addStep(step); err != nil {
					return nil, err
				}
			}
			for _, tc := range it.calls {
				step, err := toolCallStep(tc, results[tc.id])
				if err != nil {
					return nil, err
				}
				if err := addStep(step); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return turns, nil
}

func toolCallStep(tc toolCall, result string) (*agentv1.ConversationStep, error) {
	var obj map[string]any
	if err := json.Unmarshal(tc.args, &obj); err != nil {
		return nil, err
	}
	args := make(map[string]*structpb.Value, len(obj))
	for k, v := range obj {
		pv, err := structpb.NewValue(v)
		if err != nil {
			return nil, err
		}
		args[k] = pv
	}
	return &agentv1.ConversationStep{Message: &agentv1.ConversationStep_ToolCall{ToolCall: &agentv1.ToolCall{
		ToolCallId: proto.String(tc.id),
		Tool: &agentv1.ToolCall_McpToolCall{McpToolCall: &agentv1.McpToolCall{
			Args: &agentv1.McpArgs{
				Name: providerIdentifier + "-" + tc.name, Args: args, ToolCallId: tc.id,
				ProviderIdentifier: providerIdentifier, ToolName: tc.name,
			},
			Result: &agentv1.McpToolResult{Result: &agentv1.McpToolResult_Success{Success: textSuccess(result)}},
		}},
	}}}, nil
}

func textSuccess(text string) *agentv1.McpSuccess {
	return &agentv1.McpSuccess{Content: []*agentv1.McpToolResultContentItem{{
		Content: &agentv1.McpToolResultContentItem_Text{Text: &agentv1.McpTextContent{Text: text}},
	}}}
}

// newUserMessage builds the UserMessage of the action or of a turn. The id is derived from the
// content and position so the same request always yields the same bytes.
func newUserMessage(u userMessage, position int) *agentv1.UserMessage {
	h := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%d:%s", position, u.text())))
	m := &agentv1.UserMessage{
		Text:      u.text(),
		MessageId: h.String(),
		Mode:      agentv1.AgentMode_AGENT_MODE_AGENT,
	}
	if imgs := u.images(); len(imgs) > 0 {
		m.SelectedContext = &agentv1.SelectedContext{}
		for _, img := range imgs {
			m.SelectedContext.SelectedImages = append(m.SelectedContext.SelectedImages, &agentv1.SelectedImage{
				Uuid:         uuid.NewSHA1(h, img.data).String(),
				MimeType:     img.mime,
				DataOrBlobId: &agentv1.SelectedImage_Data{Data: img.data},
			})
		}
	}
	return m
}

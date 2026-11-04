package translate

import (
	"fmt"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// Options configures Translate.
type Options struct {
	// Catalog resolves the requested slug; it is required.
	Catalog Catalog
	// ConversationID is sent as conversation_id; a random one when empty.
	ConversationID string
	// ResolveImage fetches image URLs that are not data: URLs; such URLs are an error when nil.
	ResolveImage ImageResolver
}

// Run is everything a Cursor run needs for one Chat Completions request.
type Run struct {
	// Request is the first message of the run. Its conversation state references Blobs by id.
	Request *agentv1.AgentRunRequest
	// Blobs is what the KV channel serves for the ids in Request.
	Blobs []Blob
	// RequestContext is the answer to the server's request_context_args exec: the rules and the
	// client tools.
	RequestContext *agentv1.RequestContext
	// Resolves lists the tool calls the request's trailing tool messages answer, in order, with the
	// call ids the client sent (the ids this package returned as tool_calls). It is empty unless the
	// request ends with tool results. A caller holding a parked run for these calls answers them with
	// the results instead of starting Request; Request replays the same conversation without state.
	Resolves []ToolResult
}

// ToolResult is the client's answer to one tool call.
type ToolResult struct {
	CallID string
	Result *agentv1.McpResult
}

// Translate converts a Chat Completions request into a Cursor run.
func Translate(req *ChatRequest, opts Options) (*Run, error) {
	conv, err := parseConversation(req.Messages, opts.ResolveImage)
	if err != nil {
		return nil, err
	}
	model, err := resolveModel(opts.Catalog, req.Model, req.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	var tools []*agentv1.McpToolDefinition
	if !toolsDisabled(req.ToolChoice) {
		if tools, err = toolDefinitions(req.Tools); err != nil {
			return nil, err
		}
	}

	blobs := newBlobSet()
	root, err := conv.rootEntries(blobs)
	if err != nil {
		return nil, err
	}
	state := &agentv1.ConversationStateStructure{RootPromptMessagesJson: root}

	var action *agentv1.ConversationAction
	if conv.active != nil {
		action = &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{
			UserMessageAction: &agentv1.UserMessageAction{UserMessage: newUserMessage(*conv.active, len(conv.history))},
		}}
	} else {
		if state.Turns, err = conv.turnEntries(blobs); err != nil {
			return nil, err
		}
		action = &agentv1.ConversationAction{Action: &agentv1.ConversationAction_ResumeAction{
			ResumeAction: &agentv1.ResumeAction{},
		}}
	}

	convID := opts.ConversationID
	if convID == "" {
		convID = uuid.NewString()
	}
	run := &Run{
		Request: &agentv1.AgentRunRequest{
			ConversationState: state,
			Action:            action,
			ModelDetails:      model.details,
			RequestedModel:    model.requested,
			ConversationId:    proto.String(convID),
		},
		RequestContext: &agentv1.RequestContext{
			Rules: cursorRules(conv.system),
			Tools: tools,
			Env:   &agentv1.RequestContextEnv{},
		},
	}
	if len(tools) > 0 {
		run.Request.McpTools = &agentv1.McpTools{McpTools: tools}
	}
	for _, r := range conv.resolves {
		run.Resolves = append(run.Resolves, ToolResult{
			CallID: r.callID,
			Result: &agentv1.McpResult{Result: &agentv1.McpResult_Success{Success: textSuccess(r.text)}},
		})
	}
	run.Blobs = blobs.blobs
	return run, nil
}

// cursorRules turns the system prompt entries into global rules, which the server follows on a
// history-less root (see rootEntries for the history case).
func cursorRules(system []string) []*agentv1.CursorRule {
	rules := make([]*agentv1.CursorRule, 0, len(system))
	for i, s := range system {
		rules = append(rules, &agentv1.CursorRule{
			FullPath: fmt.Sprintf("/client/system-prompt/%d.mdc", i),
			Content:  s,
			Source:   agentv1.CursorRuleSource_CURSOR_RULE_SOURCE_USER,
			Type:     &agentv1.CursorRuleType{Type: &agentv1.CursorRuleType_Global{Global: &agentv1.CursorRuleTypeGlobal{}}},
		})
	}
	return rules
}

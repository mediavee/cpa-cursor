package translate

import (
	"strings"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

// FinishReason is the finish_reason of the last chunk.
type FinishReason string

const (
	// FinishStop is a turn that ended normally.
	FinishStop FinishReason = "stop"
	// FinishToolCalls is a response that ends on tool calls the client must run.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishLength is a turn Cursor cut short on a length error (ERROR_MAX_TOKENS and the like).
	FinishLength FinishReason = "length"
)

// Encoder turns what a Cursor run emits into Chat Completions output. Feed it the run's messages in
// order and send the chunks it returns; Response gives the same content as one non-streaming
// response. An Encoder is used by one goroutine.
type Encoder struct {
	id, model string
	created   int64

	started   bool
	content   strings.Builder
	reasoning strings.Builder
	calls     []ToolCall
	usage     *Usage
	finish    FinishReason
}

// NewEncoder returns an encoder for a response with the given id, model and creation time (Unix seconds).
func NewEncoder(id, model string, created int64) *Encoder {
	return &Encoder{id: id, model: model, created: created}
}

// Update handles an InteractionUpdate: text and reasoning deltas become chunks, turn_ended records the
// usage reported with the last chunk, everything else is ignored. Server notices (text the server
// writes to the client rather than the model) are not part of the answer and are dropped.
func (e *Encoder) Update(u *agentv1.InteractionUpdate) []ChatChunk {
	switch m := u.GetMessage().(type) {
	case *agentv1.InteractionUpdate_TextDelta:
		if m.TextDelta.GetIsServerNotice() || m.TextDelta.GetText() == "" {
			return nil
		}
		e.content.WriteString(m.TextDelta.Text)
		return []ChatChunk{e.chunk(ChunkDelta{Content: m.TextDelta.Text}, nil)}
	case *agentv1.InteractionUpdate_ThinkingDelta:
		if m.ThinkingDelta.GetText() == "" {
			return nil
		}
		e.reasoning.WriteString(m.ThinkingDelta.Text)
		return []ChatChunk{e.chunk(ChunkDelta{ReasoningContent: m.ThinkingDelta.Text}, nil)}
	case *agentv1.InteractionUpdate_TurnEnded:
		e.usage = usageOf(m.TurnEnded)
	}
	return nil
}

// ToolCall handles a tool call Cursor made. The call id is Cursor's tool_call_id, which the client
// echoes back on its tool message.
func (e *Encoder) ToolCall(args *agentv1.McpArgs) (ChatChunk, error) {
	arguments, err := callArguments(args)
	if err != nil {
		return ChatChunk{}, err
	}
	call := ToolCall{ID: args.GetToolCallId(), Type: "function", Function: ToolCallFunction{Name: CallName(args), Arguments: arguments}}
	index := len(e.calls)
	e.calls = append(e.calls, call)
	return e.chunk(ChunkDelta{ToolCalls: []ChunkToolCall{{Index: index, ID: call.ID, Type: call.Type, Function: call.Function}}}, nil), nil
}

// Finish returns the last chunk: the finish reason and, when the turn reported it, the usage.
func (e *Encoder) Finish(reason FinishReason) ChatChunk {
	e.finish = reason
	r := string(reason)
	c := e.chunk(ChunkDelta{}, &r)
	c.Usage = e.usage
	return c
}

// Response returns the response built from everything fed so far. Its finish reason is the one given
// to Finish, or "stop" when Finish was not called.
func (e *Encoder) Response() ChatResponse {
	msg := ResponseMessage{Role: "assistant", ReasoningContent: e.reasoning.String(), ToolCalls: e.calls}
	if e.content.Len() > 0 || len(e.calls) == 0 {
		s := e.content.String()
		msg.Content = &s
	}
	finish := e.finish
	if finish == "" {
		finish = FinishStop
	}
	return ChatResponse{
		ID: e.id, Object: "chat.completion", Created: e.created, Model: e.model,
		Choices: []ResponseChoice{{Message: msg, FinishReason: string(finish)}},
		Usage:   e.usage,
	}
}

func (e *Encoder) chunk(d ChunkDelta, finish *string) ChatChunk {
	if !e.started {
		e.started = true
		d.Role = "assistant"
	}
	return ChatChunk{
		ID: e.id, Object: "chat.completion.chunk", Created: e.created, Model: e.model,
		Choices: []ChunkChoice{{Delta: d, FinishReason: finish}},
	}
}

// usageOf maps Cursor's token counts to OpenAI's: the prompt counts the cached tokens, which Cursor
// reports apart from the input.
func usageOf(t *agentv1.TurnEndedUpdate) *Usage {
	prompt := t.GetInputTokens() + t.GetCacheReadTokens()
	return &Usage{
		PromptTokens:            prompt,
		CompletionTokens:        t.GetOutputTokens(),
		TotalTokens:             prompt + t.GetOutputTokens(),
		PromptTokensDetails:     &PromptTokensDetails{CachedTokens: t.GetCacheReadTokens()},
		CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: t.GetReasoningTokens()},
	}
}

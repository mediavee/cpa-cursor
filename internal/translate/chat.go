// Package translate converts OpenAI Chat Completions requests into Cursor agent runs and Cursor
// interaction updates back into Chat Completions responses. It does no I/O.
package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ChatRequest is the subset of an OpenAI Chat Completions request a Cursor run can express.
// Fields the agent protocol has no equivalent for (temperature, max_tokens, ...) are not modelled.
type ChatRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
}

// Message is one Chat Completions message.
type Message struct {
	Role string `json:"role"`
	// Content is a string or an array of parts. It may be null on an assistant message with tool calls.
	Content Content `json:"content"`
	// ToolCalls are the calls an assistant message made.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is the call a tool message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ReasoningContent is the reasoning an assistant message carried; it is not replayed to Cursor.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Content is a message body: either a plain string or an array of typed parts.
type Content struct {
	Parts []ContentPart
}

// ContentPart is a text or image part. Type is "text" or "image_url".
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is the target of an image part: a data: URL or an http(s) URL.
type ImageURL struct {
	URL string `json:"url"`
}

// TextContent returns a Content made of one text part.
func TextContent(text string) Content {
	return Content{Parts: []ContentPart{{Type: "text", Text: text}}}
}

// UnmarshalJSON accepts a string, an array of parts, or null.
func (c *Content) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case len(data) == 0, bytes.Equal(data, []byte("null")):
		c.Parts = nil
		return nil
	case data[0] == '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		c.Parts = []ContentPart{{Type: "text", Text: s}}
		return nil
	case data[0] == '[':
		return json.Unmarshal(data, &c.Parts)
	}
	return fmt.Errorf("message content must be a string, an array or null")
}

// MarshalJSON writes a single text part as a string and anything else as an array.
func (c Content) MarshalJSON() ([]byte, error) {
	switch {
	case len(c.Parts) == 0:
		return []byte("null"), nil
	case len(c.Parts) == 1 && c.Parts[0].Type == "text":
		return json.Marshal(c.Parts[0].Text)
	}
	return json.Marshal(c.Parts)
}

// Tool is a client tool declaration. Only function tools exist in Chat Completions.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function a Tool declares; Parameters is a JSON Schema.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is a call an assistant message made.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the called tool name and its arguments as a JSON object encoded in a string.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatResponse is a complete (non-streaming) Chat Completions response.
type ChatResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []ResponseChoice `json:"choices"`
	Usage   *Usage           `json:"usage,omitempty"`
}

// ResponseChoice is the single choice of a response.
type ResponseChoice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// ResponseMessage is the assistant message of a response.
type ResponseMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// ChatChunk is one streaming chunk (chat.completion.chunk).
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is the single choice of a chunk; FinishReason is nil until the last chunk.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChunkDelta is the incremental part of a message.
type ChunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ChunkToolCall `json:"tool_calls,omitempty"`
}

// ChunkToolCall is a tool call inside a delta. Index orders the calls of one message.
type ChunkToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// Usage is the token accounting of a response.
type Usage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails breaks the prompt tokens down.
type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// CompletionTokensDetails breaks the completion tokens down.
type CompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

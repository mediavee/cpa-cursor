package plugin

import (
	"encoding/json"

	"github.com/mediavee/cpa-cursor/internal/translate"
)

const (
	bytesPerToken     = 4
	tokensPerMessage  = 4
	tokensPerImage    = 1000
	tokensPerToolCall = 8
)

// estimateTokens approximates the size of a prompt for count_tokens: about four bytes per token over
// the text of the messages, the tool calls and the tool definitions, plus a fixed cost per message
// and per image. Cursor has no counting endpoint. The figure covers the client's own content only:
// Cursor adds its agent prompt (roughly 11k tokens) to every run, so billed input tokens are higher.
func estimateTokens(req *translate.ChatRequest) int64 {
	var bytes, extra int64
	for _, m := range req.Messages {
		extra += tokensPerMessage
		for _, part := range m.Content.Parts {
			if part.Type == "image_url" {
				extra += tokensPerImage
				continue
			}
			bytes += int64(len(part.Text))
		}
		for _, call := range m.ToolCalls {
			extra += tokensPerToolCall
			bytes += int64(len(call.Function.Name) + len(call.Function.Arguments))
		}
	}
	for _, t := range req.Tools {
		b, err := json.Marshal(t)
		if err == nil {
			bytes += int64(len(b))
		}
	}
	return (bytes+bytesPerToken-1)/bytesPerToken + extra
}

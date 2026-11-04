package translate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ErrNoActiveMessage reports a request that ends with neither a user message nor tool results:
// there is nothing for the model to answer.
var ErrNoActiveMessage = errors.New("the last message must be a user message or a tool result")

// ImageResolver turns an image URL that is not a data: URL into its media type and bytes. The
// translation does no I/O, so the caller supplies it when requests may carry http(s) images.
type ImageResolver func(rawURL string) (mime string, data []byte, err error)

type image struct {
	mime string
	data []byte
}

// userPart is a text or an image; exactly one is set.
type userPart struct {
	text  string
	image *image
}

// userMessage keeps its parts in the order the client sent them. The action cannot: the protocol
// splits it into text and selected images (see text and images).
type userMessage struct {
	parts []userPart
}

func (u userMessage) text() string {
	var texts []string
	for _, p := range u.parts {
		if p.image == nil {
			texts = append(texts, p.text)
		}
	}
	return strings.Join(texts, "\n")
}

func (u userMessage) images() []image {
	var imgs []image
	for _, p := range u.parts {
		if p.image != nil {
			imgs = append(imgs, *p.image)
		}
	}
	return imgs
}

func (u userMessage) empty() bool { return len(u.parts) == 0 }

type toolCall struct {
	id   string // wire id, valid for Cursor
	name string
	args json.RawMessage // a JSON object
}

type itemKind int

const (
	itemUser itemKind = iota
	itemAssistant
	itemToolResult
)

// item is one history message in a form that does not depend on the request format.
type item struct {
	kind   itemKind
	user   userMessage // itemUser
	text   string      // itemAssistant
	calls  []toolCall  // itemAssistant
	callID string      // itemToolResult, wire id
	result string      // itemToolResult
}

// conversation is a request split into what goes to the blobs and what goes to the action.
type conversation struct {
	system []string
	// history is every message before the active one, in order.
	history []item
	// active is the trailing user message; nil when the request ends with tool results.
	active *userMessage
	// resolves are the trailing tool results, with the client's call ids.
	resolves []trailingResult
}

type trailingResult struct {
	callID string // as the client sent it
	text   string
}

func parseConversation(messages []Message, resolve ImageResolver) (*conversation, error) {
	c := &conversation{}
	pending := map[string]bool{}  // client call ids without a result yet
	issued := map[string]bool{}   // every client call id seen
	wire := map[string]string{}   // client call id -> wire id
	wireUsed := map[string]bool{} // wire ids taken, to catch sanitizing collisions

	for i, m := range messages {
		if m.Role != "tool" && len(pending) > 0 {
			return nil, fmt.Errorf("messages[%d]: tool calls %s have no result", i, sortedKeys(pending))
		}
		switch m.Role {
		case "system", "developer":
			if text := textOf(m.Content); strings.TrimSpace(text) != "" {
				c.system = append(c.system, text)
			}
		case "user":
			u, err := parseUser(m.Content, resolve)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			if u.empty() {
				continue
			}
			c.history = append(c.history, item{kind: itemUser, user: u})
		case "assistant":
			it := item{kind: itemAssistant, text: textOf(m.Content)}
			for j, tc := range m.ToolCalls {
				if tc.ID == "" {
					return nil, fmt.Errorf("messages[%d].tool_calls[%d]: id is required", i, j)
				}
				if issued[tc.ID] {
					return nil, fmt.Errorf("messages[%d].tool_calls[%d]: duplicate id %q", i, j, tc.ID)
				}
				args, err := parseArguments(tc.Function.Arguments)
				if err != nil {
					return nil, fmt.Errorf("messages[%d].tool_calls[%d]: %w", i, j, err)
				}
				w := wireCallID(tc.ID)
				if wireUsed[w] {
					return nil, fmt.Errorf("messages[%d].tool_calls[%d]: id %q collides with another id once sanitized", i, j, tc.ID)
				}
				issued[tc.ID], pending[tc.ID], wire[tc.ID], wireUsed[w] = true, true, w, true
				it.calls = append(it.calls, toolCall{id: w, name: tc.Function.Name, args: args})
			}
			if it.text == "" && len(it.calls) == 0 {
				continue
			}
			c.history = append(c.history, it)
		case "tool":
			if !pending[m.ToolCallID] {
				return nil, fmt.Errorf("messages[%d]: tool result for unknown or already answered call %q", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			c.history = append(c.history, item{kind: itemToolResult, callID: wire[m.ToolCallID], result: textOf(m.Content)})
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("tool calls %s have no result", sortedKeys(pending))
	}

	// The tail decides the action: a user message rides in it, tool results resume the run.
	switch n := len(c.history); {
	case n == 0:
		return nil, ErrNoActiveMessage
	case c.history[n-1].kind == itemUser:
		u := c.history[n-1].user
		c.active = &u
		c.history = c.history[:n-1]
	case c.history[n-1].kind == itemToolResult:
		clientID := map[string]string{}
		for id, w := range wire {
			clientID[w] = id
		}
		start := n
		for start > 0 && c.history[start-1].kind == itemToolResult {
			start--
		}
		for _, it := range c.history[start:] {
			c.resolves = append(c.resolves, trailingResult{callID: clientID[it.callID], text: it.result})
		}
	default:
		return nil, ErrNoActiveMessage
	}
	return c, nil
}

func parseUser(content Content, resolve ImageResolver) (userMessage, error) {
	var u userMessage
	for _, p := range content.Parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				u.parts = append(u.parts, userPart{text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return u, errors.New("image_url part without a url")
			}
			img, err := loadImage(p.ImageURL.URL, resolve)
			if err != nil {
				return u, err
			}
			u.parts = append(u.parts, userPart{image: &img})
		default:
			return u, fmt.Errorf("unsupported content part type %q", p.Type)
		}
	}
	return u, nil
}

func loadImage(rawURL string, resolve ImageResolver) (image, error) {
	if rest, ok := strings.CutPrefix(rawURL, "data:"); ok {
		meta, payload, ok := strings.Cut(rest, ",")
		mime, isBase64 := strings.CutSuffix(meta, ";base64")
		if !ok || !isBase64 || !strings.HasPrefix(mime, "image/") {
			return image{}, errors.New("image data URL must be image/*;base64")
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return image{}, fmt.Errorf("image data URL: %w", err)
		}
		return image{mime: mime, data: data}, nil
	}
	if resolve == nil {
		return image{}, fmt.Errorf("image URL %q is not a data: URL and no resolver is configured", rawURL)
	}
	mime, data, err := resolve(rawURL)
	if err != nil {
		return image{}, fmt.Errorf("image URL %q: %w", rawURL, err)
	}
	return image{mime: mime, data: data}, nil
}

// parseArguments checks that a tool call's arguments are a JSON object; an empty string is {}.
func parseArguments(s string) (json.RawMessage, error) {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}"), nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	return json.RawMessage(s), nil
}

// textOf joins the text parts of a content.
func textOf(c Content) string {
	var parts []string
	for _, p := range c.Parts {
		if p.Type == "text" && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func sortedKeys(m map[string]bool) string {
	return strings.Join(slices.Sorted(maps.Keys(m)), ", ")
}

// Command translate-live replays Chat Completions conversations against the real Cursor agent
// backend through internal/translate, to check the translation end to end. It is a throwaway
// check, not part of the plugin: every request spends the account's quota.
//
//	go run ./cmd/translate-live -scenario tool
//	go run ./cmd/translate-live -scenario memory
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	"github.com/mediavee/cpa-cursor/internal/translate"
)

const (
	agentURL      = "https://agentn.global.api5.cursor.sh"
	clientVersion = "cli-2026.10.22-27dcf5f"
	// settle is how long to wait for more tool calls after the first one of a step.
	settle = 1500 * time.Millisecond
)

// catalog is the one model this check uses, as AvailableModels describes it (variant fast=false,
// legacy slug composer-2.5).
type catalog struct{}

func (catalog) Lookup(slug string) (translate.Model, bool) {
	if slug != "composer-2.5" {
		return translate.Model{}, false
	}
	return translate.Model{ID: "composer-2.5", Slug: "composer-2.5", Parameters: []translate.Parameter{{ID: "fast", Value: "false"}}}, true
}

type live struct {
	token   string
	verbose bool
	stream  bool
	system  string
}

// withSystem prepends the system message, if any.
func (l *live) withSystem(msgs ...translate.Message) []translate.Message {
	if l.system == "" {
		return msgs
	}
	return append([]translate.Message{{Role: "system", Content: translate.TextContent(l.system)}}, msgs...)
}

func main() {
	scenario := flag.String("scenario", "tool", "tool | parallel | memory")
	verbose := flag.Bool("v", false, "print the root blobs of each request and every server message type")
	stream := flag.Bool("stream", false, "print streaming chunks instead of the aggregated response")
	system := flag.String("system", "You are a terse assistant.", "system message; empty sends none")
	flag.Parse()

	l := &live{token: keychain("cursor-access-token"), verbose: *verbose, stream: *stream, system: *system}
	var err error
	switch *scenario {
	case "tool":
		err = l.toolConversation()
	case "memory":
		err = l.memoryConversation()
	case "parallel":
		err = l.parallelToolConversation()
	default:
		err = fmt.Errorf("unknown scenario %q", *scenario)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

var weatherTool = translate.Tool{Type: "function", Function: translate.ToolFunction{
	Name:        "get_weather",
	Description: "Get the current weather for a city.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}}

// toolConversation is two HTTP requests with no state kept between them: the first ends on a tool
// call, the second brings the result and is replayed from the messages alone.
func (l *live) toolConversation() error {
	req1 := &translate.ChatRequest{
		Model: "composer-2.5",
		Messages: l.withSystem(
			translate.Message{Role: "user", Content: translate.TextContent("What is the weather in Paris? Use the tool, then answer in one short sentence.")},
		),
		Tools: []translate.Tool{weatherTool},
	}
	resp1, err := l.chat("request 1", req1)
	if err != nil {
		return err
	}
	msg := resp1.Choices[0].Message
	if resp1.Choices[0].FinishReason != "tool_calls" || len(msg.ToolCalls) != 1 {
		return fmt.Errorf("request 1: want one tool call, got finish=%s calls=%d", resp1.Choices[0].FinishReason, len(msg.ToolCalls))
	}
	call := msg.ToolCalls[0]
	if call.Function.Name != "get_weather" || !strings.Contains(call.Function.Arguments, "Paris") {
		return fmt.Errorf("request 1: unexpected call %+v", call)
	}

	req2 := *req1
	req2.Messages = append(append([]translate.Message{}, req1.Messages...),
		translate.Message{Role: "assistant", Content: translate.TextContent(deref(msg.Content)), ToolCalls: msg.ToolCalls},
		translate.Message{Role: "tool", ToolCallID: call.ID, Content: translate.TextContent("Sunny, 21C")},
	)
	resp2, err := l.chat("request 2 (stateless replay)", &req2)
	if err != nil {
		return err
	}
	answer := deref(resp2.Choices[0].Message.Content)
	if !strings.Contains(answer, "21") {
		return fmt.Errorf("request 2: the answer does not use the tool result: %q", answer)
	}
	fmt.Printf("answer uses the result: %q\n", answer)
	return nil
}

// parallelToolConversation asks for two calls in one step and answers them in reverse order.
func (l *live) parallelToolConversation() error {
	req1 := &translate.ChatRequest{
		Model: "composer-2.5",
		Messages: l.withSystem(
			translate.Message{Role: "user", Content: translate.TextContent("What is the weather in Paris and in Tokyo? Call get_weather for both cities at the same time, then answer in one short sentence.")},
		),
		Tools: []translate.Tool{weatherTool},
	}
	resp1, err := l.chat("request 1", req1)
	if err != nil {
		return err
	}
	msg := resp1.Choices[0].Message
	if len(msg.ToolCalls) != 2 {
		return fmt.Errorf("request 1: want two tool calls, got %d (finish=%s)", len(msg.ToolCalls), resp1.Choices[0].FinishReason)
	}
	req2 := *req1
	req2.Messages = append(append([]translate.Message{}, req1.Messages...),
		translate.Message{Role: "assistant", Content: translate.TextContent(deref(msg.Content)), ToolCalls: msg.ToolCalls})
	for i := len(msg.ToolCalls) - 1; i >= 0; i-- {
		call := msg.ToolCalls[i]
		result := "Rainy, 12C"
		if strings.Contains(call.Function.Arguments, "Paris") {
			result = "Sunny, 21C"
		}
		req2.Messages = append(req2.Messages, translate.Message{Role: "tool", ToolCallID: call.ID, Content: translate.TextContent(result)})
	}
	resp2, err := l.chat("request 2 (stateless replay)", &req2)
	if err != nil {
		return err
	}
	answer := deref(resp2.Choices[0].Message.Content)
	if !strings.Contains(answer, "21") || !strings.Contains(answer, "12") {
		return fmt.Errorf("request 2: the answer does not use both results: %q", answer)
	}
	fmt.Printf("answer uses both results: %q\n", answer)
	return nil
}

// memoryConversation is three text turns; the third asks for what the first said.
func (l *live) memoryConversation() error {
	msgs := l.withSystem(
		translate.Message{Role: "user", Content: translate.TextContent("My secret word is AZURE-42. Remember it. Reply with: noted.")},
	)
	for turn, next := range []string{"What is 2+2? Reply with the number only.", "What is my secret word? Reply with the word only."} {
		resp, err := l.chat(fmt.Sprintf("turn %d", turn+1), &translate.ChatRequest{Model: "composer-2.5", Messages: msgs})
		if err != nil {
			return err
		}
		msgs = append(msgs,
			translate.Message{Role: "assistant", Content: translate.TextContent(deref(resp.Choices[0].Message.Content))},
			translate.Message{Role: "user", Content: translate.TextContent(next)},
		)
	}
	resp, err := l.chat("turn 3", &translate.ChatRequest{Model: "composer-2.5", Messages: msgs})
	if err != nil {
		return err
	}
	if answer := deref(resp.Choices[0].Message.Content); !strings.Contains(answer, "AZURE-42") {
		return fmt.Errorf("turn 3: the model forgot the first turn: %q", answer)
	}
	return nil
}

// chat serves one Chat Completions request with one Cursor run and prints what a client would get.
func (l *live) chat(label string, req *translate.ChatRequest) (*translate.ChatResponse, error) {
	run, err := translate.Translate(req, translate.Options{Catalog: catalog{}})
	if err != nil {
		return nil, fmt.Errorf("%s: translate: %w", label, err)
	}
	fmt.Printf("== %s: %d messages, action %T, %d blobs, resolves %d\n", label, len(req.Messages), run.Request.Action.Action, len(run.Blobs), len(run.Resolves))
	if l.verbose {
		for _, id := range run.Request.ConversationState.RootPromptMessagesJson {
			for _, b := range run.Blobs {
				if bytes.Equal(b.ID, id) {
					fmt.Printf("   root %x %s\n", id[:4], b.Data)
				}
			}
		}
	}
	enc := translate.NewEncoder("chatcmpl-"+uuid.NewString(), req.Model, time.Now().Unix())
	finish, err := l.execute(run, enc, func(c translate.ChatChunk) {
		if l.stream {
			b, _ := json.Marshal(c)
			fmt.Printf("   chunk %s\n", b)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	last := enc.Finish(finish)
	if l.stream {
		b, _ := json.Marshal(last)
		fmt.Printf("   chunk %s\n", b)
	}
	resp := enc.Response()
	b, _ := json.Marshal(resp)
	fmt.Printf("   response %s\n", b)
	return &resp, nil
}

// execute runs run on the Cursor backend and feeds the encoder until the turn ends or tool calls
// arrive. It keeps no run alive after returning.
func (l *live) execute(run *translate.Run, enc *translate.Encoder, emit func(translate.ChatChunk)) (translate.FinishReason, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	blobs := map[string][]byte{}
	for _, b := range run.Blobs {
		blobs[hex.EncodeToString(b.ID)] = b.Data
	}

	pr, pw := io.Pipe()
	var wmu sync.Mutex
	send := func(m *agentv1.AgentClientMessage) {
		b, err := proto.Marshal(m)
		if err != nil {
			panic(err)
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = pw.Write(frame(b))
	}

	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, agentURL+"/agent.v1.AgentService/Run", pr)
	reqID := uuid.NewString()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	for k, v := range map[string]string{
		"authorization":            "Bearer " + l.token,
		"content-type":             "application/connect+proto",
		"connect-protocol-version": "1",
		"x-ghost-mode":             "true",
		"x-cursor-client-version":  clientVersion,
		"x-cursor-client-type":     "cli",
		"x-request-id":             reqID,
		"x-original-request-id":    reqID,
		"x-blob-encryption-key":    hex.EncodeToString(key),
		"user-agent":               "connect-es/1.6.1",
	} {
		httpReq.Header.Set(k, v)
	}
	tr := &http2.Transport{DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		return (&tls.Dialer{Config: cfg}).DialContext(ctx, network, addr)
	}, ReadIdleTimeout: 10 * time.Second, PingTimeout: 20 * time.Second}

	go send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run.Request}})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &agentv1.ClientHeartbeat{}}})
			}
		}
	}()

	resp, err := tr.RoundTrip(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	type frameMsg struct {
		flags   byte
		payload []byte
		err     error
	}
	frames := make(chan frameMsg)
	go func() {
		defer close(frames)
		for {
			flags, payload, err := readFrame(resp.Body)
			select {
			case frames <- frameMsg{flags, payload, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	finish := translate.FinishStop
	var settleTimer <-chan time.Time
	toolCalls := 0
	for {
		select {
		case <-settleTimer:
			_ = pw.Close()
			return translate.FinishToolCalls, nil
		case f, ok := <-frames:
			if !ok {
				return finish, nil
			}
			if f.err != nil {
				if errors.Is(f.err, io.EOF) {
					return finish, nil
				}
				return "", f.err
			}
			if f.flags&0x02 != 0 {
				if s := strings.TrimSpace(string(f.payload)); s != "{}" {
					return "", fmt.Errorf("end of stream: %s", s)
				}
				return finish, nil
			}
			var m agentv1.AgentServerMessage
			if err := proto.Unmarshal(f.payload, &m); err != nil {
				return "", err
			}
			switch msg := m.Message.(type) {
			case *agentv1.AgentServerMessage_InteractionUpdate:
				for _, c := range enc.Update(msg.InteractionUpdate) {
					emit(c)
				}
				if _, ok := msg.InteractionUpdate.Message.(*agentv1.InteractionUpdate_TurnEnded); ok {
					cancel()
					return finish, nil
				}
			case *agentv1.AgentServerMessage_KvServerMessage:
				send(kvReply(msg.KvServerMessage, blobs))
			case *agentv1.AgentServerMessage_ExecServerMessage:
				ex := msg.ExecServerMessage
				switch a := ex.Message.(type) {
				case *agentv1.ExecServerMessage_RequestContextArgs:
					send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: &agentv1.ExecClientMessage{
						Id: ex.Id, ExecId: ex.ExecId,
						Message: &agentv1.ExecClientMessage_RequestContextResult{RequestContextResult: &agentv1.RequestContextResult{
							Result: &agentv1.RequestContextResult_Success{Success: &agentv1.RequestContextSuccess{RequestContext: run.RequestContext}},
						}},
					}}})
				case *agentv1.ExecServerMessage_McpArgs:
					chunk, err := enc.ToolCall(a.McpArgs)
					if err != nil {
						return "", err
					}
					emit(chunk)
					toolCalls++
					if toolCalls == 1 {
						settleTimer = time.After(settle)
					}
				default:
					send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: &agentv1.ExecClientControlMessage{
						Message: &agentv1.ExecClientControlMessage_Throw{Throw: &agentv1.ExecClientThrow{Id: ex.Id, Error: "Tool not available in this client", ErrorCode: proto.String("tool_unavailable")}},
					}}})
					send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: &agentv1.ExecClientControlMessage{
						Message: &agentv1.ExecClientControlMessage_StreamClose{StreamClose: &agentv1.ExecClientStreamClose{Id: ex.Id}},
					}}})
				}
			}
		}
	}
}

func kvReply(kv *agentv1.KvServerMessage, blobs map[string][]byte) *agentv1.AgentClientMessage {
	out := &agentv1.KvClientMessage{Id: kv.Id}
	switch a := kv.Message.(type) {
	case *agentv1.KvServerMessage_GetBlobArgs:
		out.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: &agentv1.GetBlobResult{BlobData: blobs[hex.EncodeToString(a.GetBlobArgs.BlobId)]}}
	case *agentv1.KvServerMessage_SetBlobArgs:
		blobs[hex.EncodeToString(a.SetBlobArgs.BlobId)] = a.SetBlobArgs.BlobData
		out.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
	}
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: out}}
}

func frame(b []byte) []byte {
	out := make([]byte, 5+len(b))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(b)))
	copy(out[5:], b)
	return out
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(h[1:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	if h[0]&0x01 != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return 0, nil, err
		}
		if b, err = io.ReadAll(zr); err != nil {
			return 0, nil, err
		}
		h[0] &^= 0x01
	}
	return h[0], b, nil
}

func keychain(service string) string {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", "cursor-user", "-w").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "keychain:", err)
		os.Exit(1)
	}
	return string(bytes.TrimSpace(out))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

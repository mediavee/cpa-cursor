// Command spike sends one supervised request to Cursor's agent backend and prints the stream.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

const (
	agentURL      = "https://agentn.global.api5.cursor.sh"
	clientVersion = "cli-2026.10.22-27dcf5f"
)

func main() {
	model := flag.String("model", "composer-2.5", "model id")
	prompt := flag.String("prompt", "Reply with the single word: pong", "user text")
	system := flag.String("system", "", "system prompt")
	history := flag.Bool("history", false, "send a prior exchange via conversation_history")
	rootHistory := flag.Bool("root-history", false, "send a prior exchange via root_prompt_messages_json")
	tool := flag.Bool("tool", false, "declare a client tool get_weather")
	toolResult := flag.String("tool-result", "", "answer the first mcp_args with this text")
	verbose := flag.Bool("v", false, "print every server message")
	flag.Parse()

	token := keychain("cursor-access-token")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	blobs := map[string][]byte{}
	store := func(b []byte) []byte {
		id := sha256.Sum256(b)
		blobs[hex.EncodeToString(id[:])] = b
		return id[:]
	}
	sys := *system
	if sys == "" {
		sys = "You are a helpful assistant."
	}
	root := [][]byte{store(mustJSON(map[string]string{"role": "system", "content": sys}))}
	if *rootHistory && *system != "" {
		root = append(root, store(mustJSON(map[string]string{"role": "user", "content": "<rules>\n" + *system + "\n</rules>"})))
	}
	if *rootHistory {
		root = append(root,
			store(mustJSON(map[string]string{"role": "user", "content": "My secret word is AZURE-42. Remember it."})),
			store(mustJSON(map[string]string{"role": "assistant", "content": "Noted: your secret word is AZURE-42."})))
	}

	userAction := &agentv1.UserMessageAction{UserMessage: &agentv1.UserMessage{
		Text: *prompt, MessageId: uuid.NewString(), Mode: agentv1.AgentMode(1),
	}}
	if *history {
		userAction.ConversationHistory = &agentv1.ConversationHistory{Messages: []*agentv1.ConversationHistoryMessage{
			historyUser("My secret word is AZURE-42. Remember it."),
			historyAssistant("Noted: your secret word is AZURE-42."),
		}}
	}

	var tools []*agentv1.McpToolDefinition
	if *tool {
		schema, _ := structpb.NewValue(map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		})
		tools = append(tools, &agentv1.McpToolDefinition{
			Name: "client-get_weather", ProviderIdentifier: "client", ToolName: "get_weather",
			Description: "Get the current weather for a city.", InputSchema: schema,
		})
	}

	convID := uuid.NewString()
	run := &agentv1.AgentRunRequest{
		ConversationState: &agentv1.ConversationStateStructure{RootPromptMessagesJson: root},
		Action:            &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: userAction}},
		ModelDetails:      &agentv1.ModelDetails{ModelId: *model, DisplayModelId: *model, DisplayName: *model},
		RequestedModel:    &agentv1.RequestedModel{ModelId: *model},
		ConversationId:    proto.String(convID),
	}
	if len(tools) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: tools}
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
		if _, err := pw.Write(frame(b)); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
		}
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, agentURL+"/agent.v1.AgentService/Run", pr)
	reqID := uuid.NewString()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	for k, v := range map[string]string{
		"authorization":            "Bearer " + token,
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
		req.Header.Set(k, v)
	}

	tr := &http2.Transport{DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
		return (&tls.Dialer{Config: cfg}).DialContext(ctx, network, addr)
	}, ReadIdleTimeout: 10 * time.Second, PingTimeout: 20 * time.Second}

	go send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run}})
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

	start := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "roundtrip:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "HTTP %d %s (%.1fs)\n", resp.StatusCode, resp.Header.Get("content-type"), time.Since(start).Seconds())
	for k, v := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-cursor") {
			fmt.Fprintln(os.Stderr, " ", k, v)
		}
	}
	defer resp.Body.Close()

	answered := false
	for {
		flags, payload, err := readFrame(resp.Body)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "read:", err)
			}
			break
		}
		if flags&0x02 != 0 {
			fmt.Fprintf(os.Stderr, "\n[end-stream] %s\n", payload)
			break
		}
		var m agentv1.AgentServerMessage
		if err := proto.Unmarshal(payload, &m); err != nil {
			fmt.Fprintln(os.Stderr, "decode:", err)
			continue
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "<< %s\n", trim(protojson.Format(&m), 600))
		}
		switch msg := m.Message.(type) {
		case *agentv1.AgentServerMessage_InteractionUpdate:
			switch u := msg.InteractionUpdate.Message.(type) {
			case *agentv1.InteractionUpdate_TextDelta:
				fmt.Print(u.TextDelta.Text)
			case *agentv1.InteractionUpdate_ThinkingDelta:
				fmt.Fprintf(os.Stderr, "[think] %s", u.ThinkingDelta.Text)
			case *agentv1.InteractionUpdate_TurnEnded:
				fmt.Fprintf(os.Stderr, "\n[turn_ended] %s (%.1fs)\n", protojson.Format(u.TurnEnded), time.Since(start).Seconds())
				cancel()
			case *agentv1.InteractionUpdate_RoutedModel:
				fmt.Fprintf(os.Stderr, "[routed_model] %s\n", protojson.Format(u.RoutedModel))
			}
		case *agentv1.AgentServerMessage_KvServerMessage:
			kv := msg.KvServerMessage
			out := &agentv1.KvClientMessage{Id: kv.Id}
			switch a := kv.Message.(type) {
			case *agentv1.KvServerMessage_GetBlobArgs:
				data, ok := blobs[hex.EncodeToString(a.GetBlobArgs.BlobId)]
				fmt.Fprintf(os.Stderr, "[kv get %x found=%v]\n", a.GetBlobArgs.BlobId[:6], ok)
				r := &agentv1.GetBlobResult{}
				if ok {
					r.BlobData = data
				}
				out.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: r}
			case *agentv1.KvServerMessage_SetBlobArgs:
				blobs[hex.EncodeToString(a.SetBlobArgs.BlobId)] = a.SetBlobArgs.BlobData
				fmt.Fprintf(os.Stderr, "[kv set %x %dB]\n", a.SetBlobArgs.BlobId[:6], len(a.SetBlobArgs.BlobData))
				out.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
			}
			send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: out}})
		case *agentv1.AgentServerMessage_ExecServerMessage:
			ex := msg.ExecServerMessage
			switch a := ex.Message.(type) {
			case *agentv1.ExecServerMessage_RequestContextArgs:
				fmt.Fprintln(os.Stderr, "[exec request_context]")
				rc := &agentv1.RequestContext{Tools: tools, Env: &agentv1.RequestContextEnv{}}
				if *system != "" {
					rc.Rules = []*agentv1.CursorRule{{
						FullPath: "/client/system-prompt/0.mdc", Content: *system, Source: agentv1.CursorRuleSource_CURSOR_RULE_SOURCE_USER,
						Type: &agentv1.CursorRuleType{Type: &agentv1.CursorRuleType_Global{Global: &agentv1.CursorRuleTypeGlobal{}}},
					}}
				}
				send(execResult(ex, &agentv1.ExecClientMessage_RequestContextResult{RequestContextResult: &agentv1.RequestContextResult{
					Result: &agentv1.RequestContextResult_Success{Success: &agentv1.RequestContextSuccess{RequestContext: rc}},
				}}))
			case *agentv1.ExecServerMessage_McpArgs:
				args, _ := json.Marshal(a.McpArgs.Args)
				fmt.Fprintf(os.Stderr, "\n[tool_call] id=%s name=%s tool=%s args=%s\n", a.McpArgs.ToolCallId, a.McpArgs.Name, a.McpArgs.ToolName, args)
				if *toolResult != "" && !answered {
					answered = true
					send(execResult(ex, &agentv1.ExecClientMessage_McpResult{McpResult: &agentv1.McpResult{
						Result: &agentv1.McpResult_Success{Success: &agentv1.McpSuccess{Content: []*agentv1.McpToolResultContentItem{
							{Content: &agentv1.McpToolResultContentItem_Text{Text: &agentv1.McpTextContent{Text: *toolResult}}},
						}}},
					}}))
					send(streamClose(ex))
				} else {
					cancel()
				}
			default:
				fmt.Fprintf(os.Stderr, "[exec %T rejected]\n", a)
				send(&agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: &agentv1.ExecClientControlMessage{
					Message: &agentv1.ExecClientControlMessage_Throw{Throw: &agentv1.ExecClientThrow{Id: ex.Id, Error: "Tool not available in this client", ErrorCode: proto.String("tool_unavailable")}},
				}}})
				send(streamClose(ex))
			}
		case *agentv1.AgentServerMessage_InteractionQuery:
			fmt.Fprintf(os.Stderr, "[interaction_query] %s\n", trim(protojson.Format(msg.InteractionQuery), 400))
		}
	}
	_ = pw.Close()
}

func execResult(ex *agentv1.ExecServerMessage, m any) *agentv1.AgentClientMessage {
	out := &agentv1.ExecClientMessage{Id: ex.Id, ExecId: ex.ExecId}
	switch v := m.(type) {
	case *agentv1.ExecClientMessage_RequestContextResult:
		out.Message = v
	case *agentv1.ExecClientMessage_McpResult:
		out.Message = v
	}
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: out}}
}

func streamClose(ex *agentv1.ExecServerMessage) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_StreamClose{StreamClose: &agentv1.ExecClientStreamClose{Id: ex.Id}},
	}}}
}

func historyUser(text string) *agentv1.ConversationHistoryMessage {
	return &agentv1.ConversationHistoryMessage{Message: &agentv1.ConversationHistoryMessage_User{User: &agentv1.ConversationHistoryUserMessage{
		Content: []*agentv1.ConversationHistoryUserContent{{Content: &agentv1.ConversationHistoryUserContent_Text{Text: &agentv1.ConversationHistoryTextContent{Text: text}}}},
	}}}
}

func historyAssistant(text string) *agentv1.ConversationHistoryMessage {
	return &agentv1.ConversationHistoryMessage{Message: &agentv1.ConversationHistoryMessage_Assistant{Assistant: &agentv1.ConversationHistoryAssistantMessage{
		Content: []*agentv1.ConversationHistoryAssistantContent{{Content: &agentv1.ConversationHistoryAssistantContent_Text{Text: &agentv1.ConversationHistoryTextContent{Text: text}}}},
	}}}
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
		return h[0], nil, errors.New("compressed frame: not handled in spike")
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

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

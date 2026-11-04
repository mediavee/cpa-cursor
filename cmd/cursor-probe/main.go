// Command cursor-probe is a diagnostic client for the Cursor backend: it lists
// models, reads usage and sends one prompt, printing the event stream.
//
// The access token comes from the CURSOR_ACCESS_TOKEN environment variable or
// from the file named by -token-file; it is never printed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/mediavee/cpa-cursor/internal/cursor"
	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
)

const usage = `usage: cursor-probe [global flags] <command> [flags]

commands:
  models   list the usable models
  usage    print the plan, the usage of the billing period and the limit status
  run      send one prompt and print the stream

global flags:
  -token-file path   read the access token from a file instead of $CURSOR_ACCESS_TOKEN
  -version string    x-cursor-client-version (default ` + cursor.DefaultClientVersion + `)

run flags:
  -model id          model id (default composer-2.5)
  -prompt text       user message
  -system text       system prompt, sent as a global rule
  -tool              declare a client tool get_weather(city)
  -tool-result text  answer the tool call with this text and let the run finish
  -v                 print every event as JSON
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ce *cursor.Error
		if errors.As(err, &ce) {
			fmt.Fprintf(os.Stderr, "class=%s connect=%s cursor_code=%s http=%d\n", ce.Class, ce.ConnectCode, ce.Code, ce.HTTPStatus)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("cursor-probe", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	tokenFile := global.String("token-file", "", "")
	version := global.String("version", cursor.DefaultClientVersion, "")
	if err := global.Parse(args); err != nil {
		return err
	}
	if global.NArg() == 0 {
		global.Usage()
		return errors.New("missing command")
	}

	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	cfg := cursor.DefaultConfig()
	cfg.ClientVersion = *version
	client, err := cursor.NewClient(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd, rest := global.Arg(0), global.Args()[1:]; cmd {
	case "models":
		return models(ctx, client, token, stdout)
	case "usage":
		return usageCmd(ctx, client, token, stdout)
	case "run":
		return runCmd(ctx, client, token, rest, stdout, stderr)
	default:
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func readToken(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		return nonEmpty(string(b))
	}
	return nonEmpty(os.Getenv("CURSOR_ACCESS_TOKEN"))
}

func nonEmpty(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no token: set CURSOR_ACCESS_TOKEN or pass -token-file")
	}
	return s, nil
}

func models(ctx context.Context, c *cursor.Client, token string, out io.Writer) error {
	usable, err := c.GetUsableModels(ctx, token)
	if err != nil {
		return err
	}
	available, err := c.AvailableModels(ctx, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "usable models: %d\n", len(usable))
	for _, m := range usable {
		fmt.Fprintf(out, "  %-40s %s\n", m.GetModelId(), m.GetDisplayName())
	}
	fmt.Fprintf(out, "base models with parameters: %d\n", len(available.Models))
	for _, m := range available.Models {
		var params []string
		for _, p := range m.ParameterDefinitions {
			var vals []string
			for _, v := range p.Values() {
				vals = append(vals, v.Value)
			}
			params = append(params, p.ID+"="+strings.Join(vals, "|"))
		}
		fmt.Fprintf(out, "  %-32s images=%-5v thinking=%-5v %s\n", m.Name, m.SupportsImages, m.SupportsThinking, strings.Join(params, " "))
	}
	return nil
}

func usageCmd(ctx context.Context, c *cursor.Client, token string, out io.Writer) error {
	plan, err := c.GetPlanInfo(ctx, token)
	if err != nil {
		return err
	}
	period, err := c.GetCurrentPeriodUsage(ctx, token)
	if err != nil {
		return err
	}
	status, err := c.GetUsageLimitStatusAndActiveGrants(ctx, token)
	if err != nil {
		return err
	}
	p := plan.PlanInfo
	fmt.Fprintf(out, "plan: %s (%s), cycle ends %s\n", p.PlanName, p.Price, p.BillingCycleEnd.Time().Format(time.RFC3339))
	u := period.PlanUsage
	fmt.Fprintf(out, "usage: total %.1f%%, auto pool %.1f%%, api pool %.1f%% (limit %.0f)\n", u.TotalPercentUsed, u.AutoPercentUsed, u.APIPercentUsed, u.Limit)
	fmt.Fprintf(out, "message: %s\n", period.DisplayMessage)
	s := status.Policy
	fmt.Fprintf(out, "limit status: slow_pool=%v stage=%q limit_type=%q resets=%s\n", s.IsInSlowPool, s.Stage, s.LimitType, s.ResetAtMs.Time().Format(time.RFC3339))
	if s.ErrorTitle != "" {
		fmt.Fprintf(out, "  %s: %s\n", s.ErrorTitle, s.ErrorDetail)
	}
	return nil
}

func runCmd(ctx context.Context, c *cursor.Client, token string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	model := fs.String("model", "composer-2.5", "")
	prompt := fs.String("prompt", "Reply with the single word: pong", "")
	system := fs.String("system", "", "")
	withTool := fs.Bool("tool", false, "")
	toolResult := fs.String("tool-result", "", "")
	verbose := fs.Bool("v", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}

	params, err := buildRun(*model, *prompt, *system, *withTool)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	start := time.Now()
	s, err := c.Run(ctx, token, params)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(stderr, "request %s\n", s.RequestID())

	for {
		ev, err := s.Next(ctx)
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "\n[end] %.1fs\n", time.Since(start).Seconds())
			return nil
		}
		if err != nil {
			return err
		}
		if *verbose {
			printEvent(stderr, ev)
		}
		switch e := ev.(type) {
		case cursor.UpdateEvent:
			switch u := e.Update.GetMessage().(type) {
			case *agentv1.InteractionUpdate_TextDelta:
				fmt.Fprint(stdout, u.TextDelta.GetText())
			case *agentv1.InteractionUpdate_ThinkingDelta:
				fmt.Fprint(stderr, u.ThinkingDelta.GetText())
			case *agentv1.InteractionUpdate_RoutedModel:
				fmt.Fprintf(stderr, "[routed_model] %s\n", u.RoutedModel.GetDisplayName())
			case *agentv1.InteractionUpdate_TurnEnded:
				fmt.Fprintf(stderr, "\n[turn_ended] %s (%.1fs)\n", protojson.Format(u.TurnEnded), time.Since(start).Seconds())
			}
		case cursor.ToolCallsEvent:
			var results []cursor.ToolResult
			for _, call := range e.Calls {
				fmt.Fprintf(stderr, "\n[tool_call] id=%s name=%s args=%s\n", call.GetToolCallId(), call.GetToolName(), protojson.MarshalOptions{}.Format(&structpb.Struct{Fields: call.GetArgs()}))
				results = append(results, cursor.ToolResult{ToolCallID: call.GetToolCallId(), Content: []cursor.ToolContent{{Text: *toolResult}}})
			}
			if *toolResult == "" {
				fmt.Fprintln(stderr, "[stop] no -tool-result given")
				return nil
			}
			if err := s.SubmitToolResults(results...); err != nil {
				return err
			}
		}
	}
}

// buildRun is the probe's own minimal request. The real translation from
// OpenAI messages lives elsewhere.
func buildRun(model, prompt, system string, withTool bool) (cursor.RunParams, error) {
	blobs := cursor.NewBlobs()
	sysContent := system
	if sysContent == "" {
		sysContent = "You are a helpful assistant."
	}
	sysBlob, err := json.Marshal(map[string]string{"role": "system", "content": sysContent})
	if err != nil {
		return cursor.RunParams{}, err
	}
	root := [][]byte{blobs.Put(sysBlob)}

	req := &agentv1.AgentRunRequest{
		ConversationState: &agentv1.ConversationStateStructure{RootPromptMessagesJson: root},
		Action: &agentv1.ConversationAction{Action: &agentv1.ConversationAction_UserMessageAction{UserMessageAction: &agentv1.UserMessageAction{
			UserMessage: &agentv1.UserMessage{Text: prompt, MessageId: uuid.NewString(), Mode: agentv1.AgentMode(1)},
		}}},
		ModelDetails:   &agentv1.ModelDetails{ModelId: model, DisplayModelId: model, DisplayName: model},
		RequestedModel: &agentv1.RequestedModel{ModelId: model},
		ConversationId: proto.String(uuid.NewString()),
	}
	params := cursor.RunParams{Request: req, Blobs: blobs}

	if system != "" {
		params.Rules = []*agentv1.CursorRule{{
			FullPath: "/client/system-prompt/0.mdc", Content: system, Source: agentv1.CursorRuleSource_CURSOR_RULE_SOURCE_USER,
			Type: &agentv1.CursorRuleType{Type: &agentv1.CursorRuleType_Global{Global: &agentv1.CursorRuleTypeGlobal{}}},
		}}
	}
	if withTool {
		schema, err := structpb.NewValue(map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		})
		if err != nil {
			return cursor.RunParams{}, err
		}
		params.Tools = []*agentv1.McpToolDefinition{{
			Name: "client-get_weather", ProviderIdentifier: "client", ToolName: "get_weather",
			Description: "Get the current weather for a city.", InputSchema: schema,
		}}
		req.McpTools = &agentv1.McpTools{McpTools: params.Tools}
	}
	return params, nil
}

func printEvent(w io.Writer, ev cursor.Event) {
	var m proto.Message
	switch e := ev.(type) {
	case cursor.UpdateEvent:
		m = e.Update
	case cursor.TtftEvent:
		m = e.Breakdown
	case cursor.CheckpointEvent:
		m = e.State
	case cursor.ToolCallsEvent:
		for _, c := range e.Calls {
			fmt.Fprintf(w, "<< tool_calls %s\n", protojson.Format(c))
		}
		return
	}
	s := strings.Join(strings.Fields(protojson.Format(m)), " ")
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	fmt.Fprintf(w, "<< %T %s\n", ev, s)
}

package cursor

import (
	agentv1 "github.com/mediavee/cpa-cursor/internal/gen/agent/v1"
	"google.golang.org/protobuf/proto"
)

// mcpServerIdentifier is the provider_identifier of every client tool.
const mcpServerIdentifier = "client"

// Error codes carried by ExecClientThrow, so the server and our logs can tell
// a built-in tool we do not offer from a message we cannot even name.
const (
	throwToolUnavailable = "tool_unavailable"
	throwUnknownExec     = "unknown_exec_variant"
)

func clientExec(id uint32, execID string, set func(*agentv1.ExecClientMessage)) *agentv1.AgentClientMessage {
	m := &agentv1.ExecClientMessage{Id: id, ExecId: execID}
	set(m)
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: m}}
}

func execControl(c *agentv1.ExecClientControlMessage) *agentv1.AgentClientMessage {
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: c}}
}

func streamClose(id uint32) *agentv1.AgentClientMessage {
	return execControl(&agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_StreamClose{StreamClose: &agentv1.ExecClientStreamClose{Id: id}},
	})
}

func throwMessage(id uint32, text, code string) *agentv1.AgentClientMessage {
	return execControl(&agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_Throw{Throw: &agentv1.ExecClientThrow{
			Id: id, Error: text, ErrorCode: proto.String(code),
		}},
	})
}

// handleExec answers one exec_server_message. Every exec gets an answer: the
// server blocks on silence. Only mcp_args is left open when it names one of
// our tools; SubmitToolResults answers it later.
func (s *Session) handleExec(ex *agentv1.ExecServerMessage) {
	id, execID := ex.GetId(), ex.GetExecId()
	switch a := ex.GetMessage().(type) {
	case *agentv1.ExecServerMessage_RequestContextArgs:
		s.send(clientExec(id, execID, func(m *agentv1.ExecClientMessage) {
			m.Message = &agentv1.ExecClientMessage_RequestContextResult{RequestContextResult: &agentv1.RequestContextResult{
				Result: &agentv1.RequestContextResult_Success{Success: &agentv1.RequestContextSuccess{RequestContext: s.requestContext()}},
			}}
		}))
	case *agentv1.ExecServerMessage_McpStateExecArgs:
		s.send(clientExec(id, execID, func(m *agentv1.ExecClientMessage) {
			m.Message = &agentv1.ExecClientMessage_McpStateExecResult{McpStateExecResult: s.mcpState(a.McpStateExecArgs)}
		}))
	case *agentv1.ExecServerMessage_ListMcpResourcesExecArgs:
		s.send(clientExec(id, execID, func(m *agentv1.ExecClientMessage) {
			m.Message = &agentv1.ExecClientMessage_ListMcpResourcesExecResult{ListMcpResourcesExecResult: &agentv1.ListMcpResourcesExecResult{
				Result: &agentv1.ListMcpResourcesExecResult_Success{Success: &agentv1.ListMcpResourcesSuccess{}},
			}}
		}))
	case *agentv1.ExecServerMessage_ReadMcpResourceExecArgs:
		s.send(clientExec(id, execID, func(m *agentv1.ExecClientMessage) {
			m.Message = &agentv1.ExecClientMessage_ReadMcpResourceExecResult{ReadMcpResourceExecResult: &agentv1.ReadMcpResourceExecResult{
				Result: &agentv1.ReadMcpResourceExecResult_NotFound{NotFound: &agentv1.ReadMcpResourceNotFound{Uri: a.ReadMcpResourceExecArgs.GetUri()}},
			}}
		}))
	case *agentv1.ExecServerMessage_McpArgs:
		s.onMcpArgs(ex, a.McpArgs)
	case nil:
		// A oneof number this build does not model: the message decodes with
		// no case, and the exec would hang if we stayed silent.
		s.send(throwMessage(id, "Unknown exec message variant", throwUnknownExec))
		s.send(streamClose(id))
	default:
		s.send(throwMessage(id, "Tool not available in this client", throwToolUnavailable))
		s.send(streamClose(id))
	}
}

func (s *Session) requestContext() *agentv1.RequestContext {
	env := s.params.Env
	if env == nil {
		env = &agentv1.RequestContextEnv{}
	}
	return &agentv1.RequestContext{Rules: s.params.Rules, Env: env, Tools: s.params.Tools}
}

func (s *Session) mcpState(args *agentv1.McpStateExecArgs) *agentv1.McpStateExecResult {
	var servers []*agentv1.McpStateServer
	wanted := args.GetServerIdentifiers()
	if len(wanted) == 0 || containsString(wanted, mcpServerIdentifier) {
		servers = append(servers, &agentv1.McpStateServer{
			ServerName:       mcpServerIdentifier,
			ServerIdentifier: mcpServerIdentifier,
			Tools:            s.params.Tools,
		})
	}
	return &agentv1.McpStateExecResult{Result: &agentv1.McpStateExecResult_Success{Success: &agentv1.McpStateSuccess{Servers: servers}}}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// knowsTool reports whether a declared client tool matches an mcp_args.
func (s *Session) knowsTool(a *agentv1.McpArgs) bool {
	for _, d := range s.params.Tools {
		if a.GetName() == d.GetName() ||
			(a.GetProviderIdentifier() == d.GetProviderIdentifier() && a.GetToolName() == d.GetToolName()) {
			return true
		}
	}
	return false
}

func (s *Session) toolNames() []string {
	names := make([]string, 0, len(s.params.Tools))
	for _, d := range s.params.Tools {
		names = append(names, d.GetName())
	}
	return names
}

// refuseQuery answers an interaction_query with a rejection of its own kind.
// Where the protocol has no rejection variant, the closest negative answer is
// used; a query kind we cannot name still gets an empty response, which at
// least unblocks the id.
func refuseQuery(q *agentv1.InteractionQuery) *agentv1.AgentClientMessage {
	const reason = "Not supported by this client"
	r := &agentv1.InteractionResponse{Id: q.GetId()}
	switch q.GetQuery().(type) {
	case *agentv1.InteractionQuery_WebSearchRequestQuery:
		r.Result = &agentv1.InteractionResponse_WebSearchRequestResponse{WebSearchRequestResponse: &agentv1.WebSearchRequestResponse{
			Result: &agentv1.WebSearchRequestResponse_Rejected_{Rejected: &agentv1.WebSearchRequestResponse_Rejected{Reason: reason}},
		}}
	case *agentv1.InteractionQuery_AskQuestionInteractionQuery:
		r.Result = &agentv1.InteractionResponse_AskQuestionInteractionResponse{AskQuestionInteractionResponse: &agentv1.AskQuestionInteractionResponse{
			Result: &agentv1.AskQuestionResult{Result: &agentv1.AskQuestionResult_Rejected{Rejected: &agentv1.AskQuestionRejected{Reason: reason}}},
		}}
	case *agentv1.InteractionQuery_SwitchModeRequestQuery:
		r.Result = &agentv1.InteractionResponse_SwitchModeRequestResponse{SwitchModeRequestResponse: &agentv1.SwitchModeRequestResponse{
			Result: &agentv1.SwitchModeRequestResponse_Rejected_{Rejected: &agentv1.SwitchModeRequestResponse_Rejected{Reason: reason}},
		}}
	case *agentv1.InteractionQuery_CreatePlanRequestQuery:
		r.Result = &agentv1.InteractionResponse_CreatePlanRequestResponse{CreatePlanRequestResponse: &agentv1.CreatePlanRequestResponse{
			Result: &agentv1.CreatePlanResult{Result: &agentv1.CreatePlanResult_Error{Error: &agentv1.CreatePlanError{Error: reason}}},
		}}
	case *agentv1.InteractionQuery_WebFetchRequestQuery:
		r.Result = &agentv1.InteractionResponse_WebFetchRequestResponse{WebFetchRequestResponse: &agentv1.WebFetchRequestResponse{
			Result: &agentv1.WebFetchRequestResponse_Rejected_{Rejected: &agentv1.WebFetchRequestResponse_Rejected{Reason: reason}},
		}}
	case *agentv1.InteractionQuery_PrManagementRequestQuery:
		r.Result = &agentv1.InteractionResponse_PrManagementResult{PrManagementResult: &agentv1.PrManagementResult{
			Result: &agentv1.PrManagementResult_Rejected{Rejected: &agentv1.PrManagementRejected{Reason: reason}},
		}}
	case *agentv1.InteractionQuery_McpAuthRequestQuery:
		r.Result = &agentv1.InteractionResponse_McpAuthRequestResponse{McpAuthRequestResponse: &agentv1.McpAuthRequestResponse{
			Result: &agentv1.McpAuthRequestResponse_Rejected_{Rejected: &agentv1.McpAuthRequestResponse_Rejected{Reason: reason}},
		}}
	case *agentv1.InteractionQuery_GenerateImageRequestQuery:
		r.Result = &agentv1.InteractionResponse_GenerateImageRequestResponse{GenerateImageRequestResponse: &agentv1.GenerateImageRequestResponse{
			Result: &agentv1.GenerateImageRequestResponse_Rejected_{Rejected: &agentv1.GenerateImageRequestResponse_Rejected{Reason: reason}},
		}}
	case *agentv1.InteractionQuery_ReplaceEnvArgs:
		r.Result = &agentv1.InteractionResponse_ReplaceEnvResult{ReplaceEnvResult: &agentv1.ReplaceEnvResult{
			Result: &agentv1.ReplaceEnvResult_Failure{Failure: &agentv1.ReplaceEnvFailure{ErrorMessage: reason}},
		}}
	case *agentv1.InteractionQuery_ConnectScmRequestQuery:
		r.Result = &agentv1.InteractionResponse_ConnectScmRequestResponse{ConnectScmRequestResponse: &agentv1.ConnectScmRequestResponse{
			Result: &agentv1.ConnectScmRequestResponse_Rejected_{Rejected: &agentv1.ConnectScmRequestResponse_Rejected{Reason: reason}},
		}}
	}
	return &agentv1.AgentClientMessage{Message: &agentv1.AgentClientMessage_InteractionResponse{InteractionResponse: r}}
}

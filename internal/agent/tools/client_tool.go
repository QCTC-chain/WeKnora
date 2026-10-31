package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// ClientBridgeState is one ReAct round's shared external-tool-bridge state.
// The engine creates one per round and hands it to every client tool call in
// the round through ToolExecContext. When the first client tool call raises
// the batch prompt and it is answered in place (the gateway answered between
// raise and suspend), later client calls of the same round read their results
// from it instead of raising again.
type ClientBridgeState struct {
	// AnsweredBatch, when set, is an answered external_tool_call prompt
	// whose Results cover the round's calls.
	AnsweredBatch *types.AgentPrompt
}

// ClientTool is a tool the OpenAI/Anthropic gateway caller declared: the
// model calls it, but execution happens on the caller's machine. The call
// suspends the run on an AgentPromptKindExternalToolCall batch; the gateway
// translates the suspension into OpenAI tool_calls / Anthropic tool_use,
// collects the results, and resumes the run with them (M3-1).
type ClientTool struct {
	BaseTool
	// wireName is the caller-facing name; it differs from the registered
	// name only when the wire name collided with an internal tool.
	wireName string
	bridge   approval.ExternalToolCaller
}

// NewClientTool registers spec under name with the bridge; wire is the
// client-facing name (empty keeps name).
func NewClientTool(name, wire, description string, params json.RawMessage, bridge approval.ExternalToolCaller) *ClientTool {
	if wire == "" {
		wire = name
	}
	return &ClientTool{
		BaseTool: NewBaseTool(name, description, params),
		wireName: wire,
		bridge:   bridge,
	}
}

// WireName returns the caller-facing tool name.
func (t *ClientTool) WireName() string { return t.wireName }

// clientToolMarker marks this tool for the engine's batch logic.
func (t *ClientTool) clientToolMarker() {}

// IsClientTool reports whether name is registered as a gateway client tool.
func (r *ToolRegistry) IsClientTool(name string) bool {
	tool, err := r.GetTool(name)
	if err != nil {
		return false
	}
	_, ok := tool.(interface{ clientToolMarker() })
	return ok
}

// ClientToolNames lists registered client tool names in registration order.
func (r *ToolRegistry) ClientToolNames() []string {
	var names []string
	for _, tool := range r.tools {
		if _, ok := tool.(interface{ clientToolMarker() }); ok {
			names = append(names, tool.Name())
		}
	}
	return names
}

// Execute implements types.Tool: raise the batch prompt on the first client
// call of the round; pick this call's result from an answered batch.
func (t *ClientTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	meta, _ := ToolExecFromContext(ctx)
	// The bridge runs inside unattended gateway turns: the prompt it raises
	// suspends the run instead of waiting for a human. Everything else
	// (no bridge, no bus) degrades to a polite refusal the model reads.
	if meta == nil || t.bridge == nil || meta.EventBus == nil {
		return &types.ToolResult{
			Success: true,
			Output: "This tool runs on the API caller's machine and is only available through the " +
				"model gateway with tool bridging. Tell the caller you cannot use it here and continue " +
				"without it.",
		}, nil
	}

	// Resume path: the answered prompt (finishPendingRound passes it on the
	// blocked call and its later client siblings), or the round's in-place
	// answer (raised between raise and suspend), both carry one result per
	// requested call.
	answered := meta.Answered
	if answered == nil || answered.Kind != types.AgentPromptKindExternalToolCall {
		answered = meta.Bridge.AnsweredBatch
	}
	if answered != nil && answered.Kind == types.AgentPromptKindExternalToolCall {
		if r := resultFromPrompt(answered, meta.ToolCallID); r != nil {
			return externalToolResult(r), nil
		}
		// The answer exists but does not cover this call (partial results,
		// tampered row). Tell the model instead of failing the run.
		return &types.ToolResult{
			Success: false,
			Error: fmt.Sprintf(
				"The caller did not return a result for this %s call. Continue without it.",
				t.wireName,
			),
		}, nil
	}

	// First client call of the round: raise the batch prompt with every
	// pending client call so the caller gets them all in one response.
	calls := meta.ClientBatch
	if len(calls) == 0 {
		calls = []types.ExternalToolCallSpec{{
			CallID:    meta.ToolCallID,
			Name:      t.Name(),
			WireName:  t.wireName,
			Arguments: string(args),
		}}
	}
	waitCtx := ctx
	if meta.ApprovalCtx != nil {
		waitCtx = meta.ApprovalCtx
	}
	var snapshot []byte
	var snapshotVersion int
	if meta.Snapshot != nil {
		var err error
		if snapshot, snapshotVersion, err = meta.Snapshot(); err != nil {
			logger.Warnf(ctx, "[Tool][ClientCall] capture run state: %v", err)
			snapshot = nil
		}
	}
	tenantID := types.CallerFromContext(ctx).TenantID
	answer, err := t.bridge.RequestExternalToolCallAndWait(waitCtx, approval.ExternalToolRequest{
		TenantID:           tenantID,
		UserID:             meta.UserID,
		SessionID:          meta.SessionID,
		AssistantMessageID: meta.AssistantMessageID,
		RequestID:          meta.RequestID,
		ToolCallID:         meta.ToolCallID,
		EventBus:           meta.EventBus,
		Request:            types.ExternalToolCallPromptRequest{Calls: calls},
		Snapshot:           snapshot,
		SnapshotVersion:    snapshotVersion,
	})
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Handing tool calls to the caller failed: %v", err),
		}, nil
	}
	if answer.Suspended != nil {
		return &types.ToolResult{
			Success: false,
			Error:   "waiting for the API caller to execute the tool calls",
			Suspend: answer.Suspended,
		}, nil
	}
	if answer.Answered {
		// In-place answer: share it with the round's later client calls.
		if meta.Bridge != nil {
			meta.Bridge.AnsweredBatch = &types.AgentPrompt{
				ID: answer.PromptID, Kind: types.AgentPromptKindExternalToolCall,
				Response: types.JSON(mustJSON(answer.Response)),
			}
		}
		if r := resultFromAnswer(&answer, meta.ToolCallID); r != nil {
			return externalToolResult(r), nil
		}
	}
	return &types.ToolResult{
		Success: false,
		Error:   fmt.Sprintf("No result for this %s call (%s).", t.wireName, answer.Reason),
	}, nil
}

func resultFromAnswer(a *approval.ExternalToolAnswer, callID string) *types.ExternalToolCallResult {
	for i := range a.Response.Results {
		if a.Response.Results[i].CallID == callID {
			return &a.Response.Results[i]
		}
	}
	return nil
}

// resultFromPrompt picks one call's result out of an answered
// external_tool_call prompt row.
func resultFromPrompt(prompt *types.AgentPrompt, callID string) *types.ExternalToolCallResult {
	var resp types.ExternalToolCallPromptResponse
	if err := json.Unmarshal(prompt.Response, &resp); err != nil {
		return nil
	}
	for i := range resp.Results {
		if resp.Results[i].CallID == callID {
			return &resp.Results[i]
		}
	}
	return nil
}

// externalToolResult converts one client result into a tool result. The
// content is sanitized and size-capped again here (M3-2): control characters
// are stripped so a hostile result cannot smuggle context-structure tricks,
// and the cap bounds what the validator already limited when the gateway
// persisted the answer — it also keeps resumed snapshots small.
func externalToolResult(r *types.ExternalToolCallResult) *types.ToolResult {
	content := sanitizeExternalToolContent(r.Content)
	if runes := len([]rune(content)); runes > types.MaxExternalToolResultRunes {
		content = string([]rune(content)[:types.MaxExternalToolResultRunes]) + "\n…[truncated]"
	}
	if r.IsError {
		return &types.ToolResult{Success: false, Error: content}
	}
	return &types.ToolResult{Success: true, Output: content}
}

// sanitizeExternalToolContent normalizes newlines and drops control
// characters (C0 except \n and \t, plus DEL and C1) from a client-supplied
// tool result before it enters the model context.
func sanitizeExternalToolContent(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	stripped := make([]rune, 0, len(content))
	for _, r := range content {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r <= 0x9f)) {
			stripped = append(stripped, r)
		}
	}
	return strings.TrimRight(string(stripped), "\n")
}

func mustJSON(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return data
}

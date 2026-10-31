package openai_compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/hitl"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// M3-1 resume path: a follow-up request whose tool-role messages carry
// gateway call IDs answers the suspended external_tool_call prompt and
// resumes the run inside THIS request, mirroring the web resume flow
// (internal/handler/session/resume.go): settle → resolve → snapshot →
// QARequest{Resume} → run.

const (
	// bridgeSettleTimeout bounds waiting for the suspended run's teardown
	// (assistant message marked awaiting-input) before the resume may start.
	bridgeSettleTimeout = 30 * time.Second
	// bridgeSettleInterval is the settle poll cadence.
	bridgeSettleInterval = 200 * time.Millisecond
	// bridgeRunNote is appended to the resumed run's system prompt only.
	bridgeRunNote = "本次请求通过模型网关发起;客户端工具的执行结果由 API 调用方提供。"
	// bridgeResultMaxPreview is the audit-row summary length per result.
	bridgeResultMaxPreview = 120
)

// turnMessages reuses existing message rows on the resume path (nil in the
// ordinary flow: rows are created fresh).
type turnMessages struct {
	user      *types.Message
	assistant *types.Message
}

// hasBridgeToolMessages reports whether any tool message carries a gateway
// call ID (the resume trigger; legacy textualized tool messages stay on the
// ordinary render path).
func hasBridgeToolMessages(msgs []ChatMessage) bool {
	for _, msg := range msgs {
		if msg.Role == "tool" && strings.HasPrefix(msg.ToolCallID, bridgeCallIDPrefix) {
			return true
		}
	}
	return false
}

// resumeToolBridge answers the suspended prompt with the request's tool
// results and runs the resumed turn to completion (stream or aggregate).
func (h *GatewayHandler) resumeToolBridge(
	c *gin.Context, proto string, tenantID uint64, agent *types.CustomAgent,
	req *ChatCompletionRequest, started time.Time,
) {
	ctx := c.Request.Context()
	now := time.Now()

	if h.toolBridge == nil {
		writeProtoError(c, proto, http.StatusServiceUnavailable, "bridge_unavailable",
			"Tool-result resume is unavailable on this instance")
		return
	}

	// 1. Decode the gateway call IDs out of the tool messages.
	wireResults, err := decodeBridgeResultMap(toolBridgeKey(h.gateway), req.Messages, now)
	if err != nil {
		writeProtoParam(c, proto, http.StatusBadRequest, "messages", err.Error())
		return
	}

	// 2. All results in one request must answer the same suspended batch.
	promptID := ""
	for _, r := range wireResults {
		if promptID == "" {
			promptID = r.call.PromptID
		} else if r.call.PromptID != promptID {
			writeProtoParam(c, proto, http.StatusBadRequest, "messages",
				"tool results span multiple pending tool-call batches; send one batch per request")
			return
		}
	}

	// 3. Load the prompt and verify scope.
	prompt, err := h.toolBridge.Store().Get(ctx, promptID)
	if err != nil || prompt == nil || prompt.TenantID != tenantID {
		writeProtoError(c, proto, http.StatusNotFound, "tool_call_not_found",
			"The tool call this result answers no longer exists")
		return
	}
	if prompt.Kind != types.AgentPromptKindExternalToolCall {
		writeProtoError(c, proto, http.StatusConflict, "tool_call_kind",
			"The referenced prompt is not a client tool call")
		return
	}

	// 4. The session the run suspended on is authoritative; the model in the
	// request must still be the session's agent.
	session, err := h.sessions.GetSession(ctx, prompt.SessionID)
	if err != nil || session == nil || session.TenantID != tenantID {
		writeProtoError(c, proto, http.StatusNotFound, "session_not_found",
			"The session behind this tool call no longer exists")
		return
	}
	if session.Kind != types.SessionKindGateway {
		writeProtoError(c, proto, http.StatusConflict, "session_kind",
			"Tool results can only resume model-gateway sessions")
		return
	}
	if session.LastRequestState == nil || session.LastRequestState.AgentID != agent.ID {
		writeProtoError(c, proto, http.StatusConflict, "model_mismatch",
			"The tool call was raised by a different model; results must go back to the same model")
		return
	}

	// 5. Answer completeness: exactly one result per requested call.
	var promptReq types.ExternalToolCallPromptRequest
	if err := json.Unmarshal(prompt.Request, &promptReq); err != nil {
		writeProtoError(c, proto, http.StatusInternalServerError, "internal_error",
			"The suspended tool-call batch is unreadable")
		return
	}
	response := types.ExternalToolCallPromptResponse{
		Results: make([]types.ExternalToolCallResult, 0, len(promptReq.Calls)),
	}
	var missing []string
	for _, call := range promptReq.Calls {
		result, ok := wireResults.byInternalID(call.CallID)
		if !ok {
			missing = append(missing, call.WireToolName())
			continue
		}
		response.Results = append(response.Results, types.ExternalToolCallResult{
			CallID:  call.CallID,
			Content: result.content,
			IsError: result.isError,
		})
	}
	if len(missing) > 0 {
		writeProtoParam(c, proto, http.StatusBadRequest, "messages",
			fmt.Sprintf("missing results for %d tool call(s): %s",
				len(missing), strings.Join(missing, ", ")))
		return
	}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		writeProtoError(c, proto, http.StatusInternalServerError, "internal_error",
			"Failed to encode the tool results")
		return
	}

	// 6. Serialize: per-key slot, then the session turn lock.
	release, ok := h.acquireTurnSlot(ctx, tenantID)
	if !ok {
		gatewayMetrics.observeConcurrencyLimited()
		writeProtoError(c, proto, http.StatusTooManyRequests, "concurrency_limit",
			"Too many concurrent requests for this API key; retry after the running requests finish")
		return
	}
	defer release()
	if !h.turnLocks.tryAcquire(session.ID) {
		c.Header("Retry-After", "5")
		writeProtoError(c, proto, http.StatusConflict, "session_busy",
			"A turn is already running in session "+session.ID+"; retry after it finishes")
		return
	}
	defer h.turnLocks.release(session.ID)

	// 7. Answer the prompt. The broker re-validates tenant/kind/expiry and
	// the response shape.
	principal, _ := types.PrincipalFromContext(ctx)
	resolution, err := h.toolBridge.Resolve(ctx, hitl.ResolveRequest{
		TenantID: tenantID, UserID: principal.StorageID(), SessionID: prompt.SessionID,
		Kind: types.AgentPromptKindExternalToolCall, PromptID: prompt.ID, Response: responseJSON,
	})
	if err != nil {
		h.writeResolveError(c, proto, err)
		return
	}
	if !resolution.Resume {
		writeProtoError(c, proto, http.StatusConflict, "prompt_not_suspended",
			"The tool call is no longer waiting on results")
		return
	}
	prompt = resolution.Prompt
	stopLease := h.toolBridge.KeepLease(ctx, prompt.ID, prompt.OwnerInstance)
	defer stopLease()

	// 8. Wait for the suspended run to finish writing its message, then
	// load the run snapshot.
	if err := h.awaitBridgeSettled(ctx, prompt); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"prompt_id": prompt.ID})
		writeProtoError(c, proto, http.StatusConflict, "run_not_settled",
			"The suspended run did not settle in time; retry shortly")
		return
	}
	state, err := h.toolBridge.Suspensions().Get(ctx, prompt.AssistantMessageID)
	if err != nil || state == nil || state.PromptID != prompt.ID {
		writeProtoError(c, proto, http.StatusConflict, "run_state_missing",
			"No suspended run state was saved for this tool call")
		return
	}
	snapshot, err := h.restoreBridgeUserMessages(ctx, prompt.SessionID, state.Snapshot)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"prompt_id": prompt.ID})
		writeProtoError(c, proto, http.StatusInternalServerError, "internal_error",
			"Failed to restore the suspended run state")
		return
	}
	var rreq types.ResumableAgentRequest
	if err := agentrun.DecodeResumableRequest(snapshot, &rreq); err != nil {
		writeProtoError(c, proto, http.StatusConflict, "run_state_invalid",
			"The suspended run state cannot be restored: "+err.Error())
		return
	}

	// 9. Reopen the assistant message for the resumed run.
	msg, err := h.messages.GetMessage(ctx, prompt.SessionID, prompt.AssistantMessageID)
	if err != nil || msg == nil {
		writeProtoError(c, proto, http.StatusConflict, "message_missing",
			"The suspended assistant message no longer exists")
		return
	}
	if msg.IsCompleted || msg.RunState != types.MessageRunStateAwaitingInput {
		writeProtoError(c, proto, http.StatusConflict, "message_not_awaiting",
			"The suspended run is not waiting for input anymore")
		return
	}
	msg.RunState = ""
	msg.UpdatedAt = time.Now()
	if err := h.messages.UpdateMessage(ctx, msg); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"message_id": msg.ID})
		writeProtoError(c, proto, http.StatusInternalServerError, "internal_error",
			"Failed to reopen the suspended run")
		return
	}

	// 10. Client tools for the resumed run: the resuming request may
	// re-declare them; otherwise fall back to the frozen definitions.
	clientTools, toolErr := validateClientTools(req.Tools)
	if toolErr != nil {
		writeProtoParam(c, proto, http.StatusBadRequest, "tools", toolErr.Error())
		return
	}
	if agent.Config.GatewayToolPolicy == "none" {
		clientTools = nil
	}
	if len(clientTools) == 0 {
		clientTools = rreq.ClientTools
	}

	// 11. Rebuild the request the suspended run saved and continue it in
	// this request.
	runCtx := ctx
	if rreq.Language != "" {
		runCtx = context.WithValue(ctx, types.LanguageContextKey, rreq.Language)
	}
	qa := &types.QARequest{
		Session:             session,
		Query:               rreq.Query,
		AssistantMessageID:  prompt.AssistantMessageID,
		UserMessageID:       rreq.UserMessageID,
		CustomAgent:         agent,
		SummaryModelID:      rreq.SummaryModelID,
		ReasoningEffort:     rreq.ReasoningEffort,
		SharedAgentReadOnly: rreq.SharedAgentReadOnly,
		KnowledgeBaseIDs:    rreq.KnowledgeBaseIDs,
		KnowledgeIDs:        rreq.KnowledgeIDs,
		TagScopes:           rreq.TagScopes,
		MCPServiceIDs:       rreq.MCPServiceIDs,
		SkillNames:          rreq.SkillNames,
		// The gateway's own default, same as ordinary turns.
		WebSearchEnabled: true,
		QuestionOrigin:   rreq.QuestionOrigin,
		PageContext:      rreq.PageContext,
		AssistantAccess:  rreq.AssistantAccess,
		TurnLeaseHeld:    false,
		Suspendable:      true,
		Mode:             "",
		RunNote:          bridgeRunNote,
		Resume:           &types.AgentResume{Snapshot: snapshot, Prompt: prompt},
		ClientTools:      clientTools,
	}
	turn := &renderedTurn{Query: rreq.Query, ClientTools: clientTools}
	tm := &turnMessages{assistant: msg}
	if rreq.UserMessageID != "" {
		if userMsg, uErr := h.messages.GetMessage(ctx, session.ID, rreq.UserMessageID); uErr == nil && userMsg != nil {
			tm.user = userMsg
		}
	}

	auditBase := gatewayTurnAudit{
		TenantID: tenantID, UserID: session.UserID,
		KeyID: gatewayKeyID(ctx), KeyName: gatewayKeyName(ctx),
		AgentID: agent.ID, Model: req.Model, Stream: req.Stream,
		Query:     "client tool results: " + summarizeBridgeResults(response.Results),
		SessionID: session.ID, StatusCode: http.StatusOK,
	}
	c.Header("X-WeKnora-Agent-Id", agent.ID)
	c.Header("X-WeKnora-Session-Id", session.ID)
	c.Header("X-WeKnora-Gateway-Version", GatewayVersion)

	var endReason string
	var usage *Usage
	var telemetry *turnTelemetry
	if req.Stream {
		stream := protoStream(c, proto, req.Model)
		stream.setHeaders(agent.ID, session.ID)
		endReason, usage, telemetry = h.runGatewayTurnStream(runCtx, session, agent, turn, stream, tm, qa)
	} else {
		outcome := h.runGatewayTurn(runCtx, session, agent, turn, tm, qa)
		auditBase.Telemetry = outcome.telemetry
		if outcome.err != nil {
			auditBase.EndReason = "error"
			auditBase.StatusCode = http.StatusInternalServerError
			auditBase.Duration = time.Since(started)
			observeTurnMetrics(proto, nil, outcome.telemetry, started)
			h.auditGatewayTurn(ctx, auditBase)
			writeProtoError(c, proto, http.StatusInternalServerError, "internal_error",
				"Agent run failed: "+outcome.err.Error())
			return
		}
		endReason = timeoutEndReason(outcome.timedOut)
		usage = outcome.usage
		telemetry = outcome.telemetry
		if outcome.bridge != nil {
			meta := h.completionsMeta(agent, session, turn)
			meta.BridgePending = true
			endReason = FinishToolCalls
			h.writeBridgeResponse(c, proto, req.Model, outcome, meta)
		} else if proto == protoAnthropic {
			respondAnthropicMessage(c, req.Model, outcome.content, outcome.usage)
		} else {
			c.JSON(http.StatusOK, ChatCompletionResponse{
				ID: "chatcmpl-" + uuid.New().String(), Object: "chat.completion",
				Created: time.Now().Unix(), Model: req.Model,
				Choices: []ChatCompletionChoice{{
					Index: 0, Message: &RespMessage{Role: "assistant", Content: outcome.content},
					FinishReason: FinishStop,
				}},
				Usage: outcome.usage,
				Meta:  h.completionsMeta(agent, session, turn),
			})
		}
	}

	auditBase.EndReason = endReason
	auditBase.Usage = usage
	auditBase.Telemetry = telemetry
	auditBase.Duration = time.Since(started)
	observeTurnMetrics(proto, usage, telemetry, started)
	switch endReason {
	case "timeout", "disconnect":
		auditBase.StatusCode = http.StatusRequestTimeout
	}
	h.auditGatewayTurn(ctx, auditBase)
}

// writeResolveError maps broker resolve failures to protocol errors.
func (h *GatewayHandler) writeResolveError(c *gin.Context, proto string, err error) {
	switch {
	case errors.Is(err, hitl.ErrPromptNotFound), errors.Is(err, hitl.ErrTenantMismatch):
		writeProtoError(c, proto, http.StatusNotFound, "tool_call_not_found",
			"The tool call this result answers no longer exists")
	case errors.Is(err, hitl.ErrUserMismatch):
		writeProtoError(c, proto, http.StatusForbidden, "tool_call_owner",
			"Tool results must be sent with the API key that raised the tool call")
	case errors.Is(err, hitl.ErrAlreadyResolved):
		writeProtoError(c, proto, http.StatusConflict, "already_answered",
			"This tool call was already answered")
	case errors.Is(err, hitl.ErrInvalidResponse):
		writeProtoParam(c, proto, http.StatusBadRequest, "messages",
			"The tool results were rejected: "+err.Error())
	default:
		writeProtoError(c, proto, http.StatusInternalServerError, "internal_error",
			"Failed to record the tool results")
	}
}

// awaitBridgeSettled polls until the suspended run has finished writing its
// assistant message (marked awaiting-input), like the web resume's settle
// loop minus the live-run marker (gateway runs never register one).
func (h *GatewayHandler) awaitBridgeSettled(ctx context.Context, prompt *types.AgentPrompt) error {
	deadline := time.Now().Add(bridgeSettleTimeout)
	for {
		msg, err := h.messages.GetMessage(ctx, prompt.SessionID, prompt.AssistantMessageID)
		if err != nil {
			return fmt.Errorf("load assistant message: %w", err)
		}
		if msg == nil || msg.IsCompleted {
			return fmt.Errorf("assistant message is no longer open")
		}
		if msg.RunState == types.MessageRunStateAwaitingInput {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the suspended run did not finish within %s", bridgeSettleTimeout)
		}
		time.Sleep(bridgeSettleInterval)
	}
}

// restoreBridgeUserMessages resolves steering-message IDs the snapshot only
// stored by reference (same rule as the web resume path).
func (h *GatewayHandler) restoreBridgeUserMessages(
	ctx context.Context, sessionID string, snapshot []byte,
) ([]byte, error) {
	snap, err := agentrun.DecodeRunSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	if snap.UserMessages == nil {
		snap.UserMessages = make(map[string]string)
	}
	steps := append(append([]types.AgentStep(nil), snap.CompletedSteps...), snap.PendingRound.Step)
	for _, step := range steps {
		for _, id := range step.UserMessagesBefore {
			if _, ok := snap.UserMessages[id]; ok {
				continue
			}
			msg, err := h.messages.GetMessage(ctx, sessionID, id)
			if err != nil {
				return nil, fmt.Errorf("load steered message %s: %w", id, err)
			}
			if msg == nil || msg.Role != "user" || msg.SessionID != sessionID {
				return nil, fmt.Errorf("steered message %s is not a user message of this session", id)
			}
			snap.UserMessages[id] = msg.Content
		}
	}
	return json.Marshal(snap)
}

// summarizeBridgeResults builds the audit-row query summary.
func summarizeBridgeResults(results []types.ExternalToolCallResult) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		text := strings.ReplaceAll(strings.TrimSpace(r.Content), "\n", " ")
		if runes := []rune(text); len(runes) > bridgeResultMaxPreview {
			text = string(runes[:bridgeResultMaxPreview]) + "…"
		}
		if r.IsError {
			text = "error: " + text
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " | ")
}

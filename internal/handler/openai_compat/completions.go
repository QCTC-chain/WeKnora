package openai_compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// gatewayChannel tags sessions and messages created by the gateway so the
// audit trail can tell them from web/IM/MCP traffic.
const gatewayChannel = "gateway"

// gatewaySessionTitleMaxRunes bounds the temporary session title.
const gatewaySessionTitleMaxRunes = 60

// DefaultRequestTimeout bounds one non-streaming completion unless
// gateway.request_timeout overrides it (docs §9).
const DefaultRequestTimeout = 600 * time.Second

// StreamingNotImplementedCode is obsolete since G3: stream=true now streams.
// The constant remains only as a machine-readable marker for tests and
// tooling; the handler no longer emits it.
const StreamingNotImplementedCode = "streaming_not_implemented"

// ignored sampling params are echoed in meta so clients can see what the
// gateway dropped (docs §4.2).
var ignoredSamplingParams = []string{"temperature", "top_p", "max_tokens", "reasoning_effort", "stop", "n", "logprobs"}

// ChatCompletions godoc
// @Summary      网关对话补全
// @Description  OpenAI 兼容端点:把一次 chat/completions 请求作为单轮提问发给
// @Description  指定的已发布智能体。messages 数组整体折叠为一轮 query(调用方指令 +
// @Description  对话历史 + 本次问题),每次调用创建 Kind=gateway 的临时会话,
// @Description  跑完留在库中供审计(默认 72h 后由回收任务清理)。tools[] 中的
// @Description  function 定义按智能体 gateway_tool_policy 处理:默认桥接为客户端
// @Description  工具(模型调用时以 finish_reason="tool_calls" 挂起,续轮带 tool
// @Description  消息恢复运行),策略为 none 时计数忽略并回显 meta.client_tools_ignored;
// @Description  temperature 等采样参数以智能体配置为准。
// @Description  stream=true 走 SSE(chat.completion.chunk + [DONE]),
// @Description  首个内容前每 15s 输出 keep-alive 注释行;stream=false 聚合返回。
// @Description  每 key 并发默认上限 3,超限立即 429(concurrency_limit)。
// @Tags         模型网关
// @Accept       json
// @Produce      json
// @Produce      text/event-stream
// @Param        request  body  ChatCompletionRequest  true  "对话补全请求"
// @Success      200  {object}  ChatCompletionResponse  "补全结果(非流式)"
// @Failure      400  {object}  ErrorEnvelope  "请求不合法"
// @Failure      404  {object}  ErrorEnvelope  "model 不存在或未发布"
// @Failure      429  {object}  ErrorEnvelope  "每 key 并发超限"
// @Failure      500  {object}  ErrorEnvelope  "智能体运行失败"
// @Security     ApiKeyAuth
// @Router       /v1/chat/completions [post]
func (h *GatewayHandler) ChatCompletions(c *gin.Context) {
	h.handleCompletions(c, protoOpenAI)
}

// protoOpenAI discriminates the OpenAI shell in the shared handler.
const protoOpenAI = "openai"

// handleCompletions is the shared completion pipeline for both protocol
// shells (M1 OpenAI /v1/chat/completions, M2-5 Anthropic /v1/messages):
// identical resolution, grants, budgets, idempotency, threading, turn
// execution, audit and metering; only request parsing and response/error
// encoding differ.
func (h *GatewayHandler) handleCompletions(c *gin.Context, proto string) {
	started := time.Now()
	ctx := c.Request.Context()
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		logger.Error(ctx, "Gateway ChatCompletions called without tenant context")
		writeError(c, http.StatusInternalServerError, "internal_error", "Workspace context missing")
		return
	}

	// M2-4: keep the raw body for the idempotency fingerprint, then restore
	// it for the bind (ShouldBindJSON consumes the reader).
	rawBody, readErr := io.ReadAll(c.Request.Body)
	if readErr != nil {
		writeErrorParam(c, http.StatusBadRequest, "", "body", "Failed to read request body: "+readErr.Error())
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	req, parseErr := parseCompletionsRequest(rawBody, proto)
	if parseErr != nil {
		writeProtoParam(c, proto, http.StatusBadRequest, "body", "Invalid request body: "+parseErr.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeProtoParam(c, proto, http.StatusBadRequest, "model", "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeProtoParam(c, proto, http.StatusBadRequest, "messages", "messages must not be empty")
		return
	}

	agent, ok := h.resolveAgent(ctx, tenantID, req.Model)
	if !ok {
		writeProtoError(c, proto, http.StatusNotFound, "model_not_found",
			"The model '"+req.Model+"' does not exist or is not published in this workspace")
		gatewayMetrics.observeModelNotFound(req.Model)
		return
	}
	// M2-b per-key grants: a restricted key may only call its listed agents
	// (404, same as unpublished — the whitelist is not worth leaking).
	if !agentGranted(ctx, agent.ID) {
		writeProtoError(c, proto, http.StatusNotFound, "model_not_found",
			"The model '"+req.Model+"' is not granted to this API key")
		gatewayMetrics.observeModelNotFound(req.Model)
		return
	}

	// M2-b budgets: entry-time probe against key + tenant daily budgets.
	if decision := h.checkGatewayBudget(ctx, tenantID); !decision.ok {
		gatewayMetrics.observeBudgetExceeded(decision.limit)
		writeProtoError(c, proto, http.StatusTooManyRequests, "budget_exceeded", decision.reason+" ("+budgetResetHint()+")")
		return
	}

	// M3-1 client-tool bridge (design doc agent-model-gateway-m2-m3.md §7):
	// policy "none" keeps the M1 behavior (tools ignored); otherwise the
	// caller's function tools are bridged into the run, and a follow-up
	// whose tool messages carry gateway call IDs resumes the suspended run
	// instead of starting a new turn.
	bridgeEnabled := agent.Config.GatewayToolPolicy != "none"
	clientTools, toolErr := validateClientTools(req.Tools)
	if toolErr != nil {
		writeProtoParam(c, proto, http.StatusBadRequest, "tools", toolErr.Error())
		return
	}
	if !bridgeEnabled {
		clientTools = nil
	}
	if bridgeEnabled && hasBridgeToolMessages(req.Messages) {
		h.resumeToolBridge(c, proto, tenantID, agent, req, started)
		return
	}

	turn, err := renderMessages(req.Messages, agent.Config.ImageUploadEnabled)
	if err != nil {
		writeProtoParam(c, proto, http.StatusBadRequest, "messages", err.Error())
		return
	}
	turn.ClientTools = clientTools

	// Per-key concurrency (docs §9): over-limit requests answer 429
	// immediately, before any session or message rows are created.
	release, ok := h.acquireTurnSlot(ctx, tenantID)
	if !ok {
		gatewayMetrics.observeConcurrencyLimited()
		writeProtoError(c, proto, http.StatusTooManyRequests, "concurrency_limit",
			"Too many concurrent requests for this API key; retry after the running requests finish")
		return
	}
	defer release()

	// M2-3 session continuation: explicit header wins, then the
	// (owner|key, agent) derived thread; miss falls back to a fresh session.
	thread, ok := h.resolveGatewayThread(ctx, c, tenantID, agent)
	if !ok {
		writeProtoError(c, proto, http.StatusNotFound, "session_not_found",
			"The session referenced by "+SessionIDHeader+" does not exist in this workspace, is not a gateway session, or belongs to another agent")
		return
	}
	// M2-4 idempotency: claim AFTER thread 404/409 exits (those must not be
	// replayed) and BEFORE session creation/execution. From here the
	// response is captured for replay.
	claim, replay, handled := h.claimIdempotency(ctx, c, tenantID, rawBody)
	if handled || replay {
		return
	}
	var capture *capturingResponseWriter
	if claim != nil {
		capture = &capturingResponseWriter{ResponseWriter: c.Writer}
		c.Writer = capture
		defer h.finishIdempotency(ctx, claim, capture)
	}

	var session *types.Session
	if thread.session != nil {
		session = thread.session
		// Design §3 conflict handling: one live run per session; concurrent
		// sends answer 409 immediately.
		if !h.turnLocks.tryAcquire(session.ID) {
			c.Header("Retry-After", "5")
			writeProtoError(c, proto, http.StatusConflict, "session_busy",
				"A turn is already running in session "+session.ID+"; retry after it finishes, change the user field, or send "+SessionIDHeader+": new")
			return
		}
		defer h.turnLocks.release(session.ID)
	} else {
		created, err := h.createGatewaySession(ctx, tenantID, agent, req.Model)
		if err != nil {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{"tenant_id": tenantID})
			h.releaseIdempotency(ctx, claim)
			claim = nil
			writeProtoError(c, proto, http.StatusInternalServerError, "internal_error", "Failed to create gateway session")
			return
		}
		session = created
	}

	// Continued threads only send the newest user message; WeKnora session
	// history supplies the rest (design §3 continuation semantics).
	if thread.continued {
		lastUser := lastUserMessageIndex(req.Messages)
		if lastUser >= 0 {
			if trimmed, err := renderMessages(req.Messages[lastUser:], agent.Config.ImageUploadEnabled); err == nil {
				turn = trimmed
			}
		}
		// Sliding retention: a continued thread stays alive.
		if err := h.sessions.TouchGatewaySession(ctx, session.ID); err != nil {
			logger.Warnf(ctx, "[gateway] touch gateway session failed: session=%s err=%v", session.ID, err)
		}
	}

	meta := &CompletionMeta{
		Warnings:      turn.Warnings,
		IgnoredParams: ignoredSamplingParams,
		AgentID:       agent.ID,
		SessionID:     session.ID,
	}
	if !bridgeEnabled && len(req.Tools) > 0 {
		meta.ClientToolsIgnored = len(req.Tools)
	}

	auditBase := gatewayTurnAudit{
		TenantID:   tenantID,
		UserID:     session.UserID,
		KeyID:      gatewayKeyID(ctx),
		KeyName:    gatewayKeyName(ctx),
		AgentID:    agent.ID,
		Model:      req.Model,
		Stream:     req.Stream,
		Query:      turn.Query,
		Warnings:   meta.Warnings,
		SessionID:  session.ID,
		StatusCode: http.StatusOK,
	}

	c.Header("X-WeKnora-Agent-Id", agent.ID)
	c.Header("X-WeKnora-Session-Id", session.ID)
	c.Header("X-WeKnora-Gateway-Version", GatewayVersion)
	if agent.GatewayDeprecated(time.Now()) {
		// M2-a deprecated transition: keep serving, but tell clients the
		// model is scheduled for removal (same idea as RFC 8594 Sunshine).
		c.Header("X-WeKnora-Deprecation", "sunset="+agent.GatewaySunset.UTC().Format(time.RFC3339))
	}

	if req.Stream {
		stream := protoStream(c, proto, req.Model)
		stream.setHeaders(agent.ID, session.ID)
		endReason, usage, telemetry := h.runGatewayTurnStream(ctx, session, agent, turn, stream, nil, nil)
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
		return
	}

	outcome := h.runGatewayTurn(ctx, session, agent, turn, nil, nil)
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
	if outcome.timedOut {
		meta.Warnings = append(meta.Warnings, "[网关] 响应超时,返回已产生的部分内容")
	}

	auditBase.EndReason = timeoutEndReason(outcome.timedOut)
	auditBase.Usage = outcome.usage
	auditBase.Duration = time.Since(started)
	observeTurnMetrics(proto, outcome.usage, outcome.telemetry, started)
	if outcome.bridge != nil {
		// M3-1: the run suspended on client tools; answer tool_calls and
		// leave the assistant message open (it is awaiting input).
		auditBase.EndReason = FinishToolCalls
		meta.BridgePending = true
		h.auditGatewayTurn(ctx, auditBase)
		h.writeBridgeResponse(c, proto, req.Model, outcome, meta)
		return
	}
	h.auditGatewayTurn(ctx, auditBase)

	if proto == protoAnthropic {
		respondAnthropicMessage(c, req.Model, outcome.content, outcome.usage)
		return
	}
	c.JSON(http.StatusOK, ChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.New().String(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []ChatCompletionChoice{{
			Index:        0,
			Message:      &RespMessage{Role: "assistant", Content: outcome.content},
			FinishReason: FinishStop,
		}},
		Usage: outcome.usage,
		Meta:  meta,
	})
}

// timeoutEndReason maps the non-stream outcome to an audit end reason.
func timeoutEndReason(timedOut bool) string {
	if timedOut {
		return "timeout"
	}
	return "completed"
}

// acquireTurnSlot takes the per-key concurrency slot. The key identity comes
// from the GatewayAuth-attached API key scope; requests without one (tests,
// misconfigured middleware) are counted under the workspace id.
func (h *GatewayHandler) acquireTurnSlot(ctx context.Context, tenantID uint64) (func(), bool) {
	key := tenantID
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok && scope.KeyID > 0 {
		key = scope.KeyID
	}
	max := h.gateway.EffectiveMaxConcurrencyPerKey()
	release := h.concurrency.tryAcquire(key, max)
	if release == nil {
		return nil, false
	}
	return release, true
}

// gatewayKeyID extracts the credential fingerprint for audit details.
func gatewayKeyID(ctx context.Context) uint64 {
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		return scope.KeyID
	}
	return 0
}

// gatewayKeyName extracts the credential display name for audit details.
func gatewayKeyName(ctx context.Context) string {
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		return scope.Name
	}
	return ""
}

// turnOutcome aggregates one gateway run for the non-stream response.
type turnOutcome struct {
	content   string
	usage     *Usage
	err       error
	timedOut  bool
	telemetry *turnTelemetry
	// bridge is set when the run suspended on client-tool calls (M3-1);
	// the response becomes finish_reason "tool_calls" instead.
	bridge *bridgeSuspension
}

// runGatewayTurn creates the message rows, drives SessionService.AgentQA to
// completion on the shared event vocabulary, and aggregates the final answer
// plus references. It mirrors the MCP ask tool's synchronous path
// (internal/mcpserver/tools_ask.go runQA) — same service entry, same
// unattended semantics, TurnLeaseHeld=false.
func (h *GatewayHandler) runGatewayTurn(
	ctx context.Context, session *types.Session, agent *types.CustomAgent,
	turn *renderedTurn, tm *turnMessages, qa *types.QARequest,
) turnOutcome {
	userMsg, assistantMsg, err := h.createTurnMessages(ctx, session, turn, tm)
	if err != nil {
		return turnOutcome{err: err}
	}

	var (
		mu          sync.Mutex
		answer      strings.Builder
		refs        []*types.SearchResult
		finalAnswer string
		usage       *Usage
		qaErr       error
		promptNote  string
		rounds      int
		// M3-1 bridge capture: the external_tool_call prompt the run raised
		// plus the suspension event confirming the run parked on it.
		bridgePrompt   *types.ExternalToolCallPromptRequest
		bridgePromptID string
		bridgeSuspend  *types.PromptSuspension
		done           = make(chan struct{})
		complete       = make(chan struct{})
		doneOnce       sync.Once
		compOnce       sync.Once
	)
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	closeComplete := func() { compOnce.Do(func() { close(complete) }) }

	bus := event.NewEventBus()
	bus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		mu.Lock()
		answer.WriteString(data.Content)
		if data.Done {
			closeDone()
		}
		mu.Unlock()
		return nil
	})
	bus.On(event.EventAgentReferences, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentReferencesData)
		if !ok {
			return nil
		}
		mu.Lock()
		refs = appendReferences(refs, data.References)
		mu.Unlock()
		return nil
	})
	bus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentCompleteData)
		if !ok {
			return nil
		}
		mu.Lock()
		if data.FinalAnswer != "" {
			finalAnswer = data.FinalAnswer
		}
		if len(data.KnowledgeRefs) > 0 {
			refs = appendReferences(refs, data.KnowledgeRefs)
		}
		if tu, ok := data.Usage.(*types.TokenUsage); ok && tu != nil {
			usage = &Usage{
				PromptTokens:     int64(tu.PromptTokens),
				CompletionTokens: int64(tu.CompletionTokens),
				TotalTokens:      int64(tu.TotalTokens),
			}
		}
		rounds = data.TotalSteps
		closeComplete()
		mu.Unlock()
		return nil
	})
	bus.On(event.EventError, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.ErrorData)
		if !ok {
			return nil
		}
		mu.Lock()
		qaErr = errors.New(data.Error)
		closeDone()
		closeComplete()
		mu.Unlock()
		return nil
	})
	bus.On(event.EventAgentPromptRequired, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentPromptRequiredData)
		if !ok {
			return nil
		}
		mu.Lock()
		if data.Kind == types.AgentPromptKindExternalToolCall {
			// M3-1: a client-tool call the gateway forwards to the caller.
			if req, err := decodeBridgePromptRequest(data.Request); err == nil {
				bridgePrompt = req
				bridgePromptID = data.PendingID
			}
		} else {
			promptNote = fmt.Sprintf("[提示] 智能体请求人工输入(%s),网关以无人值守模式继续。", data.Kind)
		}
		mu.Unlock()
		return nil
	})
	bus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.AgentCompleteData); ok {
			if susp, ok := data.Suspended.(*types.PromptSuspension); ok && susp != nil {
				mu.Lock()
				bridgeSuspend = susp
				mu.Unlock()
			}
		}
		return nil
	})

	timeout := h.gateway.EffectiveRequestTimeout()
	runParent := ctx
	if !h.gateway.EffectiveCancelOnDisconnect() {
		// Docs §8: let the run finish even when the caller hangs up —
		// already-spent tokens are not wasted.
		runParent = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithTimeout(runParent, timeout)
	defer cancel()

	onReturn := func(runErr error) {
		mu.Lock()
		if qaErr == nil {
			qaErr = runErr
		}
		closeDone()
		closeComplete()
		mu.Unlock()
	}
	qaFinished := h.launchAgentQA(runCtx, session, agent, turn, userMsg, assistantMsg, bus, onReturn, qa)

	timedOut := false
	select {
	case <-done:
		// Agent mode: answer events arrived; give the completion event a
		// grace window for usage/references (same as the MCP caller).
		timer := time.NewTimer(askCompleteWait)
		select {
		case <-complete:
		case <-runCtx.Done():
		case <-timer.C:
			logger.Warnf(ctx, "[gateway] timed out waiting for agent completion: session=%s", session.ID)
		}
		timer.Stop()
	case <-complete:
	case <-qaFinished:
		// AgentQA returned without emitting answer/complete events: fall
		// through to the empty-answer handling instead of burning budget.
	case <-runCtx.Done():
		timedOut = true
	}

	mu.Lock()
	text := answer.String()
	if strings.TrimSpace(text) == "" && finalAnswer != "" {
		text = finalAnswer
	}
	collected := refs
	runErr := qaErr
	note := promptNote
	var bridge *bridgeSuspension
	if bridgeSuspend != nil && bridgePrompt != nil &&
		bridgeSuspend.PromptID == bridgePromptID && bridgePromptID != "" {
		bridge = &bridgeSuspension{
			PromptID:  bridgePromptID,
			Calls:     bridgePrompt.Calls,
			ExpiresAt: bridgeSuspend.ExpiresAt,
		}
	}
	mu.Unlock()

	if bridge != nil {
		// M3-1: no answer text is owed; the tool_calls response ends this
		// request. The assistant message stays open (awaiting input).
		return turnOutcome{usage: usage, bridge: bridge,
			telemetry: &turnTelemetry{Rounds: rounds}}
	}

	if strings.TrimSpace(text) == "" && note != "" {
		text = note
	}
	if timedOut {
		// Best effort partial answer before giving up (docs §9).
		if strings.TrimSpace(text) != "" {
			text += "\n[网关] 响应超时,以上为已产生的部分内容。"
			return turnOutcome{content: text, usage: usage, timedOut: true,
				telemetry: &turnTelemetry{Rounds: rounds, AnswerRunes: len([]rune(text))}}
		}
		return turnOutcome{err: errors.New("gateway request timeout"),
			telemetry: &turnTelemetry{Rounds: rounds}}
	}
	if strings.TrimSpace(text) == "" && runErr != nil {
		finalizeGatewayMessage(ctx, h.messages, assistantMsg, "The answer failed: "+runErr.Error(), nil)
		return turnOutcome{err: runErr, telemetry: &turnTelemetry{Rounds: rounds}}
	}
	if strings.TrimSpace(text) == "" {
		text = "No answer could be produced for this question."
	}

	// Citations ride inside the text: OpenAI clients have no reference
	// channel, so the source list is appended when the agent cites. Nil keeps
	// the pre-citation default (enabled), preserving legacy agent configs.
	if agent.Config.CitationEnabled == nil || *agent.Config.CitationEnabled {
		if len(collected) > 0 {
			text += "\n" + renderCitations(collected)
		}
	}

	content := text
	finalizeGatewayMessage(ctx, h.messages, assistantMsg, content, collected)
	return turnOutcome{content: content, usage: usage, telemetry: &turnTelemetry{
		Rounds: rounds, AnswerRunes: len([]rune(content))}}
}

// askCompleteWait is the grace window between the answer completing and the
// completion event (usage/references) arriving — same idea as the MCP caller.
const askCompleteWait = 30 * time.Second

// createTurnMessages persists the user + assistant rows of one gateway turn.
// Both completions paths (aggregate and stream) share this pair.
func (h *GatewayHandler) createTurnMessages(
	ctx context.Context, session *types.Session, turn *renderedTurn, tm *turnMessages,
) (*types.Message, *types.Message, error) {
	if tm != nil {
		// Resume path: the rows already exist; only the missing user row is
		// synthesized (runs suspended before any steering message arrived).
		if tm.user == nil && tm.assistant != nil {
			userMsg, err := h.messages.CreateMessage(ctx, &types.Message{
				SessionID: session.ID, Role: "user", Content: turn.Query,
				RequestID: tm.assistant.RequestID, CreatedAt: time.Now(),
				IsCompleted: true, Channel: gatewayChannel,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("create user message: %w", err)
			}
			tm.user = userMsg
		}
		return tm.user, tm.assistant, nil
	}
	requestID := uuid.New().String()
	userMsg, err := h.messages.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     turn.Query,
		RequestID:   requestID,
		CreatedAt:   time.Now(),
		IsCompleted: true,
		Channel:     gatewayChannel,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create user message: %w", err)
	}
	assistantMsg, err := h.messages.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "assistant",
		RequestID:   requestID,
		CreatedAt:   time.Now(),
		IsCompleted: false,
		Channel:     gatewayChannel,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create assistant message: %w", err)
	}
	return userMsg, assistantMsg, nil
}

// launchAgentQA starts the SessionService.AgentQA goroutine. Ordinary turns
// use the shared unattended semantics (docs §7.1): TurnLeaseHeld=false so the
// service takes the session lease itself, Suspendable=false, Mode="" — the
// same shape as the IM and MCP callers. M3-1 bridge turns (turn.ClientTools)
// flip Suspendable on so a client-tool call can park the run for the caller;
// the qa override (resume) replaces the built request entirely. onReturn
// receives AgentQA's own return error (bus-level failures travel as error
// events instead). The returned channel closes when AgentQA returns — with
// or without events — so a run that finishes silently unblocks the waiters
// instead of burning the whole request budget.
func (h *GatewayHandler) launchAgentQA(
	ctx context.Context,
	session *types.Session,
	agent *types.CustomAgent,
	turn *renderedTurn,
	userMsg, assistantMsg *types.Message,
	bus *event.EventBus,
	onReturn func(error),
	qa *types.QARequest,
) <-chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var req *types.QARequest
		if qa != nil {
			// M3-1 resume: the caller prepared the full resumed request
			// (Resume snapshot, frozen tools, session) already.
			req = qa
		} else {
			req = &types.QARequest{
				Session:            session,
				Query:              turn.Query,
				AssistantMessageID: assistantMsg.ID,
				UserMessageID:      userMsg.ID,
				CustomAgent:        agent,
				// The agent's own KB/MCP/skill selection governs; the gateway
				// adds no scope of its own.
				ImageURLs: turn.Images,
				// ANDed with the agent config inside the service, so "true" here
				// means "let the agent's own web-search setting decide".
				WebSearchEnabled: true,
				// One-shot unattended turn, same as IM/MCP callers: the service
				// takes the session turn lease itself and auto-handles prompts.
				TurnLeaseHeld: false,
				Mode:          "",
			}
			// M3-1: a bridge turn suspends on client-tool calls instead of
			// answering them itself.
			req.ClientTools = turn.ClientTools
			req.Suspendable = len(turn.ClientTools) > 0
		}
		if runErr := h.sessions.AgentQA(ctx, req, bus); runErr != nil && onReturn != nil {
			onReturn(runErr)
		}
	}()
	return finished
}

// createGatewaySession creates the one-shot audit session. Kind="gateway"
// keeps it out of ordinary chat listings (repository filters kind=”) and
// gives the retention task a marker to delete by (G4).
func (h *GatewayHandler) createGatewaySession(
	ctx context.Context, tenantID uint64, agent *types.CustomAgent, model string,
) (*types.Session, error) {
	title := "gateway:" + model
	if runes := []rune(title); len(runes) > gatewaySessionTitleMaxRunes {
		title = string(runes[:gatewaySessionTitleMaxRunes])
	}
	created, err := h.sessions.CreateSession(ctx, &types.Session{
		TenantID:    tenantID,
		Kind:        types.SessionKindGateway,
		Title:       title,
		Description: "OpenAI-compatible gateway turn; model=" + model,
		// M2-3: pins the agent so thread lookups (continuation) and explicit
		// header continues can verify the session belongs to this agent.
		LastRequestState: &types.SessionLastRequestState{AgentID: agent.ID, AgentEnabled: true},
	})
	if err != nil {
		return nil, err
	}
	// Bind the API-key principal as owner, exactly like the MCP ask flow, so
	// per-owner sandbox allocation and session scope resolve consistently.
	if ownerID := types.SessionOwnerIDFromContext(ctx); ownerID != "" {
		if err := h.sessions.SetSessionOwnerID(ctx, tenantID, created.ID, ownerID); err != nil {
			return nil, err
		}
		created.UserID = ownerID
		created.OwnerAuthSource = types.AuthSourceForBoundOwnerID(ownerID)
	}
	return created, nil
}

// finalizeGatewayMessage persists the aggregated answer on the assistant row,
// mirroring the MCP caller's finalization. Storage failures are logged, not
// surfaced: the caller already has the answer.
func finalizeGatewayMessage(
	ctx context.Context, messages interfaces.MessageService, msg *types.Message, content string, refs []*types.SearchResult,
) {
	msg.Content = content
	msg.IsCompleted = true
	if len(refs) > 0 {
		msg.KnowledgeReferences = types.References(refs)
	}
	if err := messages.UpdateMessage(context.WithoutCancel(ctx), msg); err != nil {
		logger.Warnf(ctx, "[gateway] failed to finalize assistant message: session=%s msg=%s err=%v",
			msg.SessionID, msg.ID, err)
	}
}

// renderCitations appends the numbered source list to the answer body.
func renderCitations(refs []*types.SearchResult) string {
	var b strings.Builder
	b.WriteString("---\n参考来源:")
	seen := map[string]struct{}{}
	n := 0
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		key := ref.KnowledgeID + ":" + fmt.Sprint(ref.ChunkIndex)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		n++
		title := strings.TrimSpace(ref.KnowledgeTitle)
		if title == "" {
			title = ref.KnowledgeID
		}
		fmt.Fprintf(&b, "\n%d. %s", n, title)
		if n >= 10 {
			break
		}
	}
	return b.String()
}

// appendReferences merges one references event into the accumulator,
// accepting both concrete and decoded shapes (same tolerance as the stream
// handler's reference collection).
func appendReferences(dst []*types.SearchResult, raw interface{}) []*types.SearchResult {
	switch v := raw.(type) {
	case []*types.SearchResult:
		return append(dst, v...)
	case []interface{}:
		for _, item := range v {
			if sr, ok := item.(*types.SearchResult); ok {
				dst = append(dst, sr)
			}
		}
		return dst
	default:
		return dst
	}
}

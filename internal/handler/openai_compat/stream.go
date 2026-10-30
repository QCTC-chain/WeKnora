package openai_compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// sseStream serializes one streaming completion response: OpenAI chunk
// frames, heartbeat comments, and the terminal [DONE] sentinel. All writes
// go through one mutex because bus handlers (agent goroutine) and the
// heartbeat ticker (own goroutine) interleave.
//
// Frame grammar (docs §4.2):
//
//	: keep-alive                    (comment line, pre-first-delta only)
//	data: {"id":…,"choices":[{"delta":{…}}]}
//	data: [DONE]
type sseStream struct {
	c       *gin.Context
	id      string
	model   string
	created int64

	mu             sync.Mutex
	contentStarted bool
	closed         bool
}

func newSSEStream(c *gin.Context, model string) *sseStream {
	return &sseStream{
		c:       c,
		id:      "chatcmpl-" + uuid.New().String(),
		model:   model,
		created: time.Now().Unix(),
	}
}

// setHeaders installs the SSE response headers plus the gateway trace
// headers. Call once before the first frame; gin writes the status with it.
func (s *sseStream) setHeaders(agentID, sessionID string) {
	header := s.c.Writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	if agentID != "" {
		header.Set("X-WeKnora-Agent-Id", agentID)
	}
	if sessionID != "" {
		header.Set("X-WeKnora-Session-Id", sessionID)
	}
	header.Set("X-WeKnora-Gateway-Version", GatewayVersion)
}

// writeFrame emits one raw SSE frame. Returns false once the client is gone
// or the stream is closed; later writes become cheap no-ops.
func (s *sseStream) writeFrame(frame string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if _, err := s.c.Writer.WriteString(frame); err != nil {
		s.closed = true
		return false
	}
	s.c.Writer.Flush()
	return true
}

// heartbeat emits the keep-alive comment line. It is skipped once real
// content has flowed (docs §9: the heartbeat only guards the silent warm-up)
// and after the stream closes.
func (s *sseStream) heartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.contentStarted {
		return
	}
	if _, err := s.c.Writer.WriteString(": keep-alive\n\n"); err == nil {
		s.c.Writer.Flush()
	}
}

// chunk writes one chat.completion.chunk frame. A non-empty content or
// reasoning delta marks the content as started, ending the heartbeat window.
func (s *sseStream) chunk(delta *Delta, finish string, usage *Usage, meta *CompletionMeta) bool {
	if delta != nil && (delta.Content != "" || delta.ReasoningContent != "") {
		s.mu.Lock()
		s.contentStarted = true
		s.mu.Unlock()
	}
	payload := ChatCompletionChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		Usage:   usage,
		Meta:    meta,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return s.writeFrame("data: " + string(body) + "\n\n")
}

// streamError writes the mid-stream failure frame (docs §4.3): once 200 and
// the SSE content type are out, failures travel as data frames.
func (s *sseStream) streamError(message string) {
	body, err := json.Marshal(ErrorEnvelope{Error: ErrorBody{
		Message: message,
		Type:    "server_error",
	}})
	if err != nil {
		return
	}
	s.writeFrame("data: " + string(body) + "\n\n")
}

// finish closes the stream: terminal chunk (finish_reason + usage + meta)
// followed by the [DONE] sentinel.
func (s *sseStream) finish(usage *Usage, meta *CompletionMeta) {
	s.chunk(&Delta{}, FinishStop, usage, meta)
	s.writeFrame("data: [DONE]\n\n")
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// close stops further writes (disconnect paths that skip finish).
func (s *sseStream) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// startHeartbeat runs the keep-alive ticker until stop is closed. The ticker
// goroutine owns no state: s.heartbeat() no-ops once content started.
func startHeartbeat(stream *sseStream, interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		interval = config.DefaultGatewayHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			stream.heartbeat()
		case <-stop:
			return
		}
	}
}

// streamTranslator turns internal agent events into SSE frames (docs §8) and
// mirrors the aggregator's accumulation so the assistant row is persisted
// with exactly what the client saw (plus the citation block).
type streamTranslator struct {
	stream *sseStream

	// thoughtMode / progressMode come from the gateway config (docs §8):
	// reasoning|collapse|drop and text|drop.
	thoughtMode  string
	progressMode string

	// pendingTool holds the tool name of a tool_call whose result has not
	// arrived yet (docs §8: same-name consecutive events merge into ONE
	// line, sent once). The line is emitted when the result lands, as
	// "[工具] name → result-summary"; a new different call flushes the
	// pending one as "调用中" first.
	pendingTool string

	mu          sync.Mutex
	answer      strings.Builder
	refs        []*types.SearchResult
	finalAnswer string
	usage       *Usage
	failed      error

	done     chan struct{} // answer finished or failure arrived
	complete chan struct{} // complete event or failure arrived
	doneOnce sync.Once
	compOnce sync.Once
}

func newStreamTranslator(stream *sseStream, thoughtMode, progressMode string) *streamTranslator {
	return &streamTranslator{
		stream:       stream,
		thoughtMode:  thoughtMode,
		progressMode: progressMode,
		done:         make(chan struct{}),
		complete:     make(chan struct{}),
	}
}

func (t *streamTranslator) closeDone() { t.doneOnce.Do(func() { close(t.done) }) }
func (t *streamTranslator) closeComplete() {
	t.compOnce.Do(func() { close(t.complete) })
}

// content emits one status text line in the answer channel.
func (t *streamTranslator) content(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	t.stream.chunk(&Delta{Content: line}, "", nil, nil)
}

// register wires the translator onto the bus. Unknown/ignored event classes
// (chat.*, plan/step, compaction, memory, artifacts…) simply have no handler.
func (t *streamTranslator) register(bus *event.EventBus) {
	bus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentThoughtData)
		if !ok || strings.TrimSpace(data.Content) == "" {
			return nil
		}
		switch t.thoughtMode {
		case config.GatewayThoughtModeDrop:
			return nil
		case config.GatewayThoughtModeCollapse:
			t.content("<think>" + data.Content + "</think>")
		default: // reasoning
			t.stream.chunk(&Delta{ReasoningContent: data.Content}, "", nil, nil)
		}
		return nil
	})

	toolLine := func(name, status string) string {
		return "[工具] " + name + " → " + status
	}
	bus.On(event.EventAgentToolCall, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentToolCallData)
		if !ok || t.progressMode == config.GatewayProgressModeDrop {
			return nil
		}
		t.mu.Lock()
		pending := t.pendingTool
		t.pendingTool = data.ToolName
		t.mu.Unlock()
		if pending != "" && pending != data.ToolName {
			t.content(toolLine(pending, "调用中"))
		}
		return nil
	})
	bus.On(event.EventAgentToolResult, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentToolResultData)
		if !ok || t.progressMode == config.GatewayProgressModeDrop {
			return nil
		}
		t.mu.Lock()
		pending := t.pendingTool
		t.pendingTool = ""
		t.mu.Unlock()
		status := summarizeToolOutput(data.Output)
		if data.Error != "" {
			status = "失败: " + firstLine(data.Error)
		}
		if pending != "" && pending != data.ToolName {
			// A previous tool never got its result; show it first.
			t.content(toolLine(pending, "调用中"))
		}
		t.content(toolLine(data.ToolName, status))
		return nil
	})
	bus.On(event.EventAgentProgress, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentProgressData)
		if !ok || t.progressMode == config.GatewayProgressModeDrop {
			return nil
		}
		t.content(firstLine(data.Content))
		return nil
	})
	bus.On(event.EventAgentCommandOutput, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.CommandOutputData)
		if !ok || t.progressMode == config.GatewayProgressModeDrop {
			return nil
		}
		command := strings.TrimSpace(data.Command)
		if command == "" {
			command = "命令"
		}
		t.content(firstLine("[命令] " + command))
		return nil
	})

	bus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		t.answer.WriteString(data.Content)
		if data.IsFallback && strings.TrimSpace(data.Content) != "" {
			// The fallback marker rides the first fallback chunk, mirroring
			// the web stream's fallback note.
			t.answer.WriteString("\n[未命中知识库,以下为模型直答]")
		}
		if data.Done {
			t.closeDone()
		}
		t.mu.Unlock()
		if strings.TrimSpace(data.Content) != "" {
			t.stream.chunk(&Delta{Content: data.Content}, "", nil, nil)
		}
		return nil
	})
	bus.On(event.EventAgentReferences, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentReferencesData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		t.refs = appendReferences(t.refs, data.References)
		t.mu.Unlock()
		return nil
	})
	bus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentCompleteData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		if data.FinalAnswer != "" {
			t.finalAnswer = data.FinalAnswer
		}
		if len(data.KnowledgeRefs) > 0 {
			t.refs = appendReferences(t.refs, data.KnowledgeRefs)
		}
		if tu, ok := data.Usage.(*types.TokenUsage); ok && tu != nil {
			t.usage = &Usage{
				PromptTokens:     int64(tu.PromptTokens),
				CompletionTokens: int64(tu.CompletionTokens),
				TotalTokens:      int64(tu.TotalTokens),
			}
		}
		t.closeComplete()
		t.mu.Unlock()
		return nil
	})
	bus.On(event.EventError, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.ErrorData)
		if !ok {
			return nil
		}
		t.mu.Lock()
		if t.failed == nil {
			t.failed = errors.New(data.Error)
		}
		t.closeDone()
		t.closeComplete()
		t.mu.Unlock()
		return nil
	})
	// Unattended runs auto-handle prompts; a stray one folds into a line
	// instead of stalling the stream (docs §8 defensive rule).
	bus.On(event.EventAgentPromptRequired, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentPromptRequiredData)
		if !ok {
			return nil
		}
		t.content(fmt.Sprintf("[提示] 智能体请求人工输入(%s),网关以无人值守模式继续。", data.Kind))
		return nil
	})
}

// fail records a run-level failure (AgentQA returning an error).
func (t *streamTranslator) fail(err error) {
	t.mu.Lock()
	if t.failed == nil {
		t.failed = err
	}
	t.closeDone()
	t.closeComplete()
	t.mu.Unlock()
}

// snapshot returns the accumulated answer, references, usage and failure.
func (t *streamTranslator) snapshot() (string, []*types.SearchResult, *Usage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := t.answer.String()
	if strings.TrimSpace(text) == "" && t.finalAnswer != "" {
		text = t.finalAnswer
	}
	return text, t.refs, t.usage, t.failed
}

// runGatewayTurnStream is the streaming twin of runGatewayTurn: same
// message rows, same QARequest semantics, same timeout — but agent events
// translate to SSE frames as they arrive, a heartbeat guards the warm-up,
// and the terminal frames carry usage plus meta.
//
// Returned end reason feeds the audit trail (docs §6):
// completed | error | timeout | disconnect | disconnect_run_completed.
func (h *GatewayHandler) runGatewayTurnStream(
	ctx context.Context,
	session *types.Session,
	agent *types.CustomAgent,
	turn *renderedTurn,
	stream *sseStream,
) (string, *Usage) {
	userMsg, assistantMsg, err := h.createTurnMessages(ctx, session, turn)
	if err != nil {
		stream.streamError("Failed to create gateway messages")
		stream.finish(nil, nil)
		return "error", nil
	}

	translator := newStreamTranslator(
		stream,
		h.gateway.EffectiveThoughtMode(),
		h.gateway.EffectiveProgressMode(),
	)
	bus := event.NewEventBus()
	translator.register(bus)

	runParent := ctx
	if !h.gateway.EffectiveCancelOnDisconnect() {
		// Docs §8: with cancel_on_disconnect=false the run finishes even if
		// the client hangs up — already-spent tokens are not wasted.
		runParent = context.WithoutCancel(ctx)
	}
	runCtx, cancel := context.WithTimeout(runParent, h.gateway.EffectiveRequestTimeout())
	defer cancel()

	qaFinished := h.launchAgentQA(runCtx, session, agent, turn, userMsg, assistantMsg, bus, translator.fail)

	hbStop := make(chan struct{})
	defer close(hbStop)
	go startHeartbeat(stream, h.gateway.EffectiveHeartbeatInterval(), hbStop)

	// First protocol frame: the assistant role delta.
	stream.chunk(&Delta{Role: "assistant"}, "", nil, nil)

	reason := "completed"
	select {
	case <-translator.done:
		// Answer events arrived; give the completion event a grace window
		// for usage/references (same as the non-stream caller).
		grace := time.NewTimer(askCompleteWait)
		select {
		case <-translator.complete:
		case <-runCtx.Done():
			reason = timeoutReason(ctx, h.gateway)
		case <-grace.C:
			logger.Warnf(ctx, "[gateway] stream: completion grace elapsed: session=%s", session.ID)
		}
		grace.Stop()
	case <-translator.complete:
	case <-qaFinished:
		// AgentQA returned without emitting events: surface the empty
		// answer instead of waiting out the full budget.
	case <-runCtx.Done():
		reason = timeoutReason(ctx, h.gateway)
	}

	text, refs, usage, failure := translator.snapshot()
	meta := &CompletionMeta{
		Warnings: turn.Warnings,
		AgentID:  agent.ID,
		SessionID: session.ID,
	}

	switch {
	case reason == "disconnect":
		// Client gone: persist what arrived; no further frames possible.
		stream.close()
	case reason == "timeout":
		note := "[网关] 响应超时,以上为已产生的部分内容。"
		if strings.TrimSpace(text) == "" {
			note = "[网关] 响应超时,未产生任何内容。"
		}
		translator.content("\n" + note)
		text += "\n" + note
		meta.Warnings = append(meta.Warnings, "[网关] 响应超时,返回已产生的部分内容")
		stream.finish(usage, meta)
	case failure != nil:
		stream.streamError("Agent run failed: " + failure.Error())
		if strings.TrimSpace(text) == "" {
			text = "The answer failed: " + failure.Error()
		}
		reason = "error"
		stream.finish(usage, meta)
	default:
		if strings.TrimSpace(text) == "" {
			text = "No answer could be produced for this question."
			translator.content(text)
		}
		// Citations ride inside the text like the non-stream path: OpenAI
		// clients have no reference channel.
		if agent.Config.CitationEnabled == nil || *agent.Config.CitationEnabled {
			if len(refs) > 0 {
				citations := renderCitations(refs)
				translator.content("\n" + citations)
				text += "\n" + citations
			}
		}
		stream.finish(usage, meta)
	}

	finalizeGatewayMessage(context.WithoutCancel(ctx), h.messages, assistantMsg, text, refs)
	return reason, usage
}

// timeoutReason classifies a runCtx cancellation: a cancelled request
// context means the client hung up, otherwise the turn budget ran out.
func timeoutReason(requestCtx context.Context, gateway *config.GatewayConfig) string {
	if requestCtx.Err() != nil && gateway.EffectiveCancelOnDisconnect() {
		return "disconnect"
	}
	return "timeout"
}

// firstLine trims a progress/tool text to its first line so status frames
// stay single-line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const maxStatusRunes = 120
	if runes := []rune(s); len(runes) > maxStatusRunes {
		s = string(runes[:maxStatusRunes]) + "…"
	}
	return s
}

// summarizeToolOutput condenses a tool result to a short status fragment.
func summarizeToolOutput(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "完成"
	}
	return firstLine(output)
}

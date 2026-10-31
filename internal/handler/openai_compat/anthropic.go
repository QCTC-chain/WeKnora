package openai_compat

// M2-5 Anthropic-compatible endpoint (POST /v1/messages).
//
// Claude Code, pi and friends speak the Anthropic Messages API natively.
// This is a protocol shell over the same M1 pipeline: the request converts
// to the internal ChatCompletionRequest shape (system → system message,
// content blocks → text/image parts, tool_use/tool_result → textual client
// tool traffic), the shared renderer/translator/QA flow runs unchanged, and
// only the response encoding differs — non-streaming `message` objects and
// the Anthropic SSE grammar (message_start / content_block_delta /
// message_delta / message_stop).
//
// M2 scope: no tool_use blocks in responses (M3 adds the client-tool
// bridge); `tools` on the request is counted as ignored, never executed.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Anthropic protocol discriminator for the shared completion handler.
const protoAnthropic = "anthropic"

// AnthropicMessages implements POST /v1/messages — the Anthropic Messages
// API shell over the shared gateway pipeline. Auth, grants, budgets,
// idempotency, threading, audit and metering are identical to
// /v1/chat/completions; only the wire protocol differs.
//
// @Summary      Anthropic 兼容对话
// @Description  以 Anthropic Messages 协议调用已发布的智能体(system/messages/content blocks,SSE 事件流)。
// @Description  tools(input_schema)按智能体 gateway_tool_policy 桥接:模型调用时返回
// @Description  stop_reason="tool_use" + tool_use 块,续轮以 tool_result 块恢复运行。
// @Tags         网关(Anthropic 兼容)
// @Accept       json
// @Produce      json
// @Param        request body AnthropicMessagesRequest true "Anthropic Messages 请求"
// @Success      200 {object} map[string]any "message 对象或 SSE 事件流"
// @Header       200 {string} X-WeKnora-Agent-Id 智能体 ID
// @Header       200 {string} X-WeKnora-Session-Id 会话 ID
// @Failure      400 {object} map[string]any "invalid_request_error"
// @Failure      401 {object} map[string]any "authentication_error"
// @Failure      404 {object} map[string]any "not_found_error"
// @Failure      429 {object} map[string]any "rate_limit_error"
// @Security     ApiKeyAuth
// @Router       /v1/messages [post]
func (h *GatewayHandler) AnthropicMessages(c *gin.Context) {
	h.handleCompletions(c, protoAnthropic)
}

// AnthropicMessagesRequest is the subset of the Anthropic Messages API the
// gateway consumes. Unknown fields (temperature/top_p/stop_sequences/…
// sampling controls) parse-and-ignore like the OpenAI shell.
type AnthropicMessagesRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens,omitempty"` // required by Anthropic; the agent owns the budget
	System    json.RawMessage `json:"system,omitempty"`     // string | [{type:"text",text}]
	Messages  []struct {
		Role    string          `json:"role"` // user | assistant
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream   bool `json:"stream,omitempty"`
	Metadata struct {
		// Anthropic's external user id; feeds the M2-3 derived thread key.
		UserID string `json:"user_id,omitempty"`
	} `json:"metadata,omitempty"`
	// Tools are the caller's Anthropic tool definitions ({name, description,
	// input_schema}); converted to function specs and bridged like the
	// OpenAI shell's tools[] (gateway_tool_policy "none" ignores them).
	Tools []json.RawMessage `json:"tools,omitempty"`
}

// toOpenAI converts the Anthropic request into the internal completion
// shape so the shared handler runs the M1 pipeline unchanged.
func (r *AnthropicMessagesRequest) toOpenAI() (*ChatCompletionRequest, error) {
	req := &ChatCompletionRequest{
		Model:  r.Model,
		Stream: r.Stream,
		User:   r.Metadata.UserID,
	}
	if len(r.Tools) > 0 {
		// M3-1: Anthropic tool definitions bridge like OpenAI's — each
		// {name, description, input_schema} becomes a function spec.
		defs, err := anthropicTools(r.Tools)
		if err != nil {
			return nil, err
		}
		req.Tools = defs
	}

	if sysText, err := anthropicSystemText(r.System); err != nil {
		return nil, err
	} else if sysText != "" {
		req.Messages = append(req.Messages, ChatMessage{Role: "system", Content: ContentUnion{Text: sysText}})
	}

	for i, m := range r.Messages {
		blocks, err := anthropicBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		parts, toolCalls, toolResults := anthropicPartsFromBlocks(blocks)
		if m.Role == "assistant" {
			// M2: record client tool calls textually (the gateway never
			// executes them); the M1 renderer folds them into the history
			// and uses the ids to name later tool_result records.
			req.Messages = append(req.Messages, ChatMessage{
				Role:      "assistant",
				Content:   partsToUnion(parts),
				ToolCalls: toolCalls,
			})
			continue
		}
		// user message: text/image parts; tool_result blocks become tool-
		// role records so the M1 tool textification renders them.
		req.Messages = append(req.Messages, ChatMessage{
			Role:    "user",
			Content: partsToUnion(parts),
		})
		for _, tr := range toolResults {
			req.Messages = append(req.Messages, ChatMessage{
				Role:       "tool",
				ToolCallID: tr.id,
				Content:    ContentUnion{Text: tr.text},
			})
		}
	}
	return req, nil
}

func partsToUnion(parts []ContentPart) ContentUnion {
	u := ContentUnion{Parts: parts}
	for _, p := range parts {
		if p.Type == PartTypeText {
			u.Text += p.Text
		}
	}
	return u
}

// anthropicSystemText flattens the top-level system field (string or text
// block array) into one system message text.
func anthropicSystemText(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("system: %w", err)
		}
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system: expected string or block array: %w", err)
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), nil
}

// anthropicToolResult is one tool_result block flattened for the renderer.
type anthropicToolResult struct {
	id   string
	text string
}

// anthropicBlock is one decoded content block.
type anthropicBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// image block
	Source *anthropicImageSource `json:"source,omitempty"`
	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result block
	Content   json.RawMessage `json:"content,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// anthropicImageSource is the source of an image block (base64 or url).
type anthropicImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// anthropicBlocks decodes one message content (string or block array).
func anthropicBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if s == "" {
			return nil, nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// anthropicPartsFromBlocks splits decoded blocks into content parts, client
// tool calls (assistant tool_use) and tool results (user tool_result).
func anthropicPartsFromBlocks(blocks []anthropicBlock) (
	[]ContentPart, []ClientToolCall, []anthropicToolResult,
) {
	var (
		parts   []ContentPart
		calls   []ClientToolCall
		results []anthropicToolResult
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, ContentPart{Type: PartTypeText, Text: b.Text})
			}
		case "image":
			url, err := anthropicImageURL(b.Source)
			if err != nil {
				// Degrade to a text note instead of failing the request.
				parts = append(parts, ContentPart{Type: PartTypeText,
					Text: "[图片已忽略:" + err.Error() + "]"})
				continue
			}
			parts = append(parts, ContentPart{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: url}})
		case "tool_use":
			args := strings.TrimSpace(string(b.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, ClientToolCall{
				ID:   b.ID,
				Type: "function",
				Function: ClientToolFnCall{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case "tool_result":
			text := anthropicToolResultText(b.Content)
			if b.IsError {
				text = "[错误] " + text
			}
			results = append(results, anthropicToolResult{id: b.ToolUseID, text: text})
		default:
			// Unknown block types degrade to text instead of failing the
			// whole request (nothing a client sends is silently dropped).
			parts = append(parts, ContentPart{Type: PartTypeText,
				Text: "[" + b.Type + "] (不支持的块类型,已忽略)"})
		}
	}
	return parts, calls, results
}

// anthropicImageURL converts an Anthropic image source to the URL form the
// internal pipeline already understands (data: or http(s)://).
func anthropicImageURL(src *anthropicImageSource) (string, error) {
	if src == nil {
		return "", fmt.Errorf("image block without source")
	}
	switch src.Type {
	case "url":
		if strings.HasPrefix(strings.ToLower(src.URL), "https://") ||
			strings.HasPrefix(strings.ToLower(src.URL), "http://") {
			return src.URL, nil
		}
		return "", fmt.Errorf("image source url must be http(s)")
	case "base64":
		media := src.MediaType
		if media == "" {
			media = "image/png"
		}
		if _, err := base64.StdEncoding.DecodeString(src.Data); err != nil {
			return "", fmt.Errorf("image source base64 data: %w", err)
		}
		return "data:" + media + ";base64," + src.Data, nil
	default:
		return "", fmt.Errorf("unsupported image source type %q", src.Type)
	}
}

// anthropicToolResultText flattens a tool_result content (string or block
// array) to the textual form the renderer records in history.
func anthropicToolResultText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return trimmed
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Non-array shapes (e.g. number) stringify.
		return trimmed
	}
	var sb strings.Builder
	for _, b := range blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		if b.IsError {
			sb.WriteString("[错误] ")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// --- Anthropic SSE stream ---

// anthropicStream renders the shared turn stream in Anthropic Messages SSE
// grammar. Blocks open lazily: a thinking block for reasoning deltas, a
// text block for answer/status text; opening a text block closes an open
// thinking block first.
//
//	event: message_start
//	event: content_block_start   {content_block:{type:"thinking"|"text"}}
//	event: content_block_delta   {delta:{type:"thinking_delta"|"text_delta"}}
//	event: content_block_stop
//	event: message_delta         {delta:{stop_reason:"end_turn"}, usage}
//	event: message_stop
type anthropicStream struct {
	c     *gin.Context
	id    string
	model string
	mu    sync.Mutex
	// openBlock is the index of the currently open content block, -1 none.
	openBlock  int
	blockKind  string // "thinking" | "text"
	nextIndex  int
	startedAt  time.Time
	agentID    string
	ttfbDone   bool
	hbTarget   *sseHeartbeatState
	streamDone bool // set by close/finishFrame; guards late writes
}

// sseHeartbeatState tracks whether real content has flowed, shared with the
// heartbeat ticker (keep-alives stop once content flows).
type sseHeartbeatState struct {
	mu             sync.Mutex
	contentStarted bool
}

func (h *sseHeartbeatState) mark() {
	h.mu.Lock()
	h.contentStarted = true
	h.mu.Unlock()
}

func newAnthropicStream(c *gin.Context, model string) *anthropicStream {
	return &anthropicStream{
		c:         c,
		id:        "msg_" + uuid.New().String(),
		model:     model,
		openBlock: -1,
		startedAt: time.Now(),
		agentID:   "unknown",
		hbTarget:  &sseHeartbeatState{},
	}
}

func (s *anthropicStream) setHeaders(agentID, sessionID string) {
	if agentID != "" {
		s.agentID = agentID
	}
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
	s.messageStart()
}

// writeEvent emits one named SSE event with a JSON payload; locks itself.
func (s *anthropicStream) writeEvent(name string, payload any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeEventLocked(name, payload)
}

// writeEventLocked emits one named SSE event; the caller holds s.mu.
func (s *anthropicStream) writeEventLocked(name string, payload any) bool {
	if s.streamDone {
		return false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	frame := "event: " + name + "\ndata: " + string(body) + "\n\n"
	if _, err := s.c.Writer.WriteString(frame); err != nil {
		return false
	}
	s.c.Writer.Flush()
	return true
}

func (s *anthropicStream) messageStart() {
	s.writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          s.id,
			"type":        "message",
			"role":        "assistant",
			"model":       s.model,
			"content":     []any{},
			"stop_reason": nil,
			"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// openBlockLocked lazily opens a content block of the given kind; must hold
// s.mu. Re-opening the same kind is a no-op.
func (s *anthropicStream) openBlockLocked(kind string) {
	if s.closed() || (s.openBlock >= 0 && s.blockKind == kind) {
		return
	}
	s.closeBlockLocked()
	s.openBlock = s.nextIndex
	s.blockKind = kind
	s.nextIndex++
	var contentBlock map[string]any
	if kind == "thinking" {
		contentBlock = map[string]any{"type": "thinking", "thinking": ""}
	} else {
		contentBlock = map[string]any{"type": "text", "text": ""}
	}
	s.writeEventLocked("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.openBlock,
		"content_block": contentBlock,
	})
}

// closeBlockLocked must hold s.mu.
func (s *anthropicStream) closeBlockLocked() {
	if s.openBlock < 0 || s.closed() {
		return
	}
	s.writeEventLocked("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.openBlock,
	})
	s.openBlock = -1
	s.blockKind = ""
}

// --- turnStream implementation ---

func (s *anthropicStream) beginAssistant() {} // message_start already sent

func (s *anthropicStream) contentLine(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	s.markTTFB()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openBlockLocked("text")
	s.writeEventLocked("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.openBlock,
		"delta": map[string]any{"type": "text_delta", "text": line},
	})
}

func (s *anthropicStream) reasoningDelta(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.markTTFB()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openBlockLocked("thinking")
	s.writeEventLocked("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.openBlock,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (s *anthropicStream) collapsedThought(text string) {
	// Collapse mode emits <think>…</think> into the text channel on the
	// OpenAI shell; Anthropic gets it as real thinking deltas instead.
	s.reasoningDelta(text)
}

func (s *anthropicStream) errorFrame(message string) {
	s.mu.Lock()
	s.closeBlockLocked()
	s.mu.Unlock()
	s.writeEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

func (s *anthropicStream) finishFrame(usage *Usage, meta *CompletionMeta) {
	s.mu.Lock()
	s.closeBlockLocked()
	s.mu.Unlock()
	var outputTokens int64
	if usage != nil {
		outputTokens = usage.CompletionTokens
	}
	s.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	s.writeEvent("message_stop", map[string]any{"type": "message_stop"})
	s.mu.Lock()
	s.hbTarget.mu.Lock()
	s.hbTarget.contentStarted = true // keep-alives end with the stream
	s.hbTarget.mu.Unlock()
	s.streamDone = true
	s.mu.Unlock()
}

func (s *anthropicStream) close() {
	s.mu.Lock()
	s.streamDone = true
	s.mu.Unlock()
}

// bridgeToolCalls emits one tool_use content block per client tool call
// (M3-1); the caller streams them in model order before finishBridge.
func (s *anthropicStream) bridgeToolCalls(calls []outboundCall) {
	s.markTTFB()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeBlockLocked()
	for _, call := range calls {
		index := s.nextIndex
		s.nextIndex++
		s.writeEventLocked("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "tool_use", "id": call.WireID,
				"name": call.WireToolName(), "input": map[string]any{},
			},
		})
		s.writeEventLocked("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
		})
		s.writeEventLocked("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": index,
		})
	}
}

// finishBridge closes the stream with stop_reason "tool_use".
func (s *anthropicStream) finishBridge(usage *Usage, meta *CompletionMeta) {
	s.mu.Lock()
	s.closeBlockLocked()
	s.mu.Unlock()
	var outputTokens int64
	if usage != nil {
		outputTokens = usage.CompletionTokens
	}
	s.writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	s.writeEvent("message_stop", map[string]any{"type": "message_stop"})
	s.mu.Lock()
	s.hbTarget.mu.Lock()
	s.hbTarget.contentStarted = true
	s.hbTarget.mu.Unlock()
	s.streamDone = true
	s.mu.Unlock()
}

// closed reports the terminated state; callers hold s.mu.
func (s *anthropicStream) closed() bool { return s.streamDone }

// markTTFB observes the first-content latency once.
func (s *anthropicStream) markTTFB() {
	s.mu.Lock()
	if !s.ttfbDone {
		s.ttfbDone = true
		agent := s.agentID
		started := s.startedAt
		s.mu.Unlock()
		gatewayMetrics.observeTTFB(protoAnthropic, agent, time.Since(started))
		return
	}
	s.mu.Unlock()
}

func (s *anthropicStream) heartbeatTick() {
	s.hbTarget.mu.Lock()
	started := s.hbTarget.contentStarted
	s.hbTarget.mu.Unlock()
	if started {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamDone {
		return
	}
	if _, err := s.c.Writer.WriteString(": keep-alive\n\n"); err == nil {
		s.c.Writer.Flush()
	}
}

// --- non-streaming response + error encoding ---

// respondAnthropicMessage writes the non-streaming Anthropic message object.
func respondAnthropicMessage(c *gin.Context, model, text string, usage *Usage) {
	var inputTokens, outputTokens int64
	if usage != nil {
		inputTokens = usage.PromptTokens
		outputTokens = usage.CompletionTokens
	}
	if strings.TrimSpace(text) == "" {
		text = "(empty answer)"
	}
	c.JSON(http.StatusOK, map[string]any{
		"id":            "msg_" + uuid.New().String(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]any{{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
}

// respondAnthropicBridge answers a suspended tool turn (M3-1): each pending
// call becomes a tool_use block and stop_reason is "tool_use"; the caller's
// next messages carry tool_result blocks with the same ids.
func respondAnthropicBridge(
	c *gin.Context, model, text string, calls []outboundCall, usage *Usage,
) {
	var inputTokens, outputTokens int64
	if usage != nil {
		inputTokens = usage.PromptTokens
		outputTokens = usage.CompletionTokens
	}
	content := make([]map[string]any, 0, len(calls)+1)
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range calls {
		var input map[string]any
		if len(call.Arguments) > 0 {
			_ = json.Unmarshal([]byte(call.Arguments), &input)
		}
		if input == nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.WireID,
			"name":  call.Name,
			"input": input,
		})
	}
	c.JSON(http.StatusOK, map[string]any{
		"id":            "msg_" + uuid.New().String(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
}

// anthropicToolWire is one entry of an Anthropic tools array.
type anthropicToolWire struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// anthropicTools converts the Anthropic tools array into the shared function
// definitions the bridge validates.
func anthropicTools(raw []json.RawMessage) ([]ClientToolDefinition, error) {
	defs := make([]ClientToolDefinition, 0, len(raw))
	for i, item := range raw {
		var tool anthropicToolWire
		if err := json.Unmarshal(item, &tool); err != nil {
			return nil, fmt.Errorf("tools[%d]: %v", i, err)
		}
		defs = append(defs, ClientToolDefinition{
			Type: "function",
			Function: ClientToolFnSpec{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}
	return defs, nil
}

// anthropicErrorType maps HTTP status to the Anthropic error taxonomy.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest,
		http.StatusRequestTimeout, http.StatusConflict:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// writeAnthropicError writes the Anthropic error envelope.
func writeAnthropicError(c *gin.Context, status int, message string) {
	c.JSON(status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrorType(status), "message": message},
	})
}

// --- protocol dispatch helpers used by the shared handler ---

// writeProtoError/writeProtoParam dispatch error envelopes by protocol.
func writeProtoError(c *gin.Context, proto string, status int, code, message string) {
	if proto == protoAnthropic {
		writeAnthropicError(c, status, message)
		return
	}
	writeError(c, status, code, message)
}

func writeProtoParam(c *gin.Context, proto string, status int, field, message string) {
	if proto == protoAnthropic {
		writeAnthropicError(c, status, message)
		return
	}
	writeErrorParam(c, status, "", field, message)
}

// protoStream builds the protocol stream for a streaming turn.
func protoStream(c *gin.Context, proto, model string) turnStream {
	if proto == protoAnthropic {
		return newAnthropicStream(c, model)
	}
	return newSSEStream(c, model)
}

// parseCompletionsRequest parses the protocol request into the internal
// ChatCompletionRequest shape. rawBody is the already-read request body.
func parseCompletionsRequest(rawBody []byte, proto string) (*ChatCompletionRequest, error) {
	if proto != protoAnthropic {
		var req ChatCompletionRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			return nil, err
		}
		return &req, nil
	}
	var anthropicReq AnthropicMessagesRequest
	if err := json.Unmarshal(rawBody, &anthropicReq); err != nil {
		return nil, err
	}
	return anthropicReq.toOpenAI()
}

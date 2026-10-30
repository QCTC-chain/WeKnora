// Package openai_compat exposes WeKnora agents as models on an
// OpenAI-compatible surface (GET /v1/models, POST /v1/chat/completions) so
// external agent clients (pi, zcode, deepseek, any OpenAI SDK) can use a
// published WeKnora agent as if it were an LLM.
//
// Design: docs/design/agent-model-gateway-m1.md. This file carries the wire
// types only; the handler lives in handler.go. Field names follow the OpenAI
// Chat Completions API exactly — external clients parse them unmodified.
package openai_compat

import "time"

// GatewayVersion is reported in the X-WeKnora-Gateway-Version response header
// so clients can detect breaking wire changes early.
const GatewayVersion = "1"

// ModelListResponse is the GET /v1/models body.
type ModelListResponse struct {
	Object string        `json:"object"` // always "list"
	Data   []ModelEntry  `json:"data"`
}

// ModelEntry is one published agent as an OpenAI model.
type ModelEntry struct {
	// ID is the agent UUID — the canonical model id. The agent's name is
	// accepted as an alias on completions but the UUID is what clients should
	// persist.
	ID string `json:"id"`
	// Object is always "model".
	Object string `json:"object"` // "model"
	// Created is the publish instant (unix seconds); 0 when unknown.
	Created int64 `json:"created"`
	// OwnedBy is always "weknora".
	OwnedBy string `json:"owned_by"`
	// Meta carries WeKnora-specific display hints. Non-standard: OpenAI
	// clients ignore unknown fields, WeKnora-aware UIs may render them.
	Meta *ModelMeta `json:"meta,omitempty"`
}

// ModelMeta carries non-standard display metadata for a gateway model.
type ModelMeta struct {
	// AgentName is the human-readable agent name (also usable as a model
	// alias on completions).
	AgentName string `json:"agent_name,omitempty"`
	// Description is the agent's description, trimmed for list payloads.
	Description string `json:"description,omitempty"`
	// AgentMode is quick-answer or smart-reasoning.
	AgentMode string `json:"agent_mode,omitempty"`
	// IsBuiltin marks workspace built-in agents.
	IsBuiltin bool `json:"is_builtin,omitempty"`
}

// ChatCompletionRequest is the accepted subset of the OpenAI request.
// Unknown fields are ignored (standard JSON decoding), matching how the
// OpenAI API treats unknown request fields.
type ChatCompletionRequest struct {
	// Model is the published agent: its UUID, or its name (unique among the
	// workspace's published agents).
	Model string `json:"model"`
	// Messages is the conversation so far; rendered into a single gateway
	// turn (docs/design/agent-model-gateway-m1.md §7.2).
	Messages []ChatMessage `json:"messages"`
	// Stream selects SSE chunking; false aggregates the full answer.
	Stream bool `json:"stream"`
	// User is an opaque caller identifier, forwarded to audit logs only.
	User string `json:"user"`
	// StreamOptions is accepted for wire compatibility; only include_usage
	// matters and usage is always included.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions mirrors the OpenAI stream_options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage is one OpenAI chat message. Content may be a plain string or a
// multimodal content-part array, so it decodes into Content union type.
type ChatMessage struct {
	Role    string       `json:"role"` // system / developer / user / assistant / tool
	Content ContentUnion `json:"content,omitempty"`
	// Name is the legacy participant name; kept in the history transcript.
	Name string `json:"name,omitempty"`
	// ToolCallID ties a tool result to a prior tool call (rendered as text).
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls on assistant messages are recorded textually in the history
	// transcript; the gateway never executes client tools.
	ToolCalls []ClientToolCall `json:"tool_calls,omitempty"`
}

// ClientToolCall mirrors the OpenAI tool_calls entry. M1 records these in the
// transcript but does not execute or echo them back.
type ClientToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function ClientToolFnCall `json:"function"`
}

// ClientToolFnCall is the function name/arguments pair of a tool call.
type ClientToolFnCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ContentUnion decodes OpenAI message content: a plain string, or an array of
// typed content parts (text / image_url), or null.
type ContentUnion struct {
	Text  string
	Parts []ContentPart
}

// UnmarshalJSON accepts string, []ContentPart and null.
func (cu *ContentUnion) UnmarshalJSON(data []byte) error {
	trimmed := trimSpaceBytes(data)
	if string(trimmed) == "null" {
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := jsonUnmarshal(data, &s); err != nil {
			return err
		}
		cu.Text = s
		cu.Parts = nil
		return nil
	}
	var parts []ContentPart
	if err := jsonUnmarshal(data, &parts); err != nil {
		return err
	}
	cu.Parts = parts
	// A bare text-only array still yields usable text for the renderer.
	for _, p := range parts {
		if p.Type == PartTypeText {
			cu.Text += p.Text
		}
	}
	return nil
}

// MarshalJSON round-trips the simple string form; the gateway only ever
// produces string content (assistant messages).
func (cu ContentUnion) MarshalJSON() ([]byte, error) {
	return jsonMarshal(cu.Text)
}

// ContentPart is one multimodal content block.
type ContentPart struct {
	Type     string      `json:"type"` // "text" | "image_url"
	Text     string      `json:"text,omitempty"`
	ImageURL *ImageURLPo `json:"image_url,omitempty"`
}

// ImageURLPo carries an image reference (data URL or http(s) URL).
type ImageURLPo struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// PartTypeText and PartTypeImage are the content part discriminators.
const (
	PartTypeText  = "text"
	PartTypeImage = "image_url"
)

// ChatCompletionResponse is the non-streaming response body.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"` // "chat.completion"
	Created int64                  `json:"created"`
	Model   string                 `json:"model"` // the caller's model string, echoed
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *Usage                 `json:"usage,omitempty"`
}

// ChatCompletionChoice is one completion alternative (the gateway always
// returns exactly one).
type ChatCompletionChoice struct {
	Index        int          `json:"index"`
	Message      *RespMessage `json:"message,omitempty"`
	FinishReason string       `json:"finish_reason"` // "stop" in M1
}

// RespMessage is the assistant message of a non-streaming response.
type RespMessage struct {
	Role    string `json:"role"` // "assistant"
	Content string `json:"content"`
}

// ChatCompletionChunk is one SSE payload in streaming mode.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	// Usage is present only on the final chunk (OpenAI include_usage style).
	Usage *Usage `json:"usage,omitempty"`
}

// ChunkChoice carries one incremental delta.
type ChunkChoice struct {
	Index        int           `json:"index"`
	Delta        *Delta        `json:"delta,omitempty"`
	FinishReason string        `json:"finish_reason"` // "" until final
}

// Delta is the incremental payload of a chunk. Exactly one field is typically
// set per chunk: role on the first, content or reasoning_content for
// increments, none on the terminal chunk.
type Delta struct {
	Role string `json:"role,omitempty"`
	// Content is answer text (from the agent's final_answer events).
	Content string `json:"content,omitempty"`
	// ReasoningContent carries intermediate thinking deltas (DeepSeek-style
	// extension; rendered as reasoning by pi/zcode/deepseek clients).
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Usage reports token consumption for the whole gateway turn. Both counters
// accumulate EVERY internal LLM call of the agent loop (retrieval rounds,
// tool rounds, compaction), so they are larger than the caller-visible text
// suggests — the amplification is inherent to Agent-as-Model and documented
// in the design doc §8.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Add accumulates one internal LLM call's usage.
func (u *Usage) Add(prompt, completion int64) {
	u.PromptTokens += prompt
	u.CompletionTokens += completion
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
}

// ErrorEnvelope is the OpenAI error body: {"error":{"message","type","code"}}.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// FinishStop is the only finish_reason the gateway emits in M1: client tools
// are not executed, so the run always ends in a textual answer or an error
// event.
const FinishStop = "stop"

// unixTime returns t as unix seconds, 0 for nil/zero — published_at is
// nullable in the DB.
func unixTime(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.Unix()
}

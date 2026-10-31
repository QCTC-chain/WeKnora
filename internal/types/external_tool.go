package types

import (
	"encoding/json"
	"fmt"
)

// AgentPromptKindExternalToolCall suspends an agent run while the gateway
// hands a batch of model-requested tool calls to the external OpenAI/Anthropic
// client for execution. The client answers with one result per call; the run
// then resumes with those results injected as tool outputs. See
// docs/design/agent-model-gateway-m2-m3.md §7 (M3-1).
const AgentPromptKindExternalToolCall = "external_tool_call"

// External tool bridge limits. The gateway enforces them on the wire; the
// validator re-checks the persisted answer, so a tampered row cannot inject
// unbounded text into a resumed run.
const (
	// MaxExternalToolCallsPerRound caps one suspension's call batch: the
	// OpenAI/Anthropic clients must execute them all before resuming.
	MaxExternalToolCallsPerRound = 32
	// MaxExternalToolResultRunes caps one tool result's text. Larger results
	// fail validation instead of silently flooding the model context.
	MaxExternalToolResultRunes = 32000
	// MaxExternalToolNameRunes / MaxExternalToolDescRunes / MaxExternalToolParamsBytes
	// bound the client tool definitions the gateway accepts per request.
	MaxExternalToolNameRunes   = 128
	MaxExternalToolDescRunes   = 4000
	MaxExternalToolParamsBytes = 32768
	MaxClientToolsPerRequest   = 64
)

// ExternalToolCallSpec is one model-requested client tool call inside a
// suspension batch. Name is the tool name the model called (the gateway's
// registered name); WireName is the name the client knows it by, which
// differs only when the client's name collided with an internal tool.
type ExternalToolCallSpec struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	WireName  string `json:"wire_name,omitempty"`
	Arguments string `json:"arguments"`
}

// WireToolName returns the name the external client sees.
func (s ExternalToolCallSpec) WireToolName() string {
	if s.WireName != "" {
		return s.WireName
	}
	return s.Name
}

// ExternalToolCallPromptRequest is the Request of an
// AgentPromptKindExternalToolCall: every client tool call of one ReAct round.
type ExternalToolCallPromptRequest struct {
	Calls []ExternalToolCallSpec `json:"calls"`
}

// ExternalToolCallResult is the client's answer for one call.
type ExternalToolCallResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	// IsError marks a failed client execution: the content explains why.
	IsError bool `json:"is_error,omitempty"`
}

// ExternalToolCallPromptResponse is the Response of an
// AgentPromptKindExternalToolCall: one result per requested call.
type ExternalToolCallPromptResponse struct {
	Results []ExternalToolCallResult `json:"results"`
}

// ClientToolSpec is one client tool definition from a gateway request
// (OpenAI tools[] / Anthropic tools[]), already validated for size.
type ClientToolSpec struct {
	// Name is the wire name the client declared.
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ValidateExternalToolCallResponse is the broker validator of
// AgentPromptKindExternalToolCall: exactly one well-formed result per
// requested call, no extras, within the result size cap.
func ValidateExternalToolCallResponse(request, response json.RawMessage) error {
	var req ExternalToolCallPromptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if len(req.Calls) == 0 || len(req.Calls) > MaxExternalToolCallsPerRound {
		return fmt.Errorf("request carries %d calls", len(req.Calls))
	}
	var resp ExternalToolCallPromptResponse
	if err := json.Unmarshal(response, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	byID := make(map[string]ExternalToolCallResult, len(req.Calls))
	for _, r := range resp.Results {
		if _, dup := byID[r.CallID]; dup {
			return fmt.Errorf("duplicate result for call %s", r.CallID)
		}
		byID[r.CallID] = r
	}
	for _, c := range req.Calls {
		r, ok := byID[c.CallID]
		if !ok {
			return fmt.Errorf("missing result for call %s (%s)", c.CallID, c.WireToolName())
		}
		if runes := len([]rune(r.Content)); runes > MaxExternalToolResultRunes {
			return fmt.Errorf("result for call %s exceeds %d runes", c.CallID, MaxExternalToolResultRunes)
		}
	}
	if len(resp.Results) != len(req.Calls) {
		return fmt.Errorf("%d results for %d calls", len(resp.Results), len(req.Calls))
	}
	return nil
}

package openai_compat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The M3-1 tool bridge: the gateway turns a suspension on an
// AgentPromptKindExternalToolCall prompt into an OpenAI tool_calls /
// Anthropic tool_use response, and accepts the caller's tool results as
// tool-role follow-ups that resume the run.
//
// call IDs are self-describing so a follow-up request can find its prompt
// without extra state: base64url(promptID).base64url(toolCallID).expiry.hmac.
// The HMAC stops callers from minting IDs for prompts they cannot see; the
// broker's tenant/session/kind checks are the real authorization.

const bridgeCallIDPrefix = "wc1_"

// toolBridgePepper keys the call-ID HMAC when no secret is configured. The
// broker re-validates tenant/kind/expiry on every resume, so this is a
// canary rather than the authorization boundary.
const toolBridgePepper = "weknora-gateway-tool-bridge-v1"

func toolBridgeKey(cfg *config.GatewayConfig) []byte {
	if cfg != nil && cfg.EffectiveToolBridgeSecret() != "" {
		return []byte(cfg.EffectiveToolBridgeSecret())
	}
	return []byte(toolBridgePepper)
}

func bridgeHMAC(key []byte, body string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:12])
}

// encodeBridgeCallID stamps one tool call for the response.
func encodeBridgeCallID(key []byte, promptID, toolCallID string, expiresAt *time.Time, now time.Time) string {
	exp := now.Add(config.DefaultGatewaySuspendGrace).Unix()
	if expiresAt != nil && expiresAt.Before(time.Unix(exp, 0)) {
		exp = expiresAt.Unix()
	}
	body := base64.RawURLEncoding.EncodeToString([]byte(promptID)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(toolCallID)) + "." +
		strconv.FormatInt(exp, 10)
	return bridgeCallIDPrefix + body + "." + bridgeHMAC(key, body)
}

// bridgeCall is one decoded gateway call ID.
type bridgeCall struct {
	PromptID   string
	ToolCallID string
	ExpiresAt  time.Time
}

// decodeBridgeCallID verifies the stamp and expiry.
func decodeBridgeCallID(key []byte, raw string, now time.Time) (bridgeCall, error) {
	if !strings.HasPrefix(raw, bridgeCallIDPrefix) {
		return bridgeCall{}, fmt.Errorf("not a gateway tool call id")
	}
	parts := strings.Split(strings.TrimPrefix(raw, bridgeCallIDPrefix), ".")
	if len(parts) != 4 {
		return bridgeCall{}, fmt.Errorf("malformed gateway tool call id")
	}
	if bridgeHMAC(key, parts[0]+"."+parts[1]+"."+parts[2]) != parts[3] {
		return bridgeCall{}, fmt.Errorf("gateway tool call id signature mismatch")
	}
	promptID, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return bridgeCall{}, fmt.Errorf("decode prompt id: %w", err)
	}
	callID, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return bridgeCall{}, fmt.Errorf("decode tool call id: %w", err)
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return bridgeCall{}, fmt.Errorf("decode expiry: %w", err)
	}
	expiresAt := time.Unix(exp, 0)
	if now.After(expiresAt) {
		return bridgeCall{}, fmt.Errorf(
			"gateway tool call expired at %s; send a fresh request to restart the turn",
			expiresAt.UTC().Format(time.RFC3339))
	}
	return bridgeCall{
		PromptID:   string(promptID),
		ToolCallID: string(callID),
		ExpiresAt:  expiresAt,
	}, nil
}

// bridgeSuspension is one run suspension waiting for client tool results.
type bridgeSuspension struct {
	PromptID  string
	ExpiresAt *time.Time
	Calls     []types.ExternalToolCallSpec
}

// outboundCalls pairs every model-requested call with its wire call ID.
type outboundCall struct {
	// WireID is the encoded gateway call ID the client echoes back.
	WireID string
	types.ExternalToolCallSpec
}

func (b *bridgeSuspension) outbound(key []byte, now time.Time) []outboundCall {
	out := make([]outboundCall, 0, len(b.Calls))
	for _, call := range b.Calls {
		out = append(out, outboundCall{
			WireID:               encodeBridgeCallID(key, b.PromptID, call.CallID, b.ExpiresAt, now),
			ExternalToolCallSpec: call,
		})
	}
	return out
}

// validateClientTools checks the request's tools[] against the bridge limits.
// Only function tools bridge; other flavors answer a parameter error.
func validateClientTools(defs []ClientToolDefinition) ([]types.ClientToolSpec, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	if len(defs) > types.MaxClientToolsPerRequest {
		return nil, fmt.Errorf("too many tools (%d, max %d)", len(defs), types.MaxClientToolsPerRequest)
	}
	specs := make([]types.ClientToolSpec, 0, len(defs))
	seen := make(map[string]bool, len(defs))
	for i, def := range defs {
		if def.Type != "" && def.Type != "function" {
			return nil, fmt.Errorf("tools[%d]: unsupported type %q (only \"function\")", i, def.Type)
		}
		spec := types.ClientToolSpec{
			Name:        def.Function.Name,
			Description: def.Function.Description,
			Parameters:  def.Function.Parameters,
		}
		if runes := len([]rune(spec.Name)); runes == 0 || runes > types.MaxExternalToolNameRunes {
			return nil, fmt.Errorf("tools[%d]: name must be 1-%d runes", i, types.MaxExternalToolNameRunes)
		}
		if seen[spec.Name] {
			return nil, fmt.Errorf("tools[%d]: duplicate tool name %q", i, spec.Name)
		}
		seen[spec.Name] = true
		if runes := len([]rune(spec.Description)); runes > types.MaxExternalToolDescRunes {
			return nil, fmt.Errorf("tools[%d] %q: description exceeds %d runes",
				i, spec.Name, types.MaxExternalToolDescRunes)
		}
		if len(spec.Parameters) > types.MaxExternalToolParamsBytes {
			return nil, fmt.Errorf("tools[%d] %q: parameters exceed %d bytes",
				i, spec.Name, types.MaxExternalToolParamsBytes)
		}
		if len(spec.Parameters) > 0 {
			var probe map[string]any
			if err := json.Unmarshal(spec.Parameters, &probe); err != nil {
				return nil, fmt.Errorf("tools[%d] %q: parameters must be a JSON object", i, spec.Name)
			}
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// bridgeResult is one decoded tool message.
type bridgeResult struct {
	call    bridgeCall
	content string
	isError bool
}

// bridgeResultMap indexes the request's tool results by wire and internal
// call ID.
type bridgeResultMap map[string]bridgeResult

// byInternalID finds the result whose decoded call ID equals id.
func (m bridgeResultMap) byInternalID(id string) (bridgeResult, bool) {
	for _, r := range m {
		if r.call.ToolCallID == id {
			return r, true
		}
	}
	return bridgeResult{}, false
}

// decodeBridgeResultMap extracts the tool-role answers from a follow-up
// request. Every tool message must carry a gateway call ID; anything else is
// a protocol error.
func decodeBridgeResultMap(key []byte, msgs []ChatMessage, now time.Time) (bridgeResultMap, error) {
	results := bridgeResultMap{}
	for _, msg := range msgs {
		if msg.Role != "tool" {
			continue
		}
		if msg.ToolCallID == "" {
			return nil, errors.New("tool message without tool_call_id")
		}
		call, err := decodeBridgeCallID(key, msg.ToolCallID, now)
		if err != nil {
			return nil, fmt.Errorf("tool_call_id %q: %v", msg.ToolCallID, err)
		}
		if prev, dup := results[msg.ToolCallID]; dup && prev.call.PromptID != call.PromptID {
			return nil, fmt.Errorf("tool_call_id %q conflicts with another pending call", msg.ToolCallID)
		}
		results[msg.ToolCallID] = bridgeResult{call: call, content: msg.Content.Text}
	}
	return results, nil
}

// decodeBridgePromptRequest parses the external_tool_call prompt payload an
// EventAgentPromptRequired event carries.
func decodeBridgePromptRequest(raw json.RawMessage) (*types.ExternalToolCallPromptRequest, error) {
	var req types.ExternalToolCallPromptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode external tool call request: %w", err)
	}
	if len(req.Calls) == 0 {
		return nil, errors.New("external tool call request carries no calls")
	}
	return &req, nil
}

// completionsMeta assembles the meta block shared by ordinary and resumed
// responses.
func (h *GatewayHandler) completionsMeta(
	agent *types.CustomAgent, session *types.Session, turn *renderedTurn,
) *CompletionMeta {
	return &CompletionMeta{
		Warnings:      turn.Warnings,
		IgnoredParams: ignoredSamplingParams,
		AgentID:       agent.ID,
		SessionID:     session.ID,
	}
}

// writeBridgeResponse answers finish_reason="tool_calls" in the active
// protocol shell: the suspended run's pending tool calls go back to the
// caller; the next request with matching tool_call_id results resumes the
// run.
func (h *GatewayHandler) writeBridgeResponse(
	c *gin.Context, proto, model string, outcome turnOutcome, meta *CompletionMeta,
) {
	key := toolBridgeKey(h.gateway)
	calls := outcome.bridge.outbound(key, time.Now())
	if proto == protoAnthropic {
		respondAnthropicBridge(c, model, outcome.content, calls, outcome.usage)
		return
	}
	toolCalls := make([]ClientToolCall, 0, len(calls))
	for _, call := range calls {
		toolCalls = append(toolCalls, ClientToolCall{
			ID:   call.WireID,
			Type: "function",
			Function: ClientToolFnCall{
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	c.JSON(http.StatusOK, ChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.New().String(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{{
			Index:        0,
			Message:      &RespMessage{Role: "assistant", Content: outcome.content, ToolCalls: toolCalls},
			FinishReason: FinishToolCalls,
		}},
		Usage: outcome.usage,
		Meta:  meta,
	})
}

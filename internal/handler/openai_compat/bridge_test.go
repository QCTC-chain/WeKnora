package openai_compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/agent"
	"github.com/Tencent/WeKnora/internal/agent/hitl"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- pure-function tests: call-ID codec, validation, result decoding ------

func TestBridgeCallIDRoundTrip(t *testing.T) {
	key := []byte("test-bridge-key")
	now := time.Now()
	expiry := now.Add(time.Hour)
	wire := encodeBridgeCallID(key, "prompt-1", "call-b", &expiry, now)
	require.True(t, strings.HasPrefix(wire, bridgeCallIDPrefix), wire)

	call, err := decodeBridgeCallID(key, wire, now)
	require.NoError(t, err)
	assert.Equal(t, "prompt-1", call.PromptID)
	assert.Equal(t, "call-b", call.ToolCallID)
	assert.WithinDuration(t, expiry, call.ExpiresAt, time.Second)

	// A wrong key fails the MAC.
	_, err = decodeBridgeCallID([]byte("other-key"), wire, now)
	require.Error(t, err)

	// Tampering with the payload fails too.
	_, err = decodeBridgeCallID(key, wire[:len(wire)-2]+"xx", now)
	require.Error(t, err)

	// Expired IDs are rejected with an explicit error.
	_, err = decodeBridgeCallID(key, wire, expiry.Add(time.Minute))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired")
}

func TestValidateClientTools(t *testing.T) {
	good := []ClientToolDefinition{{
		Type: "function",
		Function: ClientToolFnSpec{
			Name: "read_file", Description: "Reads a file",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}}
	specs, err := validateClientTools(good)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, "read_file", specs[0].Name)

	// nil stays nil: no bridge machinery for tool-less requests.
	specs, err = validateClientTools(nil)
	require.NoError(t, err)
	assert.Nil(t, specs)

	cases := []struct {
		name string
		defs []ClientToolDefinition
		want string
	}{
		{"bad type", []ClientToolDefinition{{Type: "web_search"}}, "unsupported type"},
		{"empty name", []ClientToolDefinition{{Function: ClientToolFnSpec{Parameters: json.RawMessage(`{}`)}}}, "name must be"},
		{
			"duplicate", []ClientToolDefinition{
				{Function: ClientToolFnSpec{Name: "a"}}, {Function: ClientToolFnSpec{Name: "a"}},
			},
			"duplicate",
		},
		{
			"params not an object",
			[]ClientToolDefinition{{Function: ClientToolFnSpec{Name: "a", Parameters: json.RawMessage(`[1]`)}}},
			"JSON object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateClientTools(tc.defs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	// The per-request cap holds.
	many := make([]ClientToolDefinition, types.MaxClientToolsPerRequest+1)
	for i := range many {
		many[i] = ClientToolDefinition{Function: ClientToolFnSpec{Name: strings.Repeat("t", 8) + string(rune('a'+i%26)) + string(rune('a'+i/26))}}
	}
	_, err = validateClientTools(many)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many tools")
}

func TestDecodeBridgeResultMap(t *testing.T) {
	key := []byte("k")
	now := time.Now()
	expiry := now.Add(time.Hour)
	wire := encodeBridgeCallID(key, "p1", "call-b", &expiry, now)

	msgs := []ChatMessage{
		{Role: "user", Content: ContentUnion{Text: "go"}},
		{Role: "tool", ToolCallID: wire, Content: ContentUnion{Text: "file body"}},
	}
	results, err := decodeBridgeResultMap(key, msgs, now)
	require.NoError(t, err)
	r, ok := results.byInternalID("call-b")
	require.True(t, ok)
	assert.Equal(t, "file body", r.content)
	assert.Equal(t, "p1", r.call.PromptID)

	// A tool message without an ID is a protocol error.
	_, err = decodeBridgeResultMap(key, []ChatMessage{{Role: "tool", Content: ContentUnion{Text: "x"}}}, now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "without tool_call_id")

	// A forged ID fails the MAC.
	_, err = decodeBridgeResultMap(key, []ChatMessage{{Role: "tool", ToolCallID: bridgeCallIDPrefix + "junk"}}, now)
	require.Error(t, err)
}

// --- handler-level tests: suspension → tool_calls, resume entry points ----

// bridgeScript makes the fake agent raise one external_tool_call batch and
// end the run suspended, exactly like the engine does for client tools.
func bridgeScript(bus *event.EventBus, _ *types.QARequest) error {
	emit := func(typ event.EventType, data interface{}) error {
		return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
	}
	request, err := json.Marshal(types.ExternalToolCallPromptRequest{Calls: []types.ExternalToolCallSpec{{
		CallID: "call-b", Name: "read_file", WireName: "read_file", Arguments: `{"path":"/a"}`,
	}}})
	if err != nil {
		return err
	}
	if err := emit(event.EventAgentPromptRequired, event.AgentPromptRequiredData{
		PendingID: "p-ext", Kind: types.AgentPromptKindExternalToolCall,
		SessionID: "sess-x", AssistantMessageID: "assistant-1",
		Request: request, Suspendable: true, ToolCallID: "call-b",
	}); err != nil {
		return err
	}
	expiry := time.Now().Add(time.Hour)
	return emit(event.EventAgentComplete, event.AgentCompleteData{
		Suspended: &types.PromptSuspension{
			PromptID: "p-ext", Kind: types.AgentPromptKindExternalToolCall,
			ExpiresAt: &expiry,
		},
	})
}

func gatewayAgentWithPolicy(id, policy string) *types.CustomAgent {
	agent := publishedAgent(id, "Doc QA", true)
	agent.Config.GatewayToolPolicy = policy
	return agent
}

func TestChatCompletionsSuspendsAsToolCalls(t *testing.T) {
	body := `{"model":"agt-1","stream":false,` +
		`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],` +
		`"messages":[{"role":"user","content":"read it"}]}`
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{gatewayAgentWithPolicy("agt-1", "")}},
		sessions: &fakeGatewaySessions{script: bridgeScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, body, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Choices)
	assert.Equal(t, FinishToolCalls, resp.Choices[0].FinishReason)
	require.NotNil(t, resp.Choices[0].Message)
	require.Len(t, resp.Choices[0].Message.ToolCalls, 1)
	call := resp.Choices[0].Message.ToolCalls[0]
	assert.Equal(t, "function", call.Type)
	assert.Equal(t, "read_file", call.Function.Name)
	assert.True(t, strings.HasPrefix(call.ID, bridgeCallIDPrefix), "the wire ID must carry the gateway envelope")
	// The ID decodes back to the engine's call.
	decoded, err := decodeBridgeCallID([]byte(gatewayBridgeTestKey), call.ID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, "call-b", decoded.ToolCallID)
	require.NotNil(t, resp.Meta)
	assert.True(t, resp.Meta.BridgePending)

	// The suspended assistant message stays open for the resume.
	fakes.messages.mu.Lock()
	completed := len(fakes.messages.updated) > 0 && fakes.messages.updated[len(fakes.messages.updated)-1].IsCompleted
	fakes.messages.mu.Unlock()
	assert.False(t, completed, "a suspended run must not finalize its assistant message")
}

func TestChatCompletionsSuspendsAsToolCallsStream(t *testing.T) {
	body := `{"model":"agt-1","stream":true,` +
		`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],` +
		`"messages":[{"role":"user","content":"read it"}]}`
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{gatewayAgentWithPolicy("agt-1", "")}},
		sessions: &fakeGatewaySessions{script: bridgeScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, body, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	payload := rec.Body.String()
	assert.Contains(t, payload, `"tool_calls"`, "the finish chunk carries reason tool_calls")
	assert.Contains(t, payload, `"delta":{"tool_calls"`, "tool-call deltas stream like OpenAI")
	assert.True(t, strings.Contains(payload, bridgeCallIDPrefix), "the wire ID appears in the delta")
	assert.Contains(t, payload, "[DONE]")
}

func TestChatCompletionsBridgeResumeWithoutBroker(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	wire := encodeBridgeCallID([]byte(gatewayBridgeTestKey), "p-ext", "call-b",
		&expiry, time.Now())
	body := `{"model":"agt-1","stream":false,"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],` +
		`"messages":[{"role":"user","content":"go"},{"role":"tool","tool_call_id":"` + wire + `","content":"file body"}]}`
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{gatewayAgentWithPolicy("agt-1", "")}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, body, "")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "a degraded instance answers 503, not a fresh turn")
	assert.Contains(t, rec.Body.String(), "bridge_unavailable")
}

func TestAnthropicMessagesSuspendsAsToolUse(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{gatewayAgentWithPolicy("agt-1", "")}}
	body := `{"model":"agt-1","max_tokens":64,` +
		`"tools":[{"name":"read_file","description":"Reads a file","input_schema":{"type":"object"}}],` +
		`"messages":[{"role":"user","content":"read it"}]}`
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil), body,
		func(bus *event.EventBus, req *types.QARequest) error { return bridgeScript(bus, req) })
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "tool_use", resp.StopReason)
	require.Len(t, resp.Content, 1)
	assert.Equal(t, "tool_use", resp.Content[0].Type)
	assert.Equal(t, "read_file", resp.Content[0].Name)
	assert.True(t, strings.HasPrefix(resp.Content[0].ID, bridgeCallIDPrefix))
}

// gatewayBridgeTestKey is the zero-config HMAC key: no configured secret →
// the built-in pepper.
var gatewayBridgeTestKey = toolBridgePepper

// TestChatCompletionsBridgeResumeEndToEnd drives the full resume path with a
// real memory-backed hitl broker: a suspended external_tool_call prompt is
// answered by a follow-up request carrying the wc1_ call ID, and the resumed
// run streams to the final answer.
func TestChatCompletionsBridgeResumeEndToEnd(t *testing.T) {
	ctx := context.Background()
	tenant := uint64(42)
	broker := hitl.NewBroker(nil, nil)
	broker.RegisterKind(types.AgentPromptKindExternalToolCall, types.ValidateExternalToolCallResponse)

	// The state the first round left behind: one suspended prompt with a
	// single call, the run snapshot keyed by assistant message, the open
	// assistant message, and the gateway session bound to the agent.
	now := time.Now()
	expiry := now.Add(time.Hour)
	promptReq, err := json.Marshal(types.ExternalToolCallPromptRequest{Calls: []types.ExternalToolCallSpec{{
		CallID: "call-b", Name: "read_file", WireName: "read_file", Arguments: `{"path":"/a"}`,
	}}})
	require.NoError(t, err)
	prompt := &types.AgentPrompt{
		ID: "p-ext", TenantID: tenant, SessionID: "sess-x", AssistantMessageID: "assistant-1",
		Kind: types.AgentPromptKindExternalToolCall, Suspendable: true,
		Request: promptReq, Status: types.AgentPromptSuspended, ExpiresAt: &expiry,
	}
	require.NoError(t, broker.Store().Create(ctx, prompt))

	rreq := types.ResumableAgentRequest{
		Query: "read it",
		ClientTools: []types.ClientToolSpec{{
			Name: "read_file", Description: "Reads a file",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
	rreqJSON, err := json.Marshal(rreq)
	require.NoError(t, err)
	snapshot, err := json.Marshal(agent.RunSnapshot{
		Version: agent.RunSnapshotVersion, Request: rreqJSON, Query: "read it",
	})
	require.NoError(t, err)
	require.NoError(t, broker.Suspensions().Save(ctx, &types.AgentRunSuspension{
		AssistantMessageID: "assistant-1", TenantID: tenant, SessionID: "sess-x",
		PromptID: "p-ext", SnapshotVersion: agent.RunSnapshotVersion, Snapshot: snapshot,
	}))

	sessions := &fakeGatewaySessions{script: answerScript}
	sessions.existing = &types.Session{
		ID: "sess-x", TenantID: tenant, Kind: types.SessionKindGateway,
		LastRequestState: &types.SessionLastRequestState{AgentID: "agt-1"},
	}
	messages := &fakeGatewayMessages{}
	openAssistant := &types.Message{
		ID: "assistant-1", SessionID: "sess-x", Role: "assistant",
		RunState: types.MessageRunStateAwaitingInput,
	}
	messages.created = append(messages.created, openAssistant)

	agents := &fakeAgentService{agents: []*types.CustomAgent{gatewayAgentWithPolicy("agt-1", "")}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(agents, sessions, messages, nil, nil, nil, nil, broker, testGatewayCfg)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), types.TenantIDContextKey, tenant))
		c.Next()
	}, handler.ChatCompletions)

	// The follow-up encodes the call ID with exactly the key the handler
	// derives (tests swap testGatewayCfg, so read it back at call time).
	wire := encodeBridgeCallID(toolBridgeKey(handler.gateway), "p-ext", "call-b", &expiry, now)
	body := `{"model":"agt-1","stream":false,` +
		`"messages":[{"role":"user","content":"read it"},` +
		`{"role":"tool","tool_call_id":"` + wire + `","content":"file body"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Choices)
	assert.Equal(t, FinishStop, resp.Choices[0].FinishReason)
	assert.Contains(t, resp.Choices[0].Message.Content, "答案")

	// The run consumed the prompt and the suspension state.
	resolved, err := broker.Store().Get(ctx, "p-ext")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, types.AgentPromptAnswered, resolved.Status)
	assert.Contains(t, string(resolved.Response), "file body")

	// The resumed run reused the assistant message row and closed it.
	fakesMsg := messages.updated[len(messages.updated)-1]
	assert.Equal(t, "assistant-1", fakesMsg.ID)
	assert.True(t, fakesMsg.IsCompleted)

	// The QA request carried the resume payload and frozen client tools.
	require.NotEmpty(t, sessions.qaRequests)
	qa := sessions.qaRequests[len(sessions.qaRequests)-1]
	require.NotNil(t, qa.Resume)
	assert.Equal(t, "p-ext", qa.Resume.Prompt.ID)
	require.Len(t, qa.ClientTools, 1)
	assert.Equal(t, "read_file", qa.ClientTools[0].Name)
}

// TestChatCompletionsBridgeResumeWrongAgent proves the suspended run can only
// be resumed by the same model: a different published agent gets 409.
func TestChatCompletionsBridgeResumeWrongAgent(t *testing.T) {
	ctx := context.Background()
	tenant := uint64(42)
	broker := hitl.NewBroker(nil, nil)
	broker.RegisterKind(types.AgentPromptKindExternalToolCall, types.ValidateExternalToolCallResponse)
	now := time.Now()
	expiry := now.Add(time.Hour)
	promptReq, err := json.Marshal(types.ExternalToolCallPromptRequest{Calls: []types.ExternalToolCallSpec{{
		CallID: "call-b", Name: "read_file", WireName: "read_file", Arguments: `{}`,
	}}})
	require.NoError(t, err)
	require.NoError(t, broker.Store().Create(ctx, &types.AgentPrompt{
		ID: "p-ext", TenantID: tenant, SessionID: "sess-x", AssistantMessageID: "assistant-1",
		Kind: types.AgentPromptKindExternalToolCall, Status: types.AgentPromptSuspended,
		Request: promptReq, ExpiresAt: &expiry,
	}))

	// The run suspended on agt-1's session; asking a DIFFERENT published
	// model to continue it must fail closed without consuming the prompt.
	sessions := &fakeGatewaySessions{}
	sessions.existing = &types.Session{
		ID: "sess-x", TenantID: tenant, Kind: types.SessionKindGateway,
		LastRequestState: &types.SessionLastRequestState{AgentID: "agt-1"},
	}
	agents := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgentWithPolicy("agt-1", ""), gatewayAgentWithPolicy("agt-other", ""),
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(agents, sessions, &fakeGatewayMessages{}, nil, nil, nil, nil, broker, testGatewayCfg)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), types.TenantIDContextKey, tenant))
		c.Next()
	}, handler.ChatCompletions)

	wire := encodeBridgeCallID(toolBridgeKey(handler.gateway), "p-ext", "call-b", &expiry, now)
	body := `{"model":"agt-other","stream":false,` +
		`"messages":[{"role":"user","content":"read it"},` +
		`{"role":"tool","tool_call_id":"` + wire + `","content":"file body"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "model_mismatch")

	// The failed resume leaves the prompt suspended and answerable.
	kept, err := broker.Store().Get(ctx, "p-ext")
	require.NoError(t, err)
	require.NotNil(t, kept)
	assert.Equal(t, types.AgentPromptSuspended, kept.Status)
}

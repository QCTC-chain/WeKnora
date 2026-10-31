package openai_compat

// M2-5 Anthropic shell tests: request conversion, non-streaming message
// object, SSE event grammar, protocol error envelopes, and the shared
// machinery (idempotency claim, session header) running over /v1/messages.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// serveMessagesWithScope is serveCompletionsWithScope's Anthropic twin:
// full turn flow over POST /v1/messages with a scripted session.
func serveMessagesWithScope(
	t *testing.T, svc *fakeAgentService, usage interfaces.GatewayUsageService,
	scope *types.TenantAPIKeyScope, body string, script func(*event.EventBus, *types.QARequest) error,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(svc, &fakeGatewaySessions{script: script}, &fakeGatewayMessages{}, nil, nil, usage, nil, nil, nil)
	router.POST("/v1/messages", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		if scope != nil {
			ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, *scope)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.AnthropicMessages)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// simpleMessagesBody is a minimal Anthropic Messages request.
func simpleMessagesBody(stream bool) string {
	body := `{"model":"agt-1","max_tokens":1024,"messages":[{"role":"user","content":"什么是知识库?"}],"metadata":{"user_id":"cron-job"}}`
	if stream {
		return `{"model":"agt-1","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"什么是知识库?"}]}`
	}
	return body
}

func TestAnthropicMessagesNonStream(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil),
		simpleMessagesBody(false), answerScript)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "sess-1", rec.Header().Get("X-WeKnora-Session-Id"))

	var body struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "message", body.Type)
	require.Equal(t, "assistant", body.Role)
	require.Equal(t, "agt-1", body.Model)
	require.Equal(t, "end_turn", body.StopReason)
	require.True(t, strings.HasPrefix(body.ID, "msg_"), "message id uses the Anthropic msg_ prefix")
	require.Len(t, body.Content, 1)
	require.Equal(t, "text", body.Content[0].Type)
	require.Contains(t, body.Content[0].Text, "部分")
	require.Contains(t, body.Content[0].Text, "答案")
	require.EqualValues(t, 120, body.Usage.InputTokens)
	require.EqualValues(t, 30, body.Usage.OutputTokens)
}

func TestAnthropicMessagesStreamSSE(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil),
		simpleMessagesBody(true), answerScript)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")

	raw := rec.Body.String()
	require.Contains(t, raw, "event: message_start")
	require.Contains(t, raw, `"type":"message_start"`)
	require.Contains(t, raw, "event: content_block_start")
	require.Contains(t, raw, `"type":"content_block_delta"`)
	require.Contains(t, raw, `"type":"text_delta"`)
	require.Contains(t, raw, "部分")
	require.Contains(t, raw, "答案")
	require.Contains(t, raw, "event: message_delta")
	require.Contains(t, raw, `"stop_reason":"end_turn"`)
	require.Contains(t, raw, "event: message_stop")

	// Events must appear in protocol order.
	start := strings.Index(raw, "event: message_start")
	delta := strings.Index(raw, "event: content_block_delta")
	mdelta := strings.Index(raw, "event: message_delta")
	stop := strings.LastIndex(raw, "event: message_stop")
	require.GreaterOrEqual(t, delta, 0)
	require.Greater(t, delta, start)
	require.Greater(t, mdelta, delta)
	require.Greater(t, stop, mdelta)
}

func TestAnthropicMessagesThinkingDeltas(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	// A reasoning script: thought event then final answer.
	script := func(bus *event.EventBus, _ *types.QARequest) error {
		emit := func(typ event.EventType, data interface{}) error {
			return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
		}
		if err := emit(event.EventAgentThought, event.AgentThoughtData{Content: "推理中"}); err != nil {
			return err
		}
		if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "结论", Done: true}); err != nil {
			return err
		}
		return emit(event.EventAgentComplete, event.AgentCompleteData{FinalAnswer: "结论"})
	}
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil),
		simpleMessagesBody(true), script)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.String()
	require.Contains(t, raw, `"type":"thinking_delta"`)
	require.Contains(t, raw, "推理中")
	require.Contains(t, raw, `"type":"text_delta"`)
}

func TestAnthropicMessagesModelNotFound404(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil),
		`{"model":"nope","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "error", body.Type)
	require.Equal(t, "not_found_error", body.Error.Type)
	require.Contains(t, body.Error.Message, "nope")
}

func TestAnthropicMessagesBadBody400(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	// Malformed JSON must answer the Anthropic error envelope.
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil),
		`{"model":"agt-1","max_tokens":8,"messages":"not-an-array"}`, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"type":"error"`)
	require.Contains(t, rec.Body.String(), "invalid_request_error")
}

func TestAnthropicMessagesImageAndToolBlocks(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	smallPNG := "iVBORw0KGgoAAAANSUhEUg=="
	body := `{"model":"agt-1","max_tokens":64,"system":"你是文档助手",
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + smallPNG + `"}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"kb"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"共 3 条结果"}]},
			{"role":"user","content":[{"type":"text","text":"总结"}]}
		]}`
	var qaReq *types.QARequest
	rec := serveMessagesWithScope(t, svc, nil, keyScope(nil, nil, nil), body,
		func(bus *event.EventBus, req *types.QARequest) error {
			qaReq = req
			return answerScript(bus, req)
		})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// The turn text carries the tool traffic textualized the M1 way; the
	// tool_result is named after the earlier tool_use id.
	require.NotNil(t, qaReq)
	require.Contains(t, qaReq.Query, "[客户端工具 lookup] → 共 3 条结果")
	require.Contains(t, qaReq.Query, "[对话历史]")
}

func TestAnthropicConvertRequest(t *testing.T) {
	raw := []byte(`{
		"model":"agt-1","max_tokens":32,"stream":true,
		"system":[{"type":"text","text":"s1"},{"type":"text","text":"s2"}],
		"metadata":{"user_id":"u-ext"},
		"tools":[{"name":"lookup"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aGk="}}]},
			{"role":"assistant","content":"好的"},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu9","content":[{"type":"text","text":"r1"}]}]}
		]}`)
	var req AnthropicMessagesRequest
	require.NoError(t, json.Unmarshal(raw, &req))
	conv, err := req.toOpenAI()
	require.NoError(t, err)
	require.Equal(t, "agt-1", conv.Model)
	require.True(t, conv.Stream)
	require.Equal(t, "u-ext", conv.User)
	require.Len(t, conv.Tools, 1, "tools counted as ignored")
	// system blocks flatten into one system message.
	require.Equal(t, "system", conv.Messages[0].Role)
	require.Equal(t, "s1\ns2", conv.Messages[0].Content.Text)
	// user: text + base64 image → data URL part.
	require.Equal(t, "user", conv.Messages[1].Role)
	require.Len(t, conv.Messages[1].Content.Parts, 2)
	require.Equal(t, "image_url", conv.Messages[1].Content.Parts[1].Type)
	require.Equal(t, "data:image/jpeg;base64,aGk=", conv.Messages[1].Content.Parts[1].ImageURL.URL)
	// assistant string content stays plain.
	require.Equal(t, "assistant", conv.Messages[2].Role)
	require.Equal(t, "好的", conv.Messages[2].Content.Text)
	// tool_result becomes a tool-role message tied to the call id.
	require.Equal(t, "tool", conv.Messages[4].Role)
	require.Equal(t, "tu9", conv.Messages[4].ToolCallID)
	require.Equal(t, "r1", conv.Messages[4].Content.Text)
}

func TestAnthropicMessagesIdempotentReplay(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	gin.SetMode(gin.TestMode)
	store := newFakeIdempotencyStore()
	handler := NewGatewayHandler(svc, &fakeGatewaySessions{script: answerScript},
		&fakeGatewayMessages{}, nil, nil, &fakeGatewayUsage{}, store, nil, nil)
	router := gin.New()
	router.POST("/v1/messages", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, *keyScope(nil, nil, nil))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.AnthropicMessages)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(simpleMessagesBody(false)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "anthro-1")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	first := send()
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.Empty(t, first.Header().Get("X-WeKnora-Idempotent-Replay"))

	replay := send()
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	require.Equal(t, "true", replay.Header().Get("X-WeKnora-Idempotent-Replay"))
	require.Equal(t, first.Body.String(), replay.Body.String(), "replay returns the stored Anthropic message")
}

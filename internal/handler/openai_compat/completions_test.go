package openai_compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGatewaySessions implements the SessionService slice the gateway uses:
// session creation, owner binding, and the AgentQA run (which replays a
// script of events on the bus it is handed).
type fakeGatewaySessions struct {
	interfaces.SessionService

	mu          sync.Mutex
	created     *types.Session
	ownerID     string
	ownerTenant uint64
	qaRequests  []*types.QARequest

	script func(bus *event.EventBus, req *types.QARequest) error

	// M2-3 continuation state: thread is what FindGatewayThreadSession /
	// GetSessionByID return; touched records TouchGatewaySession calls.
	thread  *types.Session
	touched []string
}

func (f *fakeGatewaySessions) CreateSession(_ context.Context, s *types.Session) (*types.Session, error) {
	out := *s
	out.ID = "sess-1"
	f.mu.Lock()
	f.created = &out
	f.mu.Unlock()
	return &out, nil
}

func (f *fakeGatewaySessions) SetSessionOwnerID(_ context.Context, tenantID uint64, _ string, ownerID string) error {
	f.mu.Lock()
	f.ownerTenant = tenantID
	f.ownerID = ownerID
	f.mu.Unlock()
	return nil
}

func (f *fakeGatewaySessions) AgentQA(_ context.Context, req *types.QARequest, bus *event.EventBus) error {
	f.mu.Lock()
	f.qaRequests = append(f.qaRequests, req)
	f.mu.Unlock()
	if f.script == nil {
		return nil
	}
	return f.script(bus, req)
}

// fakeGatewayMessages implements the MessageService slice the gateway uses.
type fakeGatewayMessages struct {
	interfaces.MessageService

	mu        sync.Mutex
	nextID    int
	created   []*types.Message
	updated   []*types.Message
	updateErr error
}

func (f *fakeGatewayMessages) CreateMessage(_ context.Context, m *types.Message) (*types.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	out := *m
	out.ID = fmt.Sprintf("%s-%d", m.Role, f.nextID)
	f.created = append(f.created, &out)
	return &out, nil
}

func (f *fakeGatewayMessages) UpdateMessage(_ context.Context, m *types.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	cp := *m
	f.updated = append(f.updated, &cp)
	return nil
}

func answerScript(bus *event.EventBus, _ *types.QARequest) error {
	emit := func(typ event.EventType, data interface{}) error {
		return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
	}
	if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "部分"}); err != nil {
		return err
	}
	if err := emit(event.EventAgentReferences, event.AgentReferencesData{
		References: []*types.SearchResult{{ID: "r1", KnowledgeID: "k1", KnowledgeTitle: "手册", ChunkIndex: 2}},
	}); err != nil {
		return err
	}
	if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "答案", Done: true}); err != nil {
		return err
	}
	return emit(event.EventAgentComplete, event.AgentCompleteData{
		FinalAnswer: "答案",
		Usage:       &types.TokenUsage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
	})
}

type completionsFakes struct {
	agents   *fakeAgentService
	sessions *fakeGatewaySessions
	messages *fakeGatewayMessages
}

// serveCompletions mounts ChatCompletions with injected tenant + user context
// (what GatewayAuth attaches) and returns the recorder.
func serveCompletions(
	t *testing.T, fakes completionsFakes, body string, userID string,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := newTestHandler(fakes.agents, fakes.sessions, fakes.messages)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		if userID != "" {
			ctx = context.WithValue(ctx, types.UserIDContextKey, userID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func simpleCompletionsBody() string {
	return `{"model":"agt-1","stream":false,"messages":[` +
		`{"role":"user","content":"武汉今天天气如何?"}]}`
}

func TestChatCompletionsHappyPath(t *testing.T) {
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, "agt-1", rec.Header().Get("X-WeKnora-Agent-Id"))
	assert.Equal(t, "sess-1", rec.Header().Get("X-WeKnora-Session-Id"))
	assert.Equal(t, GatewayVersion, rec.Header().Get("X-WeKnora-Gateway-Version"))

	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "chat.completion", resp.Object)
	assert.True(t, strings.HasPrefix(resp.ID, "chatcmpl-"))
	assert.Equal(t, "agt-1", resp.Model)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, 0, resp.Choices[0].Index)
	assert.Equal(t, FinishStop, resp.Choices[0].FinishReason)
	require.NotNil(t, resp.Choices[0].Message)
	assert.Equal(t, "assistant", resp.Choices[0].Message.Role)
	assert.Equal(t, "部分答案\n---\n参考来源:\n1. 手册", resp.Choices[0].Message.Content)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, int64(120), resp.Usage.PromptTokens)
	assert.Equal(t, int64(30), resp.Usage.CompletionTokens)
	assert.Equal(t, int64(150), resp.Usage.TotalTokens)
	require.NotNil(t, resp.Meta)
	assert.Equal(t, "agt-1", resp.Meta.AgentID)
	assert.Equal(t, "sess-1", resp.Meta.SessionID)

	// Temp session: gateway kind, gateway title, owner bound to the caller.
	sessions := fakes.sessions
	sessions.mu.Lock()
	created, ownerID, ownerTenant := sessions.created, sessions.ownerID, sessions.ownerTenant
	sessions.mu.Unlock()
	require.NotNil(t, created)
	assert.Equal(t, types.SessionKindGateway, created.Kind)
	assert.Equal(t, uint64(42), created.TenantID)
	assert.True(t, strings.HasPrefix(created.Title, "gateway:agt-1"))
	assert.Equal(t, "user-9", ownerID)
	assert.Equal(t, uint64(42), ownerTenant)

	// Message rows: user message carries the rendered query; assistant row
	// finalized with the aggregated answer and references.
	fakes.messages.mu.Lock()
	createdMsgs, updated := fakes.messages.created, fakes.messages.updated
	fakes.messages.mu.Unlock()
	require.Len(t, createdMsgs, 2)
	assert.Equal(t, "user", createdMsgs[0].Role)
	assert.Contains(t, createdMsgs[0].Content, "[本次问题]\n武汉今天天气如何?")
	assert.Equal(t, gatewayChannel, createdMsgs[0].Channel)
	require.Len(t, updated, 1)
	assert.Equal(t, "部分答案\n---\n参考来源:\n1. 手册", updated[0].Content)
	assert.True(t, updated[0].IsCompleted)
	require.Len(t, updated[0].KnowledgeReferences, 1)
}

func TestChatCompletionsQARequestSemantics(t *testing.T) {
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "user-9")
	require.Equal(t, http.StatusOK, rec.Code)

	fakes.sessions.mu.Lock()
	reqs := fakes.sessions.qaRequests
	fakes.sessions.mu.Unlock()
	require.Len(t, reqs, 1)
	req := reqs[0]
	assert.False(t, req.TurnLeaseHeld, "the service takes the turn lease itself (IM/MCP parity)")
	assert.False(t, req.Suspendable, "gateway turns never suspend")
	assert.Equal(t, types.RunMode(""), req.Mode, "unattended: nobody can answer raised prompts")
	assert.Same(t, fakes.agents.agents[0], req.CustomAgent)
	assert.Equal(t, "sess-1", req.Session.ID)
	assert.Equal(t, "user-1", req.UserMessageID)
	assert.Equal(t, "assistant-2", req.AssistantMessageID)
}

func TestChatCompletionsNameAliasAnd404(t *testing.T) {
	fakes := func() completionsFakes {
		return completionsFakes{
			agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
			sessions: &fakeGatewaySessions{script: answerScript},
			messages: &fakeGatewayMessages{},
		}
	}

	rec := serveCompletions(t, fakes(),
		`{"model":" DOC qa ","stream":false,"messages":[{"role":"user","content":"hi"}]}`, "")
	require.Equal(t, http.StatusOK, rec.Code)

	rec = serveCompletions(t, fakes(),
		`{"model":"ghost","stream":false,"messages":[{"role":"user","content":"hi"}]}`, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	var env ErrorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "model_not_found", env.Error.Code)

	unpublished := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-2", "Hidden", false)}},
		sessions: &fakeGatewaySessions{},
		messages: &fakeGatewayMessages{},
	}
	rec = serveCompletions(t, unpublished,
		`{"model":"Hidden","stream":false,"messages":[{"role":"user","content":"hi"}]}`, "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestChatCompletionsRequestValidation(t *testing.T) {
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{},
		messages: &fakeGatewayMessages{},
	}
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, 400, ""},
		{"missing messages", `{"model":"agt-1"}`, 400, ""},
		{"no user turn", `{"model":"agt-1","messages":[{"role":"system","content":"x"}]}`, 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveCompletions(t, fakes, tc.body, "")
			require.Equal(t, tc.status, rec.Code)
			var env ErrorEnvelope
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			assert.Equal(t, tc.code, env.Error.Code)
		})
	}
	// A malformed JSON body is also a 400.
	rec := serveCompletions(t, fakes, "{not json", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestChatCompletionsAgentFailureEnvelope(t *testing.T) {
	failing := &fakeGatewaySessions{script: func(bus *event.EventBus, _ *types.QARequest) error {
		return bus.Emit(context.Background(), event.Event{
			Type: event.EventError, Data: event.ErrorData{Error: "LLM provider down"},
		})
	}}
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: failing,
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var env ErrorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "server_error", env.Error.Type)

	// The assistant row is finalized with the failure note even on error.
	fakes.messages.mu.Lock()
	updated := fakes.messages.updated
	fakes.messages.mu.Unlock()
	require.Len(t, updated, 1)
	assert.True(t, updated[0].IsCompleted)
	assert.Contains(t, updated[0].Content, "LLM provider down")
}

func TestChatCompletionsAgentQAReturnError(t *testing.T) {
	failing := &fakeGatewaySessions{script: func(*event.EventBus, *types.QARequest) error {
		return errors.New("boom")
	}}
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: failing,
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestChatCompletionsCitationsAndDisabledCitations(t *testing.T) {
	citing := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, citing, simpleCompletionsBody(), "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Choices[0].Message.Content, "参考来源:")
	assert.Contains(t, resp.Choices[0].Message.Content, "1. 手册")

	// Explicitly disabled citations suppress the source block.
	agent := publishedAgent("agt-1", "Doc QA", true)
	disabled := false
	agent.Config.CitationEnabled = &disabled
	noCite := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{agent}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec = serveCompletions(t, noCite, simpleCompletionsBody(), "")
	require.Equal(t, http.StatusOK, rec.Code)
	resp = ChatCompletionResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotContains(t, resp.Choices[0].Message.Content, "参考来源:")
}

func TestChatCompletionsImageRouting(t *testing.T) {
	body := `{"model":"agt-1","stream":false,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看这张图"},{"type":"image_url","image_url":{"url":"https://x.test/a.png"}}]}]}`
	withImages := publishedAgent("agt-1", "Vision", true)
	withImages.Config.ImageUploadEnabled = true
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{withImages}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	fakes.sessions.mu.Lock()
	reqs := fakes.sessions.qaRequests
	fakes.sessions.mu.Unlock()
	require.Len(t, reqs, 1)
	assert.Equal(t, []string{"https://x.test/a.png"}, reqs[0].ImageURLs)

	// Same request against a text-only agent: images dropped with a warning.
	textOnly := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec = serveCompletions(t, textOnly, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Meta)
	assert.Empty(t, reqs2ImageURLs(t, textOnly))
	assert.Contains(t, resp.Meta.Warnings[0], "图片已忽略")
}

func reqs2ImageURLs(t *testing.T, fakes completionsFakes) []string {
	t.Helper()
	fakes.sessions.mu.Lock()
	defer fakes.sessions.mu.Unlock()
	require.Len(t, fakes.sessions.qaRequests, 1)
	return fakes.sessions.qaRequests[0].ImageURLs
}

func TestChatCompletionsClientToolsIgnored(t *testing.T) {
	body := `{"model":"agt-1","stream":false,"tools":[{"type":"function","function":{"name":"read_file","parameters":{}}}],` +
		`"temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, body, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Meta)
	assert.Equal(t, 1, resp.Meta.ClientToolsIgnored)
	assert.Contains(t, resp.Meta.IgnoredParams, "temperature")
	assert.Equal(t, FinishStop, resp.Choices[0].FinishReason, "client tools never produce tool_calls in M1")
}

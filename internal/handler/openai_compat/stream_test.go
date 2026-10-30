package openai_compat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamBody builds a stream=true completions body.
func streamBody(model string) string {
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
}

// sseFrame is one parsed `data: ...` line; comment lines are dropped.
type sseFrame struct {
	Raw string
}

// parseSSE splits a recorder body into data frames ([DONE] included as the
// literal sentinel).
func parseSSE(t *testing.T, body string) []sseFrame {
	t.Helper()
	var frames []sseFrame
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		frames = append(frames, sseFrame{Raw: payload})
	}
	return frames
}

// decodeChunk unmarshals one frame into a chunk, failing the test if it is
// the [DONE] sentinel or an error frame.
func decodeChunk(t *testing.T, f sseFrame) ChatCompletionChunk {
	t.Helper()
	require.NotEqual(t, "[DONE]", f.Raw)
	var chunk ChatCompletionChunk
	require.NoError(t, json.Unmarshal([]byte(f.Raw), &chunk))
	return chunk
}

// streamFakes is the standard fixture: one published agent + answer script.
func streamFakes(script func(*event.EventBus, *types.QARequest) error) completionsFakes {
	return completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: script},
		messages: &fakeGatewayMessages{},
	}
}

// thoughtScript emits one thought, one tool call/result pair, then the answer.
func thoughtScript(bus *event.EventBus, _ *types.QARequest) error {
	emit := func(typ event.EventType, data interface{}) error {
		return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
	}
	if err := emit(event.EventAgentThought, event.AgentThoughtData{Content: "先检索知识库"}); err != nil {
		return err
	}
	if err := emit(event.EventAgentToolCall, event.AgentToolCallData{ToolName: "search", ToolCallID: "t1"}); err != nil {
		return err
	}
	if err := emit(event.EventAgentToolResult, event.AgentToolResultData{
		ToolName: "search", ToolCallID: "t1", Output: "命中 3 条",
	}); err != nil {
		return err
	}
	if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "答案", Done: true}); err != nil {
		return err
	}
	return emit(event.EventAgentComplete, event.AgentCompleteData{
		FinalAnswer: "答案",
		Usage:       &types.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
	})
}

func TestChatCompletionsStreamHappyPath(t *testing.T) {
	fakes := streamFakes(answerScript)
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// SSE headers.
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "agt-1", rec.Header().Get("X-WeKnora-Agent-Id"))
	assert.Equal(t, "sess-1", rec.Header().Get("X-WeKnora-Session-Id"))

	frames := parseSSE(t, rec.Body.String())
	require.NotEmpty(t, frames)
	require.Equal(t, "[DONE]", frames[len(frames)-1].Raw, "stream must end with [DONE]")

	// First frame: assistant role delta.
	first := decodeChunk(t, frames[0])
	assert.Equal(t, "chat.completion.chunk", first.Object)
	assert.True(t, strings.HasPrefix(first.ID, "chatcmpl-"))
	require.Len(t, first.Choices, 1)
	require.NotNil(t, first.Choices[0].Delta)
	assert.Equal(t, "assistant", first.Choices[0].Delta.Role)

	// Content deltas carry the answer; the final frame has finish/usage/meta.
	var content strings.Builder
	var last *ChatCompletionChunk
	for i := 1; i < len(frames)-1; i++ {
		chunk := decodeChunk(t, frames[i])
		if chunk.Choices[0].Delta != nil && chunk.Choices[0].Delta.Content != "" {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
		last = &chunk
	}
	require.NotNil(t, last)
	assert.Equal(t, "部分答案\n---\n参考来源:\n1. 手册", content.String())
	assert.Equal(t, FinishStop, last.Choices[0].FinishReason)
	require.NotNil(t, last.Usage)
	assert.Equal(t, int64(120), last.Usage.PromptTokens)
	assert.Equal(t, int64(150), last.Usage.TotalTokens)
	require.NotNil(t, last.Meta)
	assert.Equal(t, "agt-1", last.Meta.AgentID)
	assert.Equal(t, "sess-1", last.Meta.SessionID)

	// Citations appended like the aggregate path.
	assert.Contains(t, content.String(), "参考来源")
	// The assistant row is persisted with everything the client saw.
	fakes.messages.mu.Lock()
	defer fakes.messages.mu.Unlock()
	var assistant *types.Message
	for _, m := range fakes.messages.updated {
		if m.Role == "assistant" {
			assistant = m
		}
	}
	require.NotNil(t, assistant, "assistant row must be finalized")
	assert.Equal(t, "部分答案\n---\n参考来源:\n1. 手册", assistant.Content)
	assert.True(t, assistant.IsCompleted)
}

// lastString returns the raw payload of the second-to-last frame (the final
// chunk before [DONE]).
func lastString(t *testing.T, frames []sseFrame) string {
	t.Helper()
	require.GreaterOrEqual(t, len(frames), 2)
	return frames[len(frames)-2].Raw
}

func TestChatCompletionsStreamThoughtModes(t *testing.T) {
	thoughts := func(mode string) (reasoning, collapse []string) {
		fakes := streamFakes(thoughtScript)
		old := testGatewayCfg
		cfg := &config.Config{Gateway: &config.GatewayConfig{}}
		switch mode {
		case config.GatewayThoughtModeCollapse:
			cfg.Gateway.ThoughtMode = config.GatewayThoughtModeCollapse
		case config.GatewayThoughtModeDrop:
			cfg.Gateway.ThoughtMode = config.GatewayThoughtModeDrop
		}
		testGatewayCfg = cfg
		defer func() { testGatewayCfg = old }()
		rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
		require.Equal(t, http.StatusOK, rec.Code)
		var reasoningOut, collapseOut []string
		for _, f := range parseSSE(t, rec.Body.String()) {
			if f.Raw == "[DONE]" {
				continue
			}
			var chunk ChatCompletionChunk
			require.NoError(t, json.Unmarshal([]byte(f.Raw), &chunk))
			d := chunk.Choices[0].Delta
			if d == nil {
				continue
			}
			if d.ReasoningContent != "" {
				reasoningOut = append(reasoningOut, d.ReasoningContent)
			}
			if strings.Contains(d.Content, "<think>") {
				collapseOut = append(collapseOut, d.Content)
			}
		}
		return reasoningOut, collapseOut
	}

	reasoning, collapse := thoughts("reasoning")
	require.Len(t, reasoning, 1)
	assert.Equal(t, "先检索知识库", reasoning[0])
	assert.Empty(t, collapse)

	reasoning, collapse = thoughts(config.GatewayThoughtModeCollapse)
	assert.Empty(t, reasoning)
	require.Len(t, collapse, 1)
	assert.Equal(t, "<think>先检索知识库</think>", collapse[0])

	reasoning, collapse = thoughts(config.GatewayThoughtModeDrop)
	assert.Empty(t, reasoning)
	assert.Empty(t, collapse)
}

func TestChatCompletionsStreamToolProgressAndMerge(t *testing.T) {
	fakes := streamFakes(thoughtScript)
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	require.Equal(t, http.StatusOK, rec.Code)

	var toolLines []string
	for _, f := range parseSSE(t, rec.Body.String()) {
		if f.Raw == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(f.Raw), &chunk))
		d := chunk.Choices[0].Delta
		if d != nil && strings.HasPrefix(d.Content, "[工具]") {
			toolLines = append(toolLines, d.Content)
		}
	}
	// call + result for the same tool merge into ONE line (docs §8).
	require.Len(t, toolLines, 1)
	assert.Contains(t, toolLines[0], "search")
	assert.Contains(t, toolLines[0], "命中 3 条")
}

func TestChatCompletionsStreamProgressDrop(t *testing.T) {
	fakes := streamFakes(thoughtScript)
	old := testGatewayCfg
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{ProgressMode: config.GatewayProgressModeDrop}}
	defer func() { testGatewayCfg = old }()

	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	for _, f := range parseSSE(t, rec.Body.String()) {
		if f.Raw == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(f.Raw), &chunk))
		if d := chunk.Choices[0].Delta; d != nil {
			assert.NotContains(t, d.Content, "[工具]", "progress frames are dropped")
		}
	}
}

func TestChatCompletionsStreamAgentError(t *testing.T) {
	fakes := streamFakes(func(bus *event.EventBus, _ *types.QARequest) error {
		return bus.Emit(context.Background(), event.Event{
			Type: event.EventError, Data: event.ErrorData{Error: "LLM provider down"},
		})
	})
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	// Once SSE headers are out the HTTP status is 200; failure is in-band.
	require.Equal(t, http.StatusOK, rec.Code)

	var sawErr, sawDone bool
	for _, f := range parseSSE(t, rec.Body.String()) {
		if f.Raw == "[DONE]" {
			sawDone = true
			continue
		}
		var env struct {
			Error *ErrorBody `json:"error"`
		}
		if json.Unmarshal([]byte(f.Raw), &env) == nil && env.Error != nil {
			sawErr = true
			assert.Contains(t, env.Error.Message, "LLM provider down")
		}
	}
	assert.True(t, sawErr, "an error frame must be emitted")
	assert.True(t, sawDone, "[DONE] still terminates the stream")
}

func TestChatCompletionsStreamDisconnectPersistsPartial(t *testing.T) {
	// partialEmitted fires after the bus Emit for "部分" returned — the
	// sync EventBus guarantees the translator has recorded it by then.
	partialEmitted := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	fakes := streamFakes(func(bus *event.EventBus, _ *types.QARequest) error {
		// Emit partial content, then stall as if the run were long.
		err := bus.Emit(context.Background(), event.Event{
			Type: event.EventAgentFinalAnswer,
			Data: event.AgentFinalAnswerData{Content: "部分"},
		})
		close(partialEmitted)
		if err != nil {
			return err
		}
		<-release
		return bus.Emit(context.Background(), event.Event{
			Type: event.EventAgentFinalAnswer,
			Data: event.AgentFinalAnswerData{Content: "答案", Done: true},
		})
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := newTestHandler(fakes.agents, fakes.sessions, fakes.messages)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)

	// Cancel the request once the partial answer is in the stream: the
	// client hung up mid-turn.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-partialEmitted
		cancel()
	}()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody("agt-1"))).
		WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	// Default cancel_on_disconnect=true: whatever was translated is
	// persisted on the assistant row even though the writer is gone.
	fakes.messages.mu.Lock()
	defer fakes.messages.mu.Unlock()
	var assistant *types.Message
	for _, m := range fakes.messages.updated {
		if m.Role == "assistant" {
			assistant = m
		}
	}
	require.NotNil(t, assistant)
	assert.Equal(t, "部分", assistant.Content)
}

func TestChatCompletionsStreamNoCancelOnDisconnect(t *testing.T) {
	done := make(chan struct{})
	fakes := streamFakes(answerScript)
	old := testGatewayCfg
	noCancel := false
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{CancelOnDisconnect: &noCancel}}
	defer func() { testGatewayCfg = old; close(done) }()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := newTestHandler(fakes.agents, fakes.sessions, fakes.messages)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody("agt-1"))).
		WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	cancel()
	router.ServeHTTP(rec, req)

	// The run still finished and the full answer was persisted.
	fakes.messages.mu.Lock()
	defer fakes.messages.mu.Unlock()
	var assistant *types.Message
	for _, m := range fakes.messages.updated {
		if m.Role == "assistant" {
			assistant = m
		}
	}
	require.NotNil(t, assistant)
	assert.Equal(t, "部分答案\n---\n参考来源:\n1. 手册", assistant.Content)
}

func TestChatCompletionsStreamTimeoutNote(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	fakes := streamFakes(func(bus *event.EventBus, _ *types.QARequest) error {
		// A run that never finishes within the budget.
		<-block
		return nil
	})
	old := testGatewayCfg
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{RequestTimeout: 80 * time.Millisecond}}
	defer func() { testGatewayCfg = old }()

	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "[网关] 响应超时", "timeout note must stream")
	assert.Contains(t, body, "data: [DONE]")

	// The warning lands in the meta of the final chunk too.
	var meta *CompletionMeta
	for _, f := range parseSSE(t, body) {
		if f.Raw == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if json.Unmarshal([]byte(f.Raw), &chunk) == nil && chunk.Meta != nil && len(chunk.Meta.Warnings) > 0 {
			meta = chunk.Meta
		}
	}
	require.NotNil(t, meta)
	assert.Contains(t, meta.Warnings[0], "超时")
}

// fakeGatewayAudit captures audit rows for assertions.
type fakeGatewayAudit struct {
	interfaces.AuditLogService
	mu   sync.Mutex
	rows []*types.AuditLog
	err  error
}

// Log records the row first, then surfaces the configured error — a
// "sink down" that still lets tests inspect what was attempted.
func (f *fakeGatewayAudit) Log(_ context.Context, row *types.AuditLog) error {
	f.mu.Lock()
	f.rows = append(f.rows, row)
	err := f.err
	f.mu.Unlock()
	return err
}

func TestGatewayAuditPerTurn(t *testing.T) {
	audit := &fakeGatewayAudit{}
	gin.SetMode(gin.TestMode)
	fakes := streamFakes(answerScript)
	router := gin.New()
	handler := NewGatewayHandler(fakes.agents, fakes.sessions, fakes.messages, audit, nil)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		// GatewayAuth attaches the key scope; simulate the minimal shape.
		ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, types.TenantAPIKeyScope{
			KeyID: 7, Name: "ci-key",
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)

	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	require.Equal(t, http.StatusOK, do(simpleCompletionsBody()).Code)
	require.Equal(t, http.StatusOK, do(streamBody("agt-1")).Code)

	audit.mu.Lock()
	require.Len(t, audit.rows, 2, "one audit row per turn")
	row := audit.rows[0]
	audit.mu.Unlock()

	assert.Equal(t, types.AuditActionGatewayCompletion, row.Action)
	assert.Equal(t, uint64(42), row.TenantID)
	assert.Equal(t, "gateway.completion", string(row.Action))
	details := map[string]interface{}{}
	require.NoError(t, json.Unmarshal(row.Details, &details))
	assert.Equal(t, float64(7), details["key_id"])
	assert.Equal(t, "ci-key", details["key_name"])
	assert.Equal(t, "agt-1", details["agent_id"])
	assert.Equal(t, false, details["stream"])
	assert.Equal(t, "completed", details["end_reason"])
	assert.InDelta(t, 150, details["total_tokens"], 0.001)
	_, hasQuery := details["query"]
	assert.False(t, hasQuery, "query must stay out unless audit_payload=true")
}

func TestGatewayAuditPayloadOptInAndFailureTolerated(t *testing.T) {
	audit := &fakeGatewayAudit{err: errors.New("audit sink down")}
	old := testGatewayCfg
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{AuditPayload: true}}
	defer func() { testGatewayCfg = old }()

	rec := serveCompletionsWithAudit(t, streamFakes(answerScript), audit, simpleCompletionsBody())
	require.Equal(t, http.StatusOK, rec.Code, "audit failure must not fail the turn")

	audit.mu.Lock()
	require.Len(t, audit.rows, 1)
	details := map[string]interface{}{}
	require.NoError(t, json.Unmarshal(audit.rows[0].Details, &details))
	audit.mu.Unlock()
	assert.Contains(t, details["query"], "武汉今天天气如何", "audit_payload=true retains the query")
}

// serveCompletionsWithAudit is serveCompletions with an explicit audit sink
// and a key scope attached.
func serveCompletionsWithAudit(t *testing.T, fakes completionsFakes, audit *fakeGatewayAudit, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(fakes.agents, fakes.sessions, fakes.messages, audit, testGatewayCfg)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, types.TenantAPIKeyScope{
			KeyID: 9, Name: "k",
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPerKeyConcurrencyLimit(t *testing.T) {
	// One shared handler/router: the per-key counter lives on the handler,
	// so both requests must hit the same instance.
	release := make(chan struct{})
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: func(bus *event.EventBus, _ *types.QARequest) error {
			<-release
			return answerScript(bus, nil)
		}},
		messages: &fakeGatewayMessages{},
	}
	router := newCompletionsRouter(t, fakes, &config.Config{Gateway: &config.GatewayConfig{MaxConcurrencyPerKey: 1}})

	// First request occupies the only slot.
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- postCompletions(router, simpleCompletionsBody())
	}()
	// Wait until the first request is inside AgentQA.
	deadline := time.After(2 * time.Second)
	for {
		fakes.sessions.mu.Lock()
		n := len(fakes.sessions.qaRequests)
		fakes.sessions.mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first request never reached AgentQA")
		case <-time.After(5 * time.Millisecond):
		}
	}

	second := postCompletions(router, simpleCompletionsBody())
	require.Equal(t, http.StatusTooManyRequests, second.Code)
	var env ErrorEnvelope
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &env))
	assert.Equal(t, "concurrency_limit", env.Error.Code)

	// After the first turn releases its slot, the same key gets through.
	close(release)
	require.Equal(t, http.StatusOK, (<-first).Code)
}

// newCompletionsRouter mounts one ChatCompletions handler with explicit
// gateway config and tenant context (tests run sequentially; swapping the
// global testGatewayCfg would race the goroutine in TestPerKeyConcurrencyLimit).
func newCompletionsRouter(t *testing.T, fakes completionsFakes, cfg *config.Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(fakes.agents, fakes.sessions, fakes.messages, nil, cfg)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	return router
}

// postCompletions sends one body to the router.
func postCompletions(router *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPerKeyConcurrencyIndependentKeysAndUnlimited(t *testing.T) {
	// limit=1, but the second call runs under a different key id.
	old := testGatewayCfg
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{MaxConcurrencyPerKey: 1}}
	defer func() { testGatewayCfg = old }()

	slots := make(chan struct{}, 2)
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{publishedAgent("agt-1", "Doc QA", true)}},
		sessions: &fakeGatewaySessions{script: func(bus *event.EventBus, _ *types.QARequest) error {
			slots <- struct{}{}
			<-slots
			return answerScript(bus, nil)
		}},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletionsWithKey(t, fakes, simpleCompletionsBody(), 1)
	require.Equal(t, http.StatusOK, rec.Code)
	// Unlimited when the knob is unset (0).
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{}}
	rec = serveCompletionsWithKey(t, fakes, simpleCompletionsBody(), 2)
	require.Equal(t, http.StatusOK, rec.Code)
}

// serveCompletionsWithKey attaches a specific key id to the request.
func serveCompletionsWithKey(t *testing.T, fakes completionsFakes, body string, keyID uint64) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := newTestHandler(fakes.agents, fakes.sessions, fakes.messages)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, types.TenantAPIKeyScope{
			KeyID: keyID,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestSSEHeartbeatBeforeFirstContent(t *testing.T) {
	// A silent run: heartbeat comments (not chunks) must appear while
	// waiting, and stop once content flows.
	fakes := streamFakes(func(bus *event.EventBus, _ *types.QARequest) error {
		// Stay silent past two heartbeat ticks, then answer and finish.
		time.Sleep(60 * time.Millisecond)
		if err := bus.Emit(context.Background(), event.Event{
			Type: event.EventAgentFinalAnswer,
			Data: event.AgentFinalAnswerData{Content: "答案", Done: true},
		}); err != nil {
			return err
		}
		return bus.Emit(context.Background(), event.Event{
			Type: event.EventAgentComplete,
			Data: event.AgentCompleteData{FinalAnswer: "答案"},
		})
	})
	old := testGatewayCfg
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{HeartbeatInterval: 20 * time.Millisecond}}
	defer func() { testGatewayCfg = old }()

	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, ": keep-alive")
	// Heartbeat comments come BEFORE the first content delta.
	assert.Less(t, strings.Index(body, ": keep-alive"), strings.Index(body, `"content":"答案"`),
		"keep-alive must precede the first content frame")
	assert.Contains(t, body, "data: [DONE]")
}

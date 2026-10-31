package openai_compat

// M2-3 session continuation tests: derived (user, agent) thread reuse,
// explicit X-WeKnora-Session-ID handling, 404 on bad explicit IDs, 409
// session_busy, and last-user-message-only rendering on continued turns.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// threadSession builds a gateway-kind session owned by owner and bound to agent.
func threadSession(id, owner, agentID string, updated time.Time) *types.Session {
	return &types.Session{
		ID:        id,
		TenantID:  42,
		Kind:      types.SessionKindGateway,
		UserID:    owner,
		UpdatedAt: updated,
		LastRequestState: &types.SessionLastRequestState{
			AgentID: agentID, AgentEnabled: true,
		},
	}
}

func (f *fakeGatewaySessions) FindGatewayThreadSession(
	_ context.Context, tenantID uint64, ownerUserID, agentID string, activeAfter time.Time,
) (*types.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.thread
	if t == nil || t.TenantID != tenantID || t.UserID != ownerUserID ||
		t.LastRequestState == nil || t.LastRequestState.AgentID != agentID {
		return nil, nil
	}
	if t.UpdatedAt.Before(activeAfter) {
		return nil, nil
	}
	out := *t
	return &out, nil
}

func (f *fakeGatewaySessions) TouchGatewaySession(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, sessionID)
	return nil
}

func (f *fakeGatewaySessions) GetSessionByID(_ context.Context, tenantID uint64, id string) (*types.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.thread != nil && f.thread.ID == id && f.thread.TenantID == tenantID {
		out := *f.thread
		return &out, nil
	}
	return nil, nil
}

// serveContinuation posts a completion with an optional explicit session header.
func serveContinuation(
	t *testing.T, sessions *fakeGatewaySessions, userID, sessionHeader, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(
		&fakeAgentService{agents: []*types.CustomAgent{
			gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
		}},
		sessions, &fakeGatewayMessages{}, nil, nil, nil, nil, nil, nil)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := c.Request.Context()
		if userID != "" {
			ctx = context.WithValue(ctx, types.UserIDContextKey, userID)
		}
		ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(42))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sessionHeader != "" {
		req.Header.Set(SessionIDHeader, sessionHeader)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func twoTurnBody() string {
	return `{"model":"Doc QA","messages":[` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"first answer"},` +
		`{"role":"user","content":"second question"}]}`
}

func TestGatewayContinuationDerivedThread(t *testing.T) {
	sessions := &fakeGatewaySessions{script: answerScript}

	// Turn 1: no thread yet → fresh session, full history render.
	rec := serveContinuation(t, sessions, "user-1", "", twoTurnBody())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sessions.mu.Lock()
	qa1 := sessions.qaRequests[len(sessions.qaRequests)-1]
	created := sessions.created
	sessions.mu.Unlock()
	require.NotNil(t, created.LastRequestState, "fresh gateway sessions pin their agent (M2-3)")
	require.Equal(t, "agt-1", created.LastRequestState.AgentID)
	require.Contains(t, qa1.Query, "first question", "stateless turns fold the whole history")
	require.Contains(t, qa1.Query, "second question")

	// Turn 2: thread exists → same session, only the newest user message.
	sessions.mu.Lock()
	sessions.thread = threadSession(created.ID, "user-1", "agt-1", time.Now())
	sessions.mu.Unlock()
	rec = serveContinuation(t, sessions, "user-1", "", twoTurnBody())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sessions.mu.Lock()
	qa2 := sessions.qaRequests[len(sessions.qaRequests)-1]
	touched := append([]string(nil), sessions.touched...)
	sessions.mu.Unlock()
	require.Equal(t, created.ID, qa2.Session.ID, "derived identity reuses the thread")
	require.True(t, strings.HasSuffix(strings.TrimSpace(qa2.Query), "second question"),
		"continued turns render only the newest user message: %q", qa2.Query)
	require.NotContains(t, qa2.Query, "first question")
	require.Contains(t, touched, created.ID, "continued threads slide their retention window")
}

func TestGatewayContinuationExplicitHeader(t *testing.T) {
	sessions := &fakeGatewaySessions{script: answerScript}
	sessions.mu.Lock()
	sessions.thread = threadSession("sess-live", "user-1", "agt-1", time.Now())
	sessions.mu.Unlock()

	// Explicit header + matching agent → reuse.
	rec := serveContinuation(t, sessions, "user-1", "sess-live", `{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sessions.mu.Lock()
	qa := sessions.qaRequests[len(sessions.qaRequests)-1]
	sessions.mu.Unlock()
	require.Equal(t, "sess-live", qa.Session.ID)

	// Explicit `new` → fresh session even though a thread exists.
	rec = serveContinuation(t, sessions, "user-1", "new", `{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sessions.mu.Lock()
	qa = sessions.qaRequests[len(sessions.qaRequests)-1]
	sessions.mu.Unlock()
	require.Equal(t, "sess-1", qa.Session.ID, "X-WeKnora-Session-ID: new forces a fresh session")

	// Unknown ID → 404 session_not_found.
	rec = serveContinuation(t, sessions, "user-1", "sess-nope", `{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "session_not_found")

	// Session bound to another agent → 404 (no cross-agent history bleed).
	sessions.mu.Lock()
	sessions.thread = threadSession("sess-live", "user-1", "agt-other", time.Now())
	sessions.mu.Unlock()
	rec = serveContinuation(t, sessions, "user-1", "sess-live", `{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestGatewayContinuationSessionBusy409(t *testing.T) {
	sessions := &fakeGatewaySessions{script: answerScript}
	handler := NewGatewayHandler(
		&fakeAgentService{agents: []*types.CustomAgent{
			gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
		}},
		sessions, &fakeGatewayMessages{}, nil, nil, nil, nil, nil, nil)
	sessions.mu.Lock()
	sessions.thread = threadSession("sess-live", "user-1", "agt-1", time.Now())
	sessions.mu.Unlock()

	// Serve through THIS handler instance so the pre-held lock is visible.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.UserIDContextKey, "user-1")
		ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(42))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)

	require.True(t, handler.turnLocks.tryAcquire("sess-live"), "test pre-holds the session turn")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SessionIDHeader, "sess-live")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "session_busy")

	// Releasing unblocks the next turn.
	handler.turnLocks.release("sess-live")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestGatewayContinuationAnonymousStaysStateless(t *testing.T) {
	sessions := &fakeGatewaySessions{script: answerScript}
	// No user principal, no key → no derivable thread; always fresh.
	rec := serveContinuation(t, sessions, "", "", twoTurnBody())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sessions.mu.Lock()
	qa := sessions.qaRequests[len(sessions.qaRequests)-1]
	sessions.mu.Unlock()
	require.Contains(t, qa.Query, "first question", "anonymous turns fold full history")
}

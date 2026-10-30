package openai_compat

// M2-3 session continuation: an external "conversation" maps to one WeKnora
// session so the agent's in-session memory and retrieval history apply, and
// each turn only sends its newest user message instead of re-folding the
// whole history.
//
// Identity precedence (design §3):
//  1. explicit `X-WeKnora-Session-ID` request header (or `new` to force a
//     fresh session);
//  2. the OpenAI `user` field + `model` pair: the gateway looks up the
//     tenant's most recently active gateway session for
//     (owner user, agent) and continues it.
//
// The first response echoes `X-WeKnora-Session-Id` (M1) so clients can pin
// the thread; concurrent sends against one session answer 409 session_busy.

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// SessionIDHeader is the explicit continuation header. The response echo
// uses the canonical capitalization `X-WeKnora-Session-Id` (M1 behaviour).
const SessionIDHeader = "X-WeKnora-Session-ID"

// SessionIDHeaderNew forces a fresh session regardless of any derivable
// thread (design §3 conflict handling).
const SessionIDHeaderNew = "new"

// gatewayThreadActiveWindow caps how stale a session may be to count as a
// live thread. It mirrors the reaper retention (service.DefaultGatewaySession-
// Retention = 72h, not importable from here without a cycle), so any lookup
// result is at most that old anyway; the window makes the intent explicit.
const gatewayThreadActiveWindow = 72 * time.Hour

// sessionTurnLocks serializes turns per session (design §3 conflict
// handling): concurrent sends on one continued session answer 409
// session_busy instead of interleaving history. In-process, matching the M1
// per-key semaphore decision — the deployment unit is one gateway process.
type sessionTurnLocks struct {
	mu   sync.Mutex
	busy map[string]struct{}
}

func newSessionTurnLocks() *sessionTurnLocks {
	return &sessionTurnLocks{busy: make(map[string]struct{})}
}

func (l *sessionTurnLocks) tryAcquire(sessionID string) bool {
	if sessionID == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, held := l.busy[sessionID]; held {
		return false
	}
	l.busy[sessionID] = struct{}{}
	return true
}

func (l *sessionTurnLocks) release(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.busy, sessionID)
}

// resolvedThread is the session a turn runs in plus whether it continues an
// existing thread (which decides rendering: only the newest user message).
type resolvedThread struct {
	session    *types.Session
	continued  bool
	createdNow bool
}

// resolveGatewayThread applies the M2-3 identity precedence. It never
// creates a session; callers fall back to createGatewaySession when
// thread.session == nil. Explicit-header misses answer 404 (debugging
// surface, no silent fork); derived-key misses fall back to a new session.
func (h *GatewayHandler) resolveGatewayThread(
	ctx context.Context, c *gin.Context, tenantID uint64, agent *types.CustomAgent,
) (*resolvedThread, bool) {
	owner := types.SessionOwnerIDFromContext(ctx)

	if explicit := strings.TrimSpace(c.GetHeader(SessionIDHeader)); explicit != "" {
		if strings.EqualFold(explicit, SessionIDHeaderNew) {
			return &resolvedThread{createdNow: false}, true
		}
		session, err := h.sessions.GetSessionByID(ctx, tenantID, explicit)
		if err != nil || session == nil || session.Kind != types.SessionKindGateway ||
			session.UserID != owner || !sessionOwnedByAgent(session, agent.ID) {
			logger.Warnf(ctx, "[gateway] explicit session continue rejected: requested=%s tenant=%d err=%v",
				explicit, tenantID, err)
			return nil, false // 404 session_not_found
		}
		return &resolvedThread{session: session, continued: true}, true
	}

	// Derived identity: (owner user | key, agent). Without an owner principal
	// the key ID scopes the thread so keys cannot bleed into each other.
	lookupUser := owner
	if lookupUser == "" {
		if keyID := gatewayKeyID(ctx); keyID > 0 {
			lookupUser = gatewayKeyPrincipal(keyID)
		}
	}
	if lookupUser == "" {
		return &resolvedThread{}, true // anonymous turn: always fresh
	}
	session, err := h.sessions.FindGatewayThreadSession(
		ctx, tenantID, lookupUser, agent.ID, time.Now().Add(-gatewayThreadActiveWindow))
	if err != nil {
		logger.Warnf(ctx, "[gateway] thread lookup failed (continuing stateless): tenant=%d err=%v",
			tenantID, err)
		return &resolvedThread{}, true // fail-open to stateless, never fail the request
	}
	if session == nil {
		return &resolvedThread{}, true
	}
	return &resolvedThread{session: session, continued: true}, true
}

// gatewayKeyPrincipal is the thread-owner stand-in for tenant-level keys
// without a user principal.
func gatewayKeyPrincipal(keyID uint64) string {
	return "key:" + strconv.FormatUint(keyID, 10)
}

// sessionOwnedByAgent reports whether the session was created for this agent
// (agent_config -> SessionLastRequestState.AgentID). M1 gateway sessions pin
// it at creation; empty means a pre-M2 row → not continuable per agent.
func sessionOwnedByAgent(session *types.Session, agentID string) bool {
	return session.LastRequestState != nil && session.LastRequestState.AgentID == agentID
}

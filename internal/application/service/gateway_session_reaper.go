// Package service - gateway session reaper.
//
// Every OpenAI-compatible gateway turn (POST /v1/chat/completions) creates a
// Kind="gateway" audit session that stays in the database after the answer
// leaves (docs design §7.1): the client may be an external agent that never
// comes back, so the row exists purely for operators to trace what the key
// asked. Retention defaults to 72h (gateway.session_retention); this reaper
// deletes expired rows together with their messages on an hourly ticker.
package service

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// gatewayReaperBatchSize caps one reap pass so a large backlog cannot monopolize
// the DB; the next tick continues where this one stopped (oldest first).
const gatewayReaperBatchSize = 200

// gatewayReaperSessionStore is the repository slice the reaper needs.
type gatewayReaperSessionStore interface {
	ListExpiredGatewaySessions(ctx context.Context, olderThan time.Time, limit int) ([]*types.Session, error)
	DeleteGatewaySessionsByID(ctx context.Context, ids []string) (int64, error)
}

// gatewayReaperMessageStore deletes the conversation rows of a session.
type gatewayReaperMessageStore interface {
	DeleteMessagesBySessionID(ctx context.Context, sessionID string) error
}

// GatewaySessionReaper retires expired gateway audit sessions.
type GatewaySessionReaper struct {
	sessions  gatewayReaperSessionStore
	messages  gatewayReaperMessageStore
	retention time.Duration
}

// NewGatewaySessionReaper wires the reaper with an explicit retention window
// (tests inject a short one). Non-positive retention falls back to the
// default so a config typo cannot wipe sessions immediately.
func NewGatewaySessionReaper(
	sessions gatewayReaperSessionStore, messages gatewayReaperMessageStore, retention time.Duration,
) *GatewaySessionReaper {
	if retention <= 0 {
		retention = DefaultGatewaySessionRetention
	}
	return &GatewaySessionReaper{sessions: sessions, messages: messages, retention: retention}
}

// NewGatewaySessionReaperFromRepos is the exported DI constructor; retention
// comes from the gateway config section when enabled there. dig matches the
// exported repository interfaces to the container's providers; the reaper
// itself only uses the two methods in gatewayReaperSessionStore.
func NewGatewaySessionReaperFromRepos(
	sessions interfaces.SessionRepository, messages interfaces.MessageRepository, cfg *config.Config,
) *GatewaySessionReaper {
	var retention time.Duration
	if cfg != nil && cfg.Gateway != nil {
		retention = cfg.Gateway.EffectiveSessionRetention()
	}
	return NewGatewaySessionReaper(sessions, messages, retention)
}

// DefaultGatewaySessionRetention mirrors the config default; the config
// package owns the knob, this constant only guards a nil config.
const DefaultGatewaySessionRetention = 72 * time.Hour

// ReapOnce runs one collection pass and reports how many sessions were
// deleted. Message rows go first so a crash between the two deletes leaves
// orphan messages pointing at a live session (re-runnable next tick) instead
// of a deleted session whose messages still leak into storage.
func (r *GatewaySessionReaper) ReapOnce(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-r.retention)
	expired, err := r.sessions.ListExpiredGatewaySessions(ctx, cutoff, gatewayReaperBatchSize)
	if err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}

	ids := make([]string, 0, len(expired))
	for _, s := range expired {
		if s == nil || s.ID == "" {
			continue
		}
		if err := r.messages.DeleteMessagesBySessionID(ctx, s.ID); err != nil {
			logger.Warnf(ctx, "[GatewaySessionReaper] delete messages failed: session=%s err=%v", s.ID, err)
			continue
		}
		ids = append(ids, s.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	deleted, err := r.sessions.DeleteGatewaySessionsByID(ctx, ids)
	if err != nil {
		return 0, err
	}
	if deleted > 0 {
		logger.Infof(ctx, "[GatewaySessionReaper] reaped %d gateway sessions (retention=%s)", deleted, r.retention)
	}
	return int(deleted), nil
}

// Compile-time checks: the container hands us the full repository
// interfaces; the reaper consumes the narrow slices above.
var (
	_ gatewayReaperSessionStore = interfaces.SessionRepository(nil)
	_ gatewayReaperMessageStore = interfaces.MessageRepository(nil)
)

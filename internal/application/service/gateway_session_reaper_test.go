package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// fakeReaperSessions is an in-memory gatewayReaperSessionStore.
type fakeReaperSessions struct {
	listed  []*types.Session
	deleted []string
	listErr error
	delErr  error
}

func (f *fakeReaperSessions) ListExpiredGatewaySessions(
	_ context.Context, olderThan time.Time, limit int,
) ([]*types.Session, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*types.Session, 0, len(f.listed))
	for _, s := range f.listed {
		if s.UpdatedAt.Before(olderThan) && len(out) < limit {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeReaperSessions) DeleteGatewaySessionsByID(_ context.Context, ids []string) (int64, error) {
	if f.delErr != nil {
		return 0, f.delErr
	}
	f.deleted = append(f.deleted, ids...)
	return int64(len(ids)), nil
}

// fakeReaperMessages tracks per-session message deletes and can fail one ID.
type fakeReaperMessages struct {
	deleted  []string
	failFor  string
	deletedS map[string]bool
}

func (f *fakeReaperMessages) DeleteMessagesBySessionID(_ context.Context, sessionID string) error {
	if sessionID == f.failFor {
		return errors.New("message delete failed")
	}
	if f.deletedS == nil {
		f.deletedS = map[string]bool{}
	}
	f.deletedS[sessionID] = true
	f.deleted = append(f.deleted, sessionID)
	return nil
}

func gatewaySession(id string, updated time.Time) *types.Session {
	return &types.Session{ID: id, TenantID: 42, Kind: types.SessionKindGateway, UpdatedAt: updated}
}

func TestGatewaySessionReaperDeletesExpiredWithMessages(t *testing.T) {
	now := time.Now()
	sessions := &fakeReaperSessions{listed: []*types.Session{
		gatewaySession("old-1", now.Add(-73*time.Hour)),
		gatewaySession("old-2", now.Add(-80*time.Hour)),
		// Fresh ones must survive.
		gatewaySession("fresh", now.Add(-1*time.Hour)),
	}}
	messages := &fakeReaperMessages{}
	reaper := NewGatewaySessionReaper(sessions, messages, 72*time.Hour)

	n, err := reaper.ReapOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.ElementsMatch(t, []string{"old-1", "old-2"}, sessions.deleted)
	require.True(t, messages.deletedS["old-1"] && messages.deletedS["old-2"])
}

func TestGatewaySessionReaperSkipsSessionWhoseMessagesFail(t *testing.T) {
	now := time.Now()
	sessions := &fakeReaperSessions{listed: []*types.Session{
		gatewaySession("broken", now.Add(-73*time.Hour)),
		gatewaySession("ok", now.Add(-73*time.Hour)),
	}}
	messages := &fakeReaperMessages{failFor: "broken"}
	reaper := NewGatewaySessionReaper(sessions, messages, 72*time.Hour)

	n, err := reaper.ReapOnce(context.Background())
	require.NoError(t, err, "one broken session must not fail the pass")
	require.Equal(t, 1, n)
	require.Equal(t, []string{"ok"}, sessions.deleted)
	require.False(t, messages.deletedS["broken"])
}

func TestGatewaySessionReaperDefaultRetentionGuard(t *testing.T) {
	// A zero/negative retention falls back to 72h, never "delete now".
	reaper := NewGatewaySessionReaper(&fakeReaperSessions{}, &fakeReaperMessages{}, 0)
	require.Equal(t, 72*time.Hour, reaper.retention)
	reaper = NewGatewaySessionReaper(&fakeReaperSessions{}, &fakeReaperMessages{}, -time.Hour)
	require.Equal(t, 72*time.Hour, reaper.retention)
}

func TestGatewaySessionReaperEmptyPassIsNoop(t *testing.T) {
	sessions := &fakeReaperSessions{}
	n, err := NewGatewaySessionReaper(sessions, &fakeReaperMessages{}, time.Hour).ReapOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Empty(t, sessions.deleted)
}

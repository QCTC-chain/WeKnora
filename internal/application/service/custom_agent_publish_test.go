package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// publishFakeRepo satisfies the slice of CustomAgentRepository SetAgentPublished
// touches (GetAgentByID + ListAgentsByTenantID + UpdateAgent).
type publishFakeRepo struct {
	interfaces.CustomAgentRepository
	agents map[string]*types.CustomAgent // keyed by ID, all tenant 1
	list   []*types.CustomAgent
	writeE error
	writes int
}

func (r *publishFakeRepo) GetAgentByID(_ context.Context, id string, _ uint64) (*types.CustomAgent, error) {
	agent, ok := r.agents[id]
	if !ok {
		return nil, repository.ErrCustomAgentNotFound
	}
	return agent, nil
}

func (r *publishFakeRepo) ListAgentsByTenantID(_ context.Context, _ uint64) ([]*types.CustomAgent, error) {
	return r.list, nil
}

func (r *publishFakeRepo) UpdateAgent(_ context.Context, a *types.CustomAgent) error {
	if r.writeE != nil {
		return r.writeE
	}
	r.agents[a.ID] = a
	r.writes++
	return nil
}

func publishService(repo *publishFakeRepo) interfaces.CustomAgentService {
	return NewCustomAgentService(repo, nil, nil, nil, nil, nil, nil, nil)
}

func publishCtx() context.Context {
	return context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))
}

func TestSetAgentPublishedFlipsState(t *testing.T) {
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{}, list: nil}
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1}
	repo.agents["a1"] = agent
	svc := publishService(repo)

	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !got.Published || got.PublishedAt == nil {
		t.Fatalf("agent not marked published: %+v", got)
	}
	if repo.writes != 1 {
		t.Fatalf("writes = %d, want 1", repo.writes)
	}

	got, err = svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: false})
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got.Published || got.PublishedAt != nil {
		t.Fatalf("agent not unpublished: %+v", got)
	}
}

func TestSetAgentPublishedIdempotent(t *testing.T) {
	// Governance-only PATCH with nothing to change: no write, same row.
	agent := &types.CustomAgent{
		ID: "a1", Name: "Doc QA", TenantID: 1, Published: true,
		GatewayScope: types.GatewayScopeTenant, GatewayVersion: 1,
	}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)

	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{})
	if err != nil {
		t.Fatalf("no-op governance: %v", err)
	}
	if repo.writes != 0 {
		t.Fatalf("no-op governance must not write, writes = %d", repo.writes)
	}
	if got != agent {
		t.Fatal("no-op governance must return the stored row")
	}
}

func TestSetAgentPublishedRepublishReleasesVersion(t *testing.T) {
	// PATCH {published:true} on an already-published agent = release a new
	// version: snapshot refresh + version bump, gateway face never gaps.
	agent := &types.CustomAgent{
		ID: "a1", Name: "Doc QA", TenantID: 1, Published: true,
		GatewayScope: types.GatewayScopeTenant, GatewayVersion: 4,
		Config: types.CustomAgentConfig{SystemPrompt: "v5"},
	}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)

	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if got.GatewayVersion != 5 {
		t.Fatalf("re-publish version = %d, want 5", got.GatewayVersion)
	}
	if got.GatewayEffectiveConfig().SystemPrompt != "v5" {
		t.Fatalf("re-publish did not refresh snapshot: %+v", got.GatewayEffectiveConfig())
	}
	if repo.writes != 1 {
		t.Fatalf("re-publish writes = %d, want 1", repo.writes)
	}
}

func TestSetAgentPublishedNameConflict(t *testing.T) {
	other := &types.CustomAgent{ID: "a2", Name: "doc qa", TenantID: 1, Published: true}
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA ", TenantID: 1}
	repo := &publishFakeRepo{
		agents: map[string]*types.CustomAgent{"a1": agent, "a2": other},
		list:   []*types.CustomAgent{agent, other},
	}
	svc := publishService(repo)

	_, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if !errors.Is(err, ErrAgentPublishedNameConflict) {
		t.Fatalf("err = %v, want ErrAgentPublishedNameConflict", err)
	}
	if agent.Published {
		t.Fatal("conflicted publish must not flip the flag")
	}
	if repo.writes != 0 {
		t.Fatalf("conflicted publish must not write, writes = %d", repo.writes)
	}
}

func TestSetAgentPublishedOwnNameNoConflict(t *testing.T) {
	// An already-published agent re-publishing (e.g. after a failed toggle)
	// must not trip over its own row — the repo list still says published
	// while the working copy says unpublished in that recovery flow.
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1}
	stale := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1, Published: true}
	repo := &publishFakeRepo{
		agents: map[string]*types.CustomAgent{"a1": agent},
		list:   []*types.CustomAgent{stale},
	}
	svc := publishService(repo)
	if _, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true}); err != nil {
		t.Fatalf("self-name publish: %v", err)
	}
}

func TestSetAgentPublishedBuiltinRejected(t *testing.T) {
	agent := &types.CustomAgent{ID: "builtin-quick-answer", Name: "快速问答", TenantID: 1, IsBuiltin: true}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"builtin-quick-answer": agent}}
	svc := publishService(repo)

	_, err := svc.SetAgentPublished(publishCtx(), "builtin-quick-answer", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if !errors.Is(err, ErrCannotModifyBuiltin) {
		t.Fatalf("err = %v, want ErrCannotModifyBuiltin", err)
	}
}

func TestSetAgentPublishedNotFound(t *testing.T) {
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{}}
	svc := publishService(repo)
	if _, err := svc.SetAgentPublished(publishCtx(), "ghost", types.GatewayPublishUpdate{PublishSet: true, Published: true}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("err = %v, want ErrAgentNotFound", err)
	}
}

func TestSetAgentPublishedEmptyName(t *testing.T) {
	agent := &types.CustomAgent{ID: "a1", Name: "   ", TenantID: 1}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)
	if _, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true}); !errors.Is(err, ErrAgentNameRequired) {
		t.Fatalf("err = %v, want ErrAgentNameRequired", err)
	}
}

// --- M2-a publish governance (snapshot / version / scope / sunset) ---

func TestSetAgentPublishedSnapshotFreezeAndVersion(t *testing.T) {
	agent := &types.CustomAgent{
		ID: "a1", Name: "Doc QA", TenantID: 1,
		Config: types.CustomAgentConfig{AgentMode: "quick-answer"},
	}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}, list: []*types.CustomAgent{agent}}
	svc := publishService(repo)

	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got.GatewayVersion != 1 {
		t.Fatalf("first publish version = %d, want 1", got.GatewayVersion)
	}
	if got.GatewaySnapshot == nil || got.GatewaySnapshot.AgentMode != "quick-answer" {
		t.Fatalf("snapshot missing: %+v", got.GatewaySnapshot)
	}

	// Post-publish edit: changes the workspace face, not the snapshot.
	agent.Config.AgentMode = "smart-reasoning"
	got, err = svc.SetAgentPublished(publishCtx(), "a1",
		types.GatewayPublishUpdate{Scope: strPtr("private")})
	if err != nil {
		t.Fatalf("governance update: %v", err)
	}
	if got.GatewayVersion != 1 {
		t.Fatalf("governance update bumped version to %d, want 1", got.GatewayVersion)
	}
	if got.GatewayEffectiveConfig().AgentMode != "quick-answer" {
		t.Fatalf("snapshot not frozen: %+v", got.GatewayEffectiveConfig())
	}
	if got.GatewayScope != "private" {
		t.Fatalf("scope = %q, want private", got.GatewayScope)
	}

	// Re-publish: refreshes the snapshot and bumps the version.
	got, err = svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if got.GatewayVersion != 2 {
		t.Fatalf("re-publish version = %d, want 2", got.GatewayVersion)
	}
	if got.GatewayEffectiveConfig().AgentMode != "smart-reasoning" {
		t.Fatalf("re-publish did not refresh snapshot: %+v", got.GatewayEffectiveConfig())
	}
}

func strPtr(s string) *string { return &s }

func TestSetAgentPublishedUnpublishRetainsVersion(t *testing.T) {
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}, list: []*types.CustomAgent{agent}}
	svc := publishService(repo)
	if _, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: false})
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got.GatewayVersion != 1 || got.GatewaySnapshot == nil {
		t.Fatalf("unpublish must retain version+snapshot: version=%d snapshot=%+v",
			got.GatewayVersion, got.GatewaySnapshot)
	}
	// Next publish continues the version history instead of restarting.
	got, err = svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if got.GatewayVersion != 2 {
		t.Fatalf("version after unpublish/republish = %d, want 2", got.GatewayVersion)
	}
}

func TestSetAgentPublishedUnpublishClearsSunset(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	agent := &types.CustomAgent{
		ID: "a1", Name: "Doc QA", TenantID: 1, Published: true,
		GatewayVersion: 3, GatewaySunset: &future,
	}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)
	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: false})
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got.GatewaySunset != nil {
		t.Fatalf("unpublish must clear sunset, got %v", got.GatewaySunset)
	}
	if got.GatewayVersion != 3 {
		t.Fatalf("unpublish must retain version, got %d", got.GatewayVersion)
	}
}

func TestSetAgentPublishedInvalidScope(t *testing.T) {
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)
	_, err := svc.SetAgentPublished(publishCtx(), "a1",
		types.GatewayPublishUpdate{Published: true, Scope: strPtr("world")})
	if !errors.Is(err, ErrInvalidGatewayScope) {
		t.Fatalf("err = %v, want ErrInvalidGatewayScope", err)
	}
	if agent.Published {
		t.Fatal("invalid scope must not flip the flag")
	}
}

func TestSetAgentPublishedInvalidSunset(t *testing.T) {
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)
	past := time.Now().Add(-time.Hour)
	_, err := svc.SetAgentPublished(publishCtx(), "a1",
		types.GatewayPublishUpdate{Published: true, Sunset: &past})
	if !errors.Is(err, ErrInvalidGatewaySunset) {
		t.Fatalf("err = %v, want ErrInvalidGatewaySunset", err)
	}
	if agent.Published {
		t.Fatal("invalid sunset must not flip the flag")
	}
}

func TestSetAgentPublishedSunsetClear(t *testing.T) {
	future := time.Now().Add(24 * time.Hour)
	agent := &types.CustomAgent{
		ID: "a1", Name: "Doc QA", TenantID: 1, Published: true,
		GatewayVersion: 2, GatewaySunset: &future,
	}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)
	got, err := svc.SetAgentPublished(publishCtx(), "a1",
		types.GatewayPublishUpdate{SunsetClear: true})
	if err != nil {
		t.Fatalf("sunset clear: %v", err)
	}
	if got.GatewaySunset != nil {
		t.Fatalf("sunset not cleared: %v", got.GatewaySunset)
	}
	if got.GatewayVersion != 2 {
		t.Fatalf("sunset clear bumped version to %d", got.GatewayVersion)
	}
}

func TestSetAgentPublishedSnapshotIsDeepCopy(t *testing.T) {
	agent := &types.CustomAgent{
		ID: "a1", Name: "Doc QA", TenantID: 1,
		Config: types.CustomAgentConfig{SystemPrompt: "v1"},
	}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}, list: []*types.CustomAgent{agent}}
	svc := publishService(repo)
	got, err := svc.SetAgentPublished(publishCtx(), "a1", types.GatewayPublishUpdate{PublishSet: true, Published: true})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Mutating the live row after publish must not reach into the snapshot.
	agent.Config.SystemPrompt = "v2"
	if got.GatewayEffectiveConfig().SystemPrompt != "v1" {
		t.Fatalf("snapshot shares state with live config: %+v", got.GatewayEffectiveConfig())
	}
}

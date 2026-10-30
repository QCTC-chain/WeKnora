package service

import (
	"context"
	"errors"
	"testing"

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

	got, err := svc.SetAgentPublished(publishCtx(), "a1", true)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !got.Published || got.PublishedAt == nil {
		t.Fatalf("agent not marked published: %+v", got)
	}
	if repo.writes != 1 {
		t.Fatalf("writes = %d, want 1", repo.writes)
	}

	got, err = svc.SetAgentPublished(publishCtx(), "a1", false)
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got.Published || got.PublishedAt != nil {
		t.Fatalf("agent not unpublished: %+v", got)
	}
}

func TestSetAgentPublishedIdempotent(t *testing.T) {
	agent := &types.CustomAgent{ID: "a1", Name: "Doc QA", TenantID: 1, Published: true}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)

	got, err := svc.SetAgentPublished(publishCtx(), "a1", true)
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if repo.writes != 0 {
		t.Fatalf("idempotent publish must not write, writes = %d", repo.writes)
	}
	if got != agent {
		t.Fatal("idempotent publish must return the stored row")
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

	_, err := svc.SetAgentPublished(publishCtx(), "a1", true)
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
	if _, err := svc.SetAgentPublished(publishCtx(), "a1", true); err != nil {
		t.Fatalf("self-name publish: %v", err)
	}
}

func TestSetAgentPublishedBuiltinRejected(t *testing.T) {
	agent := &types.CustomAgent{ID: "builtin-quick-answer", Name: "快速问答", TenantID: 1, IsBuiltin: true}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"builtin-quick-answer": agent}}
	svc := publishService(repo)

	_, err := svc.SetAgentPublished(publishCtx(), "builtin-quick-answer", true)
	if !errors.Is(err, ErrCannotModifyBuiltin) {
		t.Fatalf("err = %v, want ErrCannotModifyBuiltin", err)
	}
}

func TestSetAgentPublishedNotFound(t *testing.T) {
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{}}
	svc := publishService(repo)
	if _, err := svc.SetAgentPublished(publishCtx(), "ghost", true); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("err = %v, want ErrAgentNotFound", err)
	}
}

func TestSetAgentPublishedEmptyName(t *testing.T) {
	agent := &types.CustomAgent{ID: "a1", Name: "   ", TenantID: 1}
	repo := &publishFakeRepo{agents: map[string]*types.CustomAgent{"a1": agent}}
	svc := publishService(repo)
	if _, err := svc.SetAgentPublished(publishCtx(), "a1", true); !errors.Is(err, ErrAgentNameRequired) {
		t.Fatalf("err = %v, want ErrAgentNameRequired", err)
	}
}

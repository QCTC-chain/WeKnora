package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// An agent's allowed_tools is read in three places: the session config, tool
// registration and the rerank requirement. These tests pin all three to the
// same meaning: an unset list selects the default tools and an empty list
// means no built-in tools.

func TestBuildAgentConfigSeparatesUnsetFromEmptyToolList(t *testing.T) {
	svc := &sessionService{cfg: &config.Config{}, webSearchProviderRepo: &sharedAgentWebSearchRepo{}}
	build := func(allowed []string) []string {
		req := &types.QARequest{
			Session: &types.Session{ID: "session", TenantID: 1},
			CustomAgent: &types.CustomAgent{TenantID: 1, Config: types.CustomAgentConfig{
				AgentMode: types.AgentModeSmartReasoning, AllowedTools: allowed,
			}},
		}
		cfg, err := svc.buildAgentConfig(t.Context(), req, &types.Tenant{ID: 1}, 1)
		require.NoError(t, err)
		return cfg.AllowedTools
	}

	require.Equal(t, tools.DefaultAllowedTools(), build(nil))
	empty := build([]string{})
	require.NotNil(t, empty, "the empty list must reach tool registration as empty, not unset")
	require.Empty(t, empty)
	require.Equal(t, []string{tools.ToolSearchKnowledge}, build([]string{tools.LegacyToolKnowledgeSearch}))
}

func TestRegisterToolsSeparatesUnsetFromEmptyToolList(t *testing.T) {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(7))
	offered := func(allowed []string) []string {
		chatModel := &fakeAgentChatModel{}
		engine, err := (&agentService{}).CreateAgentEngine(
			ctx, &types.AgentConfig{AllowedTools: allowed}, chatModel, nil, nil, "sess-1", "msg-1")
		require.NoError(t, err)
		_, err = engine.Execute(ctx, "sess-1", "msg-1", "hello", nil)
		require.NoError(t, err)
		return chatModel.lastToolNames
	}

	// Without a knowledge base the retrieval defaults are dropped, which
	// leaves search_conversations as the visible part of the defaults.
	require.Contains(t, offered(nil), tools.ToolSearchConversations)
	require.NotContains(t, offered([]string{}), tools.ToolSearchConversations)
	require.Empty(t, offered([]string{}), "an empty list registers no built-in tool")
}

func TestRerankRequirementSeparatesUnsetFromEmptyToolList(t *testing.T) {
	agent := func(allowed []string) *types.CustomAgent {
		return &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "all", AllowedTools: allowed}}
	}
	require.True(t, agentRequiresRerankModel(agent(nil)), "the default tools include search_knowledge")
	require.False(t, agentRequiresRerankModel(agent([]string{})), "no tool calls the reranker")
}

// knowledgeToolFallback rescues exactly the broken stored state: an explicit
// empty allowlist on an agent that still binds knowledge bases and can pay
// for search_knowledge with its own rerank model. Every other shape is left
// alone.
func TestKnowledgeToolFallbackTargetsEmptyAllowlistWithBoundKBs(t *testing.T) {
	make := func(allowed []string, mode string, kbs []string, rerank string) *types.CustomAgent {
		return &types.CustomAgent{ID: "agt-1", Config: types.CustomAgentConfig{
			KBSelectionMode: mode, KnowledgeBases: kbs, AllowedTools: allowed, RerankModelID: rerank,
		}}
	}
	boundKBs := []string{"kb-1", "kb-2"}

	fallback, ok := knowledgeToolFallback(make([]string{}, "selected", boundKBs, "rerank-1"))
	require.True(t, ok, "explicit empty allowlist with bound KBs is the broken state")
	require.Equal(t,
		[]string{tools.ToolSearchKnowledge, tools.ToolReadDocument, tools.ToolListDocuments}, fallback)

	// Applying the fallback keeps the rerank contract consistent: the agent
	// now requires the rerank model it already declares.
	rescued := make([]string{}, "selected", boundKBs, "rerank-1")
	rescued.Config.AllowedTools, _ = knowledgeToolFallback(rescued)
	require.True(t, agentRequiresRerankModel(rescued))

	for name, agent := range map[string]*types.CustomAgent{
		"nil agent": nil,
		"unset allowlist already selects the defaults":      make(nil, "selected", boundKBs, "rerank-1"),
		"non-empty allowlist is the caller's choice":        make([]string{"search_knowledge"}, "selected", boundKBs, "rerank-1"),
		"KB scope disabled":                                 make([]string{}, "none", boundKBs, "rerank-1"),
		"no knowledge bases bound":                          make([]string{}, "selected", nil, "rerank-1"),
		"no rerank model must not turn into a turn failure": make([]string{}, "selected", boundKBs, ""),
	} {
		_, ok := knowledgeToolFallback(agent)
		require.False(t, ok, "case %s must keep today's behavior", name)
	}

	// Unset mode defaults to selected behavior; all-mode binds tenant-wide.
	for _, mode := range []string{"", "all"} {
		_, ok := knowledgeToolFallback(make([]string{}, mode, boundKBs, "rerank-1"))
		require.True(t, ok, "mode %q with bound KBs is still the broken state", mode)
	}
}

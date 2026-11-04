package openai_compat

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// titleTurnBody is the exact shape harness clients send alongside every real
// question: one system instruction plus one user payload carrying the
// messages to name.
func titleTurnBody() string {
	return `{"model":"agt-1","stream":false,"store":false,"messages":[` +
		`{"role":"system","content":"Create a concise title for an AI coding-assistant session from the supplied human messages.\nReturn only the title as plain text with no quotes or punctuation."},` +
		`{"role":"user","content":"Generate the session title from this JSON array of human messages:\n[{\"role\":\"human\",\"content\":\"清大科越公司的产品或业务覆盖了哪些省份？\"}]"}` +
		`]}`
}

func kbAgent(id, name string) *types.CustomAgent {
	agent := publishedAgent(id, name, true)
	agent.Config.KBSelectionMode = "selected"
	agent.Config.KnowledgeBases = []string{"kb-1", "kb-2"}
	agent.Config.AllowedTools = []string{"search_knowledge", "read_document"}
	agent.Config.WebSearchEnabled = true
	return agent
}

func TestIsSessionTitleTurn(t *testing.T) {
	probe := []ChatMessage{
		{Role: "system", Content: ContentUnion{Text: "Create a concise title for an AI coding-assistant session from the supplied human messages.\nReturn only the title."}},
		{Role: "user", Content: ContentUnion{Text: "Generate the session title from this JSON array of human messages:\n[]"}},
	}
	require.True(t, isSessionTitleTurn(probe), "the harness probe pair matches")

	negatives := map[string][]ChatMessage{
		"the real question alone": {
			{Role: "user", Content: ContentUnion{Text: "清大科越公司的产品或业务覆盖了哪些省份？"}},
		},
		"extra history message": {
			{Role: "system", Content: ContentUnion{Text: "Create a concise title for an AI coding-assistant session."}},
			{Role: "assistant", Content: ContentUnion{Text: "之前问过省份"}},
			{Role: "user", Content: ContentUnion{Text: "Generate the session title from this JSON array of human messages:\n[]"}},
		},
		"roles swapped": {
			{Role: "user", Content: ContentUnion{Text: "Generate the session title from this JSON array of human messages:\n[]"}},
			{Role: "system", Content: ContentUnion{Text: "Create a concise title for an AI coding-assistant session."}},
		},
		"unrelated system prefix": {
			{Role: "system", Content: ContentUnion{Text: "You are a helpful software engineer assistant."}},
			{Role: "user", Content: ContentUnion{Text: "Generate the session title from this JSON array of human messages:\n[]"}},
		},
		"unrelated user prefix": {
			{Role: "system", Content: ContentUnion{Text: "Create a concise title for an AI coding-assistant session from the supplied human messages."}},
			{Role: "user", Content: ContentUnion{Text: "清大科越公司的产品或业务覆盖了哪些省份？"}},
		},
		"tool result trailing": {
			{Role: "system", Content: ContentUnion{Text: "Create a concise title for an AI coding-assistant session."}},
			{Role: "user", Content: ContentUnion{Text: "Generate the session title from this JSON array of human messages:\n[]"}},
			{Role: "tool", Content: ContentUnion{Text: "done"}},
		},
	}
	for name, msgs := range negatives {
		assert.False(t, isSessionTitleTurn(msgs), "case %s must not match a title probe", name)
	}
}

func TestUtilityAgentViewStripsKnowledgeMachinery(t *testing.T) {
	agent := kbAgent("agt-1", "Doc QA")
	view := utilityAgentView(agent)

	assert.Equal(t, "agt-1", view.ID)
	assert.Equal(t, "none", view.Config.KBSelectionMode)
	assert.Empty(t, view.Config.KnowledgeBases)
	assert.False(t, view.Config.WebSearchEnabled)
	require.NotNil(t, view.Config.CitationEnabled)
	assert.False(t, *view.Config.CitationEnabled)
	assert.Equal(t, "Doc QA", view.Name, "identity and presentation stay")

	// The receiver keeps the published configuration intact.
	assert.Equal(t, "selected", agent.Config.KBSelectionMode)
	assert.Equal(t, []string{"kb-1", "kb-2"}, agent.Config.KnowledgeBases)
	assert.True(t, agent.Config.WebSearchEnabled)
}

func TestChatCompletionsTitleTurnRunsUtilityAgent(t *testing.T) {
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{kbAgent("agt-1", "Doc QA")}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, titleTurnBody(), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	fakes.sessions.mu.Lock()
	reqs := fakes.sessions.qaRequests
	fakes.sessions.mu.Unlock()
	require.Len(t, reqs, 1)
	titleAgent := reqs[0].CustomAgent
	assert.Equal(t, "agt-1", titleAgent.ID, "the turn still audited under the real agent")
	assert.Equal(t, "none", titleAgent.Config.KBSelectionMode, "title turn must not load knowledge bases")
	assert.Empty(t, titleAgent.Config.KnowledgeBases)
	require.NotNil(t, titleAgent.Config.CitationEnabled)
	assert.False(t, *titleAgent.Config.CitationEnabled, "title turn must not render citations")

	// The published agent itself is untouched for later turns.
	require.NotEmpty(t, fakes.agents.agents)
	assert.Equal(t, "selected", fakes.agents.agents[0].Config.KBSelectionMode)
	assert.Equal(t, []string{"kb-1", "kb-2"}, fakes.agents.agents[0].Config.KnowledgeBases)

	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "agt-1", resp.Model)
	assert.Equal(t, "部分答案", resp.Choices[0].Message.Content,
		"no citation block: a title turn collects no references")
}

func TestChatCompletionsRealQuestionKeepsPublishedAgent(t *testing.T) {
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{kbAgent("agt-1", "Doc QA")}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	fakes.sessions.mu.Lock()
	reqs := fakes.sessions.qaRequests
	fakes.sessions.mu.Unlock()
	require.Len(t, reqs, 1)
	assert.Same(t, fakes.agents.agents[0], reqs[0].CustomAgent,
		"an ordinary question runs the published agent view, not a utility copy")
	assert.Equal(t, "selected", reqs[0].CustomAgent.Config.KBSelectionMode)
}

// qaRecorder captures the agent view AgentQA receives without running one.
func TestChatCompletionsTitleTurnStreamMatchesDetection(t *testing.T) {
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{kbAgent("agt-1", "Doc QA")}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	body := `{"model":"agt-1","stream":true,"store":false,"messages":[` +
		`{"role":"system","content":"Create a concise title for an AI coding-assistant session from the supplied human messages.\nReturn only the title as plain text."},` +
		`{"role":"user","content":"Generate the session title from this JSON array of human messages:\n[]"}` +
		`]}`
	rec := serveCompletions(t, fakes, body, "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "部分")
	assert.Contains(t, rec.Body.String(), "答案")
	assert.NotContains(t, rec.Body.String(), "参考来源",
		"the utility view disables citations for the title turn")

	fakes.sessions.mu.Lock()
	reqs := fakes.sessions.qaRequests
	fakes.sessions.mu.Unlock()
	require.Len(t, reqs, 1)
	assert.Equal(t, "none", reqs[0].CustomAgent.Config.KBSelectionMode,
		"the streaming path takes the same utility shortcut")
}

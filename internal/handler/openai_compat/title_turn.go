package openai_compat

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Harness clients that reuse the agent gateway as a plain chat model fire a
// session-title generation request alongside every real question. The probe
// is a utility call — it must produce a short title and nothing else — yet it
// otherwise runs the full published agent: knowledge loading, tool
// registration, rerank setup, and a multi-KB runtime context inflated by the
// folded caller instructions. That burns tokens and latency on machinery a
// title can never use.
//
// The probes are recognizable by their fixed instruction pair. A turn that
// matches runs with the knowledge machinery stripped, so the QA pipeline
// (Agent engine or KnowledgeQA, by the frozen agent_mode) degrades to a
// single plain chat round on the agent's own model. Anything else —
// including any request that merely resembles one — keeps the published
// agent untouched.
const (
	titleSystemPromptPrefix = "Create a concise title for"
	titleUserPromptPrefix   = "Generate the session title from"
)

// isSessionTitleTurn reports whether the messages array is a harness session
// title probe: exactly one system instruction asking for a title plus one
// user payload carrying the messages to name, with no other roles involved.
// The exact prefixes keep real questions from ever matching by accident.
func isSessionTitleTurn(messages []ChatMessage) bool {
	if len(messages) != 2 {
		return false
	}
	system, user := &messages[0], &messages[1]
	if system.Role != "system" || user.Role != "user" {
		return false
	}
	if !strings.HasPrefix(system.Content.Text, titleSystemPromptPrefix) {
		return false
	}
	return strings.HasPrefix(user.Content.Text, titleUserPromptPrefix)
}

// utilityAgentView returns the agent view a utility turn runs with: the
// agent's identity, model binding and sampling stay, while the knowledge
// machinery is stripped so the turn cannot trigger retrieval, citations or
// rerank setup. The receiver is never mutated — resolveAgent's return value
// stays intact for headers, audit and later turns.
func utilityAgentView(agent *types.CustomAgent) *types.CustomAgent {
	view := *agent
	view.Config = agent.Config
	view.Config.KBSelectionMode = "none"
	view.Config.KnowledgeBases = nil
	view.Config.WebSearchEnabled = false
	citations := false
	view.Config.CitationEnabled = &citations
	return &view
}

package openai_compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCitationTagStripper(t *testing.T) {
	cases := []struct {
		name  string
		feeds []string
		want  string
	}{
		{"single chunk", []string{`答案<kb doc="手册.md" chunk_id="c1" kb_id="kb1" />。`}, `答案。`},
		{"tag split across chunks", []string{`根据<kb doc="手`, `册.md" chunk_id="c1" />手册`}, `根据手册`},
		{"web tag", []string{`x<web url="https://a" title="t" />y`}, `xy`},
		{"case-insensitive", []string{`a<KB DOC="d" CHUNK_ID="c" />b`}, `ab`},
		{"self-closing without space", []string{`a<kb/>b`}, `ab`},
		{"consecutive tags", []string{`<kb doc="1" /><kb doc="2" />尾`}, `尾`},
		{"ordinary less-than kept", []string{`a < b and 1<2`}, `a < b and 1<2`},
		{"lone angle bracket released later", []string{`a<`, `b`}, `a<b`},
		{"look-alike html kept", []string{`快捷键<kbd>⌘</kbd>。`}, `快捷键<kbd>⌘</kbd>。`},
		{"look-alike prefix kept", []string{`<kbf> 保留`}, `<kbf> 保留`},
		{"exact opening held then closed", []string{`<kb`, ` doc="d" />x`}, `x`},
		{"unclosed tag dropped at flush", []string{`答案<kb doc="d"`}, `答案`},
		{"empty", []string{``, ``}, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &citationTagStripper{}
			var got strings.Builder
			for _, chunk := range tc.feeds {
				got.WriteString(s.feed(chunk))
			}
			got.WriteString(s.flush())
			assert.Equal(t, tc.want, got.String())
		})
	}
}

func TestStripCitationTagsComplete(t *testing.T) {
	got := stripCitationTagsComplete(
		`前<kb doc="d" chunk_id="c" kb_id="k" />中<web url="u" title="t" />后`)
	assert.Equal(t, "前中后", got)
	assert.Equal(t, "", stripCitationTagsComplete(""))
}

// taggedScript emits an answer carrying inline citation tags split across
// two events, plus one web citation, with references for the source list.
func taggedScript(bus *event.EventBus, _ *types.QARequest) error {
	emit := func(typ event.EventType, data interface{}) error {
		return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
	}
	if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{
		Content: `根据手册<kb doc="手册.md" chunk_id="c1" kb_id="kb1" />`,
	}); err != nil {
		return err
	}
	if err := emit(event.EventAgentReferences, event.AgentReferencesData{
		References: []*types.SearchResult{{ID: "r1", KnowledgeID: "k1", KnowledgeTitle: "手册", ChunkIndex: 2}},
	}); err != nil {
		return err
	}
	if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{
		Content: `保修 2 年。<web url="https://example.com" title="官网" />`,
		Done:    true,
	}); err != nil {
		return err
	}
	return emit(event.EventAgentComplete, event.AgentCompleteData{
		FinalAnswer: `根据手册保修 2 年。`,
		Usage:       &types.TokenUsage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
	})
}

func TestChatCompletionsStreamStripsCitationTags(t *testing.T) {
	fakes := streamFakes(taggedScript)
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	frames := parseSSE(t, rec.Body.String())
	var content strings.Builder
	for i := 1; i < len(frames)-1; i++ {
		chunk := decodeChunk(t, frames[i])
		if chunk.Choices[0].Delta != nil {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	assert.Equal(t, "根据手册保修 2 年。\n---\n参考来源:\n1. 手册", content.String())
	assert.NotContains(t, content.String(), "<kb")
	assert.NotContains(t, content.String(), "<web")

	// The assistant row persists exactly what the client saw.
	fakes.messages.mu.Lock()
	var assistant *types.Message
	for _, m := range fakes.messages.updated {
		if m.Role == "assistant" {
			assistant = m
		}
	}
	fakes.messages.mu.Unlock()
	require.NotNil(t, assistant, "assistant row must be finalized")
	assert.Equal(t, "根据手册保修 2 年。\n---\n参考来源:\n1. 手册", assistant.Content)
}

func TestChatCompletionsStreamKeepsCitationTagsWhenDisabled(t *testing.T) {
	keepTags := false
	old := testGatewayCfg
	testGatewayCfg = &config.Config{Gateway: &config.GatewayConfig{StripCitationTags: &keepTags}}
	defer func() { testGatewayCfg = old }()

	fakes := streamFakes(taggedScript)
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	frames := parseSSE(t, rec.Body.String())
	var content strings.Builder
	for i := 1; i < len(frames)-1; i++ {
		chunk := decodeChunk(t, frames[i])
		if chunk.Choices[0].Delta != nil {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	assert.Contains(t, content.String(), `<kb doc="手册.md" chunk_id="c1" kb_id="kb1" />`)
	assert.Contains(t, content.String(), `<web url="https://example.com" title="官网" />`)
	assert.Contains(t, content.String(), "参考来源")
}

func TestChatCompletionsStripsCitationTags(t *testing.T) {
	fakes := streamFakes(taggedScript)
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "user-9")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "根据手册保修 2 年。\n---\n参考来源:\n1. 手册", resp.Choices[0].Message.Content)
	assert.NotContains(t, resp.Choices[0].Message.Content, "<kb")
}

package openai_compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrandRewriterReplacesAcrossChunks(t *testing.T) {
	r := &brandRewriter{}

	// The partial token is held back until its tail arrives.
	assert.Equal(t, "先看 ", r.feed("先看 WeK"))
	assert.Equal(t, brandReplacement+" 的", r.feed("noRa 的"))
	// A trailing partial opening is held for the next chunk, then completed.
	assert.Equal(t, "内容 ", r.feed("内容 wek"))
	assert.Equal(t, brandReplacement, r.feed("nora"))
	assert.Equal(t, "", r.flush())
}

func TestBrandRewriterFlushReleasesHeldFragment(t *testing.T) {
	r := &brandRewriter{}
	assert.Equal(t, "尾巴 ", r.feed("尾巴 wekn"))
	// An unterminated partial term is never invented into a replacement.
	assert.Equal(t, "wekn", r.flush())
	assert.Equal(t, "", r.flush())
}

func TestReplaceBrandFold(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"canonical", "Ask WeKnora about it", "Ask " + brandReplacement + " about it"},
		{"lower", "weknora docs", brandReplacement + " docs"},
		{"mixed", "WEKNORA", brandReplacement},
		{"embedded", "search_weknora_docs", "search_" + brandReplacement + "_docs"},
		{"none", "nothing here", "nothing here"},
		{"multibyte prefix", "中文 WeKnora", "中文 " + brandReplacement},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replaceBrandFold(tt.in))
		})
	}
}

func TestChatCompletionsStreamRewritesThoughtAndAnswer(t *testing.T) {
	fakes := streamFakes(func(bus *event.EventBus, _ *types.QARequest) error {
		emit := func(typ event.EventType, data interface{}) error {
			return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
		}
		// The product name is split across chunks: the rewrite must hold the
		// partial token until its tail arrives.
		if err := emit(event.EventAgentThought, event.AgentThoughtData{Content: "WeK"}); err != nil {
			return err
		}
		if err := emit(event.EventAgentThought, event.AgentThoughtData{Content: "noRa 需要先检索"}); err != nil {
			return err
		}
		if err := emit(event.EventAgentThought, event.AgentThoughtData{Done: true}); err != nil {
			return err
		}
		if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "WeKnora 的回答", Done: true}); err != nil {
			return err
		}
		return emit(event.EventAgentComplete, event.AgentCompleteData{FinalAnswer: "WeKnora 的回答"})
	})
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var reasoning, content strings.Builder
	for _, f := range parseSSE(t, rec.Body.String()) {
		if f.Raw == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(f.Raw), &chunk))
		if d := chunk.Choices[0].Delta; d != nil {
			reasoning.WriteString(d.ReasoningContent)
			content.WriteString(d.Content)
		}
	}
	assert.Equal(t, brandReplacement+" 需要先检索", reasoning.String())
	assert.Equal(t, brandReplacement+" 的回答", content.String())
	assert.NotContains(t, reasoning.String()+content.String(), brandProductName)
}

func TestChatCompletionsNonStreamRewritesAnswer(t *testing.T) {
	fakes := streamFakes(func(bus *event.EventBus, _ *types.QARequest) error {
		emit := func(typ event.EventType, data interface{}) error {
			return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
		}
		if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "WeKnora 的回答", Done: true}); err != nil {
			return err
		}
		return emit(event.EventAgentComplete, event.AgentCompleteData{FinalAnswer: "WeKnora 的回答"})
	})
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ChatCompletionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Choices, 1)
	require.NotNil(t, resp.Choices[0].Message)
	assert.Equal(t, brandReplacement+" 的回答", resp.Choices[0].Message.Content)
}

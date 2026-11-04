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
	r := newBrandRewriter("Doc QA")

	// The partial token is held back until its tail arrives.
	assert.Equal(t, "先看 ", r.feed("先看 WeK"))
	assert.Equal(t, "Doc QA 的", r.feed("noRa 的"))
	// A trailing partial opening is held for the next chunk, then completed.
	assert.Equal(t, "内容 ", r.feed("内容 wek"))
	assert.Equal(t, "Doc QA", r.feed("nora"))
	assert.Equal(t, "", r.flush())
}

func TestBrandRewriterFlushReleasesHeldFragment(t *testing.T) {
	r := newBrandRewriter("Doc QA")
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
		{"canonical", "Ask WeKnora about it", "Ask Doc QA about it"},
		{"lower", "weknora docs", "Doc QA docs"},
		{"mixed", "WEKNORA", "Doc QA"},
		{"embedded", "search_weknora_docs", "search_Doc QA_docs"},
		{"none", "nothing here", "nothing here"},
		{"multibyte prefix", "中文 WeKnora", "中文 Doc QA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replaceBrandFold(tt.in, "Doc QA"))
		})
	}
}

func TestBrandRewriterDisabled(t *testing.T) {
	for _, name := range []string{"", "  ", "weknora", "WeKnora"} {
		r := newBrandRewriter(name)
		assert.Equal(t, "WeKnora tool", r.feed("WeKnora tool"))
		assert.Equal(t, "", r.flush())
	}
}

func TestChatCompletionsStreamThoughtUsesAgentName(t *testing.T) {
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
		if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "答案", Done: true}); err != nil {
			return err
		}
		return emit(event.EventAgentComplete, event.AgentCompleteData{FinalAnswer: "答案"})
	})
	rec := serveCompletions(t, fakes, streamBody("agt-1"), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var reasoning strings.Builder
	for _, f := range parseSSE(t, rec.Body.String()) {
		if f.Raw == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		require.NoError(t, json.Unmarshal([]byte(f.Raw), &chunk))
		if d := chunk.Choices[0].Delta; d != nil {
			reasoning.WriteString(d.ReasoningContent)
		}
	}
	assert.Equal(t, "Doc QA 需要先检索", reasoning.String())
	assert.NotContains(t, reasoning.String(), brandProductName)
}

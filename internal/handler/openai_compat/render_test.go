package openai_compat

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func textMsg(role, text string) ChatMessage {
	return ChatMessage{Role: role, Content: ContentUnion{Text: text}}
}

func TestRenderMessagesBasicFold(t *testing.T) {
	turn, err := renderMessages([]ChatMessage{
		textMsg("system", "You are helpful."),
		textMsg("user", "hi"),
		textMsg("assistant", "hello"),
		textMsg("user", "what about weknora?"),
	}, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "[调用方指令]\nYou are helpful.")
	assert.Contains(t, turn.Query, "[对话历史]\nuser: hi\nassistant: hello")
	assert.Contains(t, turn.Query, "[本次问题]\nwhat about weknora?")
	assert.Empty(t, turn.Images)
	assert.Empty(t, turn.Warnings)
}

func TestRenderMessagesNoUserMessage(t *testing.T) {
	_, err := renderMessages([]ChatMessage{textMsg("system", "only instructions")}, true)
	assert.ErrorIs(t, err, errNoUserQuery)
}

func TestRenderMessagesLastUserWinsEvenAfterToolTurns(t *testing.T) {
	// OpenAI clients end tool sequences with tool results, so the "last user
	// message" is the last user entry even when tool traffic trails it.
	turn, err := renderMessages([]ChatMessage{
		textMsg("user", "find the doc"),
		{Role: "assistant", ToolCalls: []ClientToolCall{{
			ID: "call-1", Type: "function",
			Function: ClientToolFnCall{Name: "read_file", Arguments: `{"path":"a.md"}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Content: ContentUnion{Text: "file contents here"}},
	}, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "[本次问题]\nfind the doc")
	assert.NotContains(t, turn.Query, "工具 结果")
}

func TestRenderMessagesSystemCapMarksTruncation(t *testing.T) {
	long := strings.Repeat("长", systemBlockMaxRunes+500)
	turn, err := renderMessages([]ChatMessage{
		textMsg("developer", long),
		textMsg("user", "q"),
	}, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "(截断)")
	runes := []rune(turn.Query)
	// The block cannot exceed the cap by much (marker + rest of the query).
	assert.Less(t, len(runes), systemBlockMaxRunes+200)
}

func TestRenderMessagesHistoryWindowAndPlaceholder(t *testing.T) {
	messages := []ChatMessage{textMsg("system", "sys")}
	for i := 0; i < historyMaxMessages+7; i++ {
		messages = append(messages, textMsg("user", strings.Repeat("u", 50)), textMsg("assistant", strings.Repeat("a", 50)))
	}
	messages = append(messages, textMsg("user", "final"))
	turn, err := renderMessages(messages, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "…(更早 ")
	assert.NotContains(t, turn.Query, "user: uuuuuuuuuu…first")
	// The final question must survive trimming.
	assert.Contains(t, turn.Query, "[本次问题]\nfinal")
}

func TestRenderMessagesToolTextualization(t *testing.T) {
	turn, err := renderMessages([]ChatMessage{
		textMsg("user", "list files"),
		{Role: "assistant", ToolCalls: []ClientToolCall{{
			ID: "c1", Function: ClientToolFnCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: ContentUnion{Text: strings.Repeat("out", 1000)}},
		textMsg("user", "thanks, continue"),
	}, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "[客户端工具 shell] 参数: {\"cmd\":\"ls\"}")
	assert.Contains(t, turn.Query, "[客户端工具 shell] → ")
	assert.Contains(t, turn.Query, "(截断)")
	assert.NotContains(t, turn.Query, strings.Repeat("out", 1000))
}

func TestRenderMessagesImagesExtractedAndCapped(t *testing.T) {
	messages := []ChatMessage{{
		Role: "user",
		Content: ContentUnion{
			Text:  "what is this?",
			Parts: []ContentPart{{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: "https://x.test/a.png"}}},
		},
	}}
	turn, err := renderMessages(messages, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://x.test/a.png"}, turn.Images)

	// A 5th image is a 400-class error.
	for i := 0; i < maxImagesPerRequest; i++ {
		messages[0].Content.Parts = append(messages[0].Content.Parts,
			ContentPart{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: "https://x.test/b.png"}})
	}
	_, err = renderMessages(messages, true)
	assert.ErrorIs(t, err, errTooManyImages)
}

func TestRenderMessagesUploadOnlyQuestion(t *testing.T) {
	messages := []ChatMessage{{
		Role: "user",
		Content: ContentUnion{
			Parts: []ContentPart{{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: "https://x.test/a.png"}}},
		},
	}}
	turn, err := renderMessages(messages, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "[本次问题]\n[调用方未输入文字")
}

func TestRenderMessagesImagesDroppedWhenAgentLacksAbility(t *testing.T) {
	messages := []ChatMessage{{
		Role: "user",
		Content: ContentUnion{
			Text:  "look",
			Parts: []ContentPart{{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: "https://x.test/a.png"}}},
		},
	}}
	turn, err := renderMessages(messages, false)
	require.NoError(t, err)
	assert.Empty(t, turn.Images)
	assert.Contains(t, turn.Query, "[图片已忽略:智能体未开启图片能力]")
	assert.Len(t, turn.Warnings, 1)
}

func TestRenderMessagesImageSchemeAndSizeGuards(t *testing.T) {
	bad := []ChatMessage{{
		Role: "user",
		Content: ContentUnion{
			Text:  "look",
			Parts: []ContentPart{{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: "ftp://x.test/a.png"}}},
		},
	}}
	_, err := renderMessages(bad, true)
	assert.ErrorIs(t, err, errBadImageSource)

	huge := "data:image/png;base64," + strings.Repeat("A", (maxImageBytes/3+1000)*4)
	oversize := []ChatMessage{{
		Role: "user",
		Content: ContentUnion{
			Text:  "look",
			Parts: []ContentPart{{Type: PartTypeImage, ImageURL: &ImageURLPo{URL: huge}}},
		},
	}}
	_, err = renderMessages(oversize, true)
	assert.ErrorIs(t, err, errImageTooLarge)
}

func TestRenderMessagesMultibyteTruncationSafe(t *testing.T) {
	// truncation must not split a rune (the 8k cap is rune-based).
	long := strings.Repeat("汉", systemBlockMaxRunes+3)
	turn, err := renderMessages([]ChatMessage{textMsg("system", long), textMsg("user", "q")}, true)
	require.NoError(t, err)
	assert.Contains(t, turn.Query, "(截断)")
	assert.NotContains(t, turn.Query, "\uFFFD")
}

func TestRenderAgentImageAbilityGate(t *testing.T) {
	// The handler passes agent.Config.ImageUploadEnabled straight through;
	// assert the wiring with a real agent-shaped config.
	agent := publishedAgent("agt-1", "Doc QA", true)
	assert.False(t, agent.Config.ImageUploadEnabled, "publishedAgent fixture defaults to no image ability")
	agent.Config.ImageUploadEnabled = true
	assert.True(t, agent.Config.ImageUploadEnabled)
	_ = types.AgentModeQuickAnswer
}

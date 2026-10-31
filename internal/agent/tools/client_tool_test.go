package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nopTool is the smallest internal tool for registry-marker tests.
type nopTool struct{ BaseTool }

func (t *nopTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	return &types.ToolResult{Success: true}, nil
}

func TestClientToolRegistryMarkers(t *testing.T) {
	registry := NewToolRegistry()
	registry.RegisterTool(&nopTool{BaseTool: NewBaseTool("builtin", "", nil)})
	client := NewClientTool("read_file", "", "Reads a file", json.RawMessage(`{"type":"object"}`), nil)
	registry.RegisterTool(client)

	assert.False(t, registry.IsClientTool("builtin"))
	assert.True(t, registry.IsClientTool("read_file"))
	assert.False(t, registry.IsClientTool("missing"))
	assert.Equal(t, []string{"read_file"}, registry.ClientToolNames())
	assert.Equal(t, "read_file", client.WireName(), "empty wire keeps the name")

	renamed := NewClientTool("client_read_file", "read_file", "", json.RawMessage(`{}`), nil)
	assert.Equal(t, "read_file", renamed.WireName(), "a collided registration keeps its wire name")
}

func TestClientToolWithoutContextDegrades(t *testing.T) {
	// No ToolExecContext in ctx at all (plain background call).
	tool := NewClientTool("read_file", "", "", json.RawMessage(`{}`), nil)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Success, "degradation never fails the run")
	assert.Contains(t, result.Output, "model gateway")
}

func TestExternalToolResultCapsAndMapsErrors(t *testing.T) {
	long := strings.Repeat("字", types.MaxExternalToolResultRunes+10)
	result := externalToolResult(&types.ExternalToolCallResult{CallID: "c1", Content: long})
	require.NotNil(t, result)
	assert.True(t, result.Success)
	assert.Contains(t, result.Output, "…[truncated]")
	assert.LessOrEqual(t, len([]rune(result.Output)), types.MaxExternalToolResultRunes+20)

	errResult := externalToolResult(&types.ExternalToolCallResult{CallID: "c1", Content: "boom", IsError: true})
	assert.False(t, errResult.Success)
	assert.Equal(t, "boom", errResult.Error)
}

func TestResultFromPromptPicksCallResult(t *testing.T) {
	prompt := &types.AgentPrompt{
		Kind: types.AgentPromptKindExternalToolCall,
		Response: types.JSON(`{"results":[
			{"call_id":"a","content":"one"},
			{"call_id":"b","content":"two","is_error":true}]}`),
	}
	got := resultFromPrompt(prompt, "b")
	require.NotNil(t, got)
	assert.True(t, got.IsError)
	assert.Equal(t, "two", got.Content)
	assert.Nil(t, resultFromPrompt(prompt, "missing"))
	assert.Nil(t, resultFromPrompt(&types.AgentPrompt{Response: types.JSON(`{}`)}, "a"))
}

// TestExternalToolResultSanitizesControlChars proves the M3-2 boundary: a
// hostile client result cannot smuggle control characters into the model
// context; newlines are normalized and the trailing trim applies.
func TestExternalToolResultSanitizesControlChars(t *testing.T) {
	got := externalToolResult(&types.ExternalToolCallResult{
		CallID:  "c1",
		Content: "line1\r\nline2\x00\u001b[2Jbad\x7f\xc2\x85ok\tend\n\n",
	})
	require.NotNil(t, got)
	assert.Equal(t, "line1\nline2[2Jbadok\tend", got.Output)
	assert.True(t, got.Success)

	errRes := externalToolResult(&types.ExternalToolCallResult{CallID: "c2", IsError: true, Content: "boom\rtext"})
	require.NotNil(t, errRes)
	assert.False(t, errRes.Success)
	assert.Equal(t, "boom\ntext", errRes.Error)
}

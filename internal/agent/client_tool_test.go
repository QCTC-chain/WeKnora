package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// fakeExternalCaller stands in for the approval gate's external-tool bridge:
// it records the raised batch and either suspends (gateway answers later) or
// answers in place.
type fakeExternalCaller struct {
	requests []approval.ExternalToolRequest
	// suspend makes the raised prompt park the run.
	suspend bool
	// inPlace answers the raised batch immediately.
	inPlace *types.ExternalToolCallPromptResponse
}

func (f *fakeExternalCaller) RequestExternalToolCallAndWait(
	ctx context.Context, req approval.ExternalToolRequest,
) (approval.ExternalToolAnswer, error) {
	f.requests = append(f.requests, req)
	if f.suspend {
		return approval.ExternalToolAnswer{
			Suspended: &types.PromptSuspension{
				PromptID: "p-ext", Kind: types.AgentPromptKindExternalToolCall,
			},
		}, nil
	}
	return approval.ExternalToolAnswer{
		Answered: true, PromptID: "p-ext", Response: *f.inPlace,
	}, nil
}

func (f *fakeExternalCaller) ResumeExternalToolAnswer(
	ctx context.Context, prompt *types.AgentPrompt, bus *event.EventBus,
) approval.ExternalToolAnswer {
	var resp types.ExternalToolCallPromptResponse
	_ = json.Unmarshal(prompt.Response, &resp)
	return approval.ExternalToolAnswer{Answered: true, PromptID: prompt.ID, Response: resp}
}

// clientToolFixture wires an engine with one counting tool, two gateway
// client tools, and one trailing counting tool. The model calls them in that
// order in a single round.
type clientToolFixture struct {
	engine          *AgentEngine
	caller          *fakeExternalCaller
	first           *countingTool
	after           *countingTool
	completedClient *event.AgentCompleteData
}

const clientFixtureScript = "call-all"

func newClientToolFixture(t *testing.T, caller *fakeExternalCaller) *clientToolFixture {
	t.Helper()
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{{
		ResponseType: types.ResponseTypeAnswer,
		Content:      "checking",
		ToolCalls: []types.LLMToolCall{
			{ID: "call-a", Type: "function", Function: types.FunctionCall{Name: "first_tool", Arguments: `{}`}},
			{ID: "call-b", Type: "function", Function: types.FunctionCall{Name: "read_file", Arguments: `{"path":"/a"}`}},
			{ID: "call-c", Type: "function", Function: types.FunctionCall{Name: "list_dir", Arguments: `{"dir":"/tmp"}`}},
			{ID: "call-d", Type: "function", Function: types.FunctionCall{Name: "after_tool", Arguments: `{}`}},
		},
		Done:         true,
		FinishReason: "tool_calls",
	}}}}}
	f := &clientToolFixture{
		engine: newTestEngine(t, model),
		caller: caller,
		first:  newCountingTool("first_tool"),
		after:  newCountingTool("after_tool"),
	}
	f.engine.toolRegistry = tools.NewToolRegistry()
	f.engine.toolRegistry.RegisterTool(f.first)
	f.engine.toolRegistry.RegisterTool(tools.NewClientTool("read_file", "", "Reads a file",
		json.RawMessage(`{"type":"object"}`), caller))
	f.engine.toolRegistry.RegisterTool(tools.NewClientTool("list_dir", "", "Lists a directory",
		json.RawMessage(`{"type":"object"}`), caller))
	f.engine.toolRegistry.RegisterTool(f.after)
	f.engine.eventBus.On(event.EventAgentComplete, func(_ context.Context, evt event.Event) error {
		data := evt.Data.(event.AgentCompleteData)
		f.completedClient = &data
		return nil
	})
	return f
}

func (f *clientToolFixture) run(t *testing.T) *types.AgentState {
	t.Helper()
	state := &types.AgentState{}
	f.engine.beginTurn("test query", nil, state)
	_, err := f.engine.executeLoop(context.Background(), state, "test query",
		emptyMessages(), emptyTools(), "sess-1", "msg-1")
	require.NoError(t, err)
	return state
}

func TestClientToolCallSuspendsWithWholeBatch(t *testing.T) {
	caller := &fakeExternalCaller{suspend: true}
	f := newClientToolFixture(t, caller)
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))

	state := f.run(t)

	require.NotNil(t, state.Suspended)
	require.Equal(t, types.AgentPromptKindExternalToolCall, state.Suspended.Kind)
	require.False(t, state.IsComplete)
	require.Len(t, caller.requests, 1, "one batch prompt for the whole round")
	batch := caller.requests[0].Request.Calls
	require.Len(t, batch, 2, "both client calls ride in one batch")
	require.Equal(t, "call-b", batch[0].CallID)
	require.Equal(t, "read_file", batch[0].WireName)
	require.Equal(t, "call-c", batch[1].CallID)
	require.JSONEq(t, `{"dir":"/tmp"}`, batch[1].Arguments)
	require.Equal(t, "call-b", caller.requests[0].ToolCallID)
	require.NotNil(t, caller.requests[0].Snapshot, "the suspension must carry a run snapshot")

	require.Equal(t, 1, f.first.calls)
	require.Zero(t, f.after.calls, "calls after the suspended client call must not run")

	require.NotNil(t, f.completedClient)
	require.Equal(t, state.Suspended, f.completedClient.Suspended)

	snap, err := DecodeRunSnapshot(caller.requests[0].Snapshot)
	require.NoError(t, err)
	require.Equal(t, 1, snap.PendingRound.BlockedIndex)
	require.Len(t, snap.PendingRound.Calls, 4)
	require.Equal(t, "read_file", snap.PendingRound.Calls[1].Name)
	require.Equal(t, "list_dir", snap.PendingRound.Calls[2].Name)
}

func TestClientToolResumeConsumesBatchResults(t *testing.T) {
	caller := &fakeExternalCaller{suspend: true}
	f := newClientToolFixture(t, caller)
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))
	state := f.run(t)
	require.NotNil(t, state.Suspended)
	snapshot := caller.requests[0].Snapshot

	// Replica: fresh engine, same tools; the model only answers.
	model := &mockChat{responses: []mockResponse{{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "all done", Done: true, FinishReason: "stop"},
	}}}}
	replica := &clientToolFixture{
		engine: newTestEngine(t, model),
		caller: &fakeExternalCaller{},
		first:  newCountingTool("first_tool"),
		after:  newCountingTool("after_tool"),
	}
	replica.engine.toolRegistry = tools.NewToolRegistry()
	replica.engine.toolRegistry.RegisterTool(replica.first)
	replica.engine.toolRegistry.RegisterTool(tools.NewClientTool("read_file", "", "",
		json.RawMessage(`{"type":"object"}`), replica.caller))
	replica.engine.toolRegistry.RegisterTool(tools.NewClientTool("list_dir", "", "",
		json.RawMessage(`{"type":"object"}`), replica.caller))
	replica.engine.toolRegistry.RegisterTool(replica.after)
	replica.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))

	answered := &types.AgentPrompt{
		ID: "p-ext", Kind: types.AgentPromptKindExternalToolCall, ToolCallID: "call-b",
		Status: types.AgentPromptAnswered,
		Response: types.JSON(`{"results":[
			{"call_id":"call-b","content":"file contents here"},
			{"call_id":"call-c","content":"a.txt b.txt"}]}`),
	}
	resumed, err := replica.engine.Resume(context.Background(), "sess-1", "msg-1", nil, snapshot, answered)
	require.NoError(t, err)
	require.True(t, resumed.IsComplete)
	require.Nil(t, resumed.Suspended)
	require.Equal(t, "all done", resumed.FinalAnswer)

	require.Zero(t, replica.first.calls, "finished calls are not rerun")
	require.Zero(t, replica.caller.requests, "answered batches are not re-raised on resume")
	require.Equal(t, 1, replica.after.calls)

	require.Len(t, resumed.RoundSteps, 2)
	round := resumed.RoundSteps[0]
	require.Len(t, round.ToolCalls, 4)
	require.True(t, round.ToolCalls[1].Result.Success)
	require.Equal(t, "file contents here", round.ToolCalls[1].Result.Output)
	require.True(t, round.ToolCalls[2].Result.Success, "the sibling client call reads the same batch")
	require.Equal(t, "a.txt b.txt", round.ToolCalls[2].Result.Output)
	require.True(t, round.ToolCalls[3].Result.Success)
}

func TestClientToolInPlaceAnswerContinuesRun(t *testing.T) {
	caller := &fakeExternalCaller{inPlace: &types.ExternalToolCallPromptResponse{Results: []types.ExternalToolCallResult{
		{CallID: "call-b", Content: "inplace file"},
		{CallID: "call-c", Content: "inplace listing"},
	}}}
	f := newClientToolFixture(t, caller)
	model := f.engine.chatModel.(*mockChat)
	model.responses = append(model.responses, mockResponse{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "done with tools", Done: true, FinishReason: "stop"},
	}})
	f.engine.EnableSuspension(json.RawMessage(`{"agent_id":"a1"}`))

	state := f.run(t)
	require.Nil(t, state.Suspended, "an in-place answer never suspends")
	require.True(t, state.IsComplete)
	require.Equal(t, "done with tools", state.FinalAnswer)
	require.Len(t, caller.requests, 1)
	require.Equal(t, 1, f.after.calls, "the run continued past the answered batch")

	// The model saw both client results.
	sent := model.calls[1]
	toolOutputs := map[string]string{}
	for _, m := range sent {
		if m.Role == "tool" {
			toolOutputs[m.ToolCallID] = m.Content
		}
	}
	require.Equal(t, "inplace file", toolOutputs["call-b"])
	require.Equal(t, "inplace listing", toolOutputs["call-c"])
}

func TestClientToolWithoutBridgeDegradesPolitely(t *testing.T) {
	f := newClientToolFixture(t, nil)
	// nil caller → the tool answers with a keep-going note instead of failing.
	f.engine.toolRegistry = tools.NewToolRegistry()
	f.engine.toolRegistry.RegisterTool(f.first)
	f.engine.toolRegistry.RegisterTool(tools.NewClientTool("read_file", "", "",
		json.RawMessage(`{"type":"object"}`), nil))
	f.engine.toolRegistry.RegisterTool(tools.NewClientTool("list_dir", "", "",
		json.RawMessage(`{"type":"object"}`), nil))
	f.engine.toolRegistry.RegisterTool(f.after)
	model := f.engine.chatModel.(*mockChat)
	model.responses = append(model.responses, mockResponse{chunks: []types.StreamResponse{
		{ResponseType: types.ResponseTypeAnswer, Content: "went on without it", Done: true, FinishReason: "stop"},
	}})

	state := f.run(t)
	require.True(t, state.IsComplete)
	require.Nil(t, state.Suspended)
	require.Len(t, state.RoundSteps, 2)
	for _, call := range state.RoundSteps[0].ToolCalls {
		if call.Name == "read_file" {
			require.True(t, call.Result.Success, "degradation answers success so the model can continue")
			require.Contains(t, call.Result.Output, "model gateway")
		}
	}
}

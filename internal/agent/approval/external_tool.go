package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/hitl"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
)

// ExternalToolRequest raises one AgentPromptKindExternalToolCall for a batch
// of model-requested client tool calls (M3-1). Snapshot, when set, lets the
// run suspend after the hot wait so the gateway can return the calls to the
// external client.
type ExternalToolRequest struct {
	TenantID           uint64
	UserID             string
	SessionID          string
	AssistantMessageID string
	RequestID          string
	// ToolCallID is the blocked call the run waits on; the resume path
	// matches the answered prompt against it.
	ToolCallID      string
	EventBus        *event.EventBus
	Request         types.ExternalToolCallPromptRequest
	Snapshot        []byte
	SnapshotVersion int
}

// Answer carries the client's results for one external tool call batch.
type ExternalToolAnswer struct {
	owner    string
	answered bool
	PromptID string
	// Answered means Results carries one result per requested call.
	Answered bool
	Response types.ExternalToolCallPromptResponse
	Reason   string
	TimedOut bool
	Canceled bool
	// Suspended is set when the run must end and wait for the gateway.
	Suspended *types.PromptSuspension
}

// ExternalToolCaller is the surface the client tool uses (mockable in tests).
type ExternalToolCaller interface {
	RequestExternalToolCallAndWait(ctx context.Context, req ExternalToolRequest) (ExternalToolAnswer, error)
	// ResumeExternalToolAnswer is the answer carried by the prompt a resumed
	// run was suspended on.
	ResumeExternalToolAnswer(ctx context.Context, prompt *types.AgentPrompt, bus *event.EventBus) ExternalToolAnswer
}

var _ ExternalToolCaller = (*Gate)(nil)

// RequestExternalToolCallAndWait raises the external_tool_call prompt and
// waits for the gateway's results, the hot wait, or the suspension.
func (g *Gate) RequestExternalToolCallAndWait(
	ctx context.Context, req ExternalToolRequest,
) (ExternalToolAnswer, error) {
	if g == nil {
		return ExternalToolAnswer{}, fmt.Errorf("external tool call: nil gate")
	}
	if req.EventBus == nil {
		return ExternalToolAnswer{}, fmt.Errorf("external tool call: EventBus is nil")
	}
	if len(req.Request.Calls) == 0 ||
		len(req.Request.Calls) > types.MaxExternalToolCallsPerRound {
		return ExternalToolAnswer{}, fmt.Errorf(
			"external tool call: %d calls in batch", len(req.Request.Calls))
	}
	request, err := json.Marshal(req.Request)
	if err != nil {
		return ExternalToolAnswer{}, fmt.Errorf("encode request: %w", err)
	}
	wait := g.timeout
	var suspend *hitl.SuspendSpec
	if req.Snapshot != nil {
		wait = g.hotWait
		suspend = &hitl.SuspendSpec{Snapshot: req.Snapshot, SnapshotVersion: req.SnapshotVersion}
	}
	out, err := g.broker.Await(ctx, hitl.Spec{
		Suspend:         suspend,
		FallbackTimeout: g.timeout,
		Prompt: &types.AgentPrompt{
			TenantID:           req.TenantID,
			UserID:             req.UserID,
			SessionID:          req.SessionID,
			AssistantMessageID: req.AssistantMessageID,
			RequestID:          req.RequestID,
			Kind:               types.AgentPromptKindExternalToolCall,
			ToolCallID:         req.ToolCallID,
			Request:            types.JSON(request),
		},
		Timeout:       wait,
		TimeoutReason: "no client result in time",
		Emit: func(ctx context.Context, p *types.AgentPrompt) error {
			waitSec := promptWaitSeconds(p, wait, g.timeout)
			return req.EventBus.Emit(ctx, event.Event{
				ID:        p.ID + "-prompt-required",
				Type:      event.EventAgentPromptRequired,
				SessionID: req.SessionID,
				Data: event.AgentPromptRequiredData{
					PendingID:          p.ID,
					Kind:               types.AgentPromptKindExternalToolCall,
					SessionID:          req.SessionID,
					AssistantMessageID: req.AssistantMessageID,
					Request:            request,
					TimeoutSeconds:     waitSec,
					HotWaitSeconds:     waitSec,
					Suspendable:        p.Suspendable,
					RequestedAtUnix:    time.Now().Unix(),
					ToolCallID:         req.ToolCallID,
					RequestID:          req.RequestID,
				},
				Metadata: map[string]interface{}{
					"assistant_message_id": req.AssistantMessageID,
					"pending_id":           p.ID,
				},
				RequestID: req.RequestID,
			})
		},
	})
	if err != nil {
		return ExternalToolAnswer{}, fmt.Errorf("external tool call: %w", err)
	}
	if out.Suspended {
		return ExternalToolAnswer{
			PromptID: out.Prompt.ID,
			Suspended: &types.PromptSuspension{
				Detached:  out.Detached,
				PromptID:  out.Prompt.ID,
				Kind:      types.AgentPromptKindExternalToolCall,
				ExpiresAt: out.ExpiresAt,
			},
		}, nil
	}
	a := externalToolAnswer(out)
	g.consumeExternalAnswer(ctx, &a, req.EventBus, req.SessionID, req.AssistantMessageID, req.RequestID)
	return a, nil
}

// ResumeExternalToolAnswer implements ExternalToolCaller.
func (g *Gate) ResumeExternalToolAnswer(
	ctx context.Context, prompt *types.AgentPrompt, bus *event.EventBus,
) ExternalToolAnswer {
	a := externalToolAnswer(hitl.Outcome{Prompt: prompt, Answered: true})
	g.consumeExternalAnswer(ctx, &a, bus, prompt.SessionID, prompt.AssistantMessageID, prompt.RequestID)
	return a
}

func externalToolAnswer(out hitl.Outcome) ExternalToolAnswer {
	a := ExternalToolAnswer{
		owner: out.Prompt.OwnerInstance, answered: out.Answered, PromptID: out.Prompt.ID,
		Reason: out.Reason, TimedOut: out.TimedOut, Canceled: out.Canceled,
	}
	if !out.Answered {
		return a
	}
	if err := json.Unmarshal(out.Prompt.Response, &a.Response); err != nil {
		a.Reason = "unreadable client results"
		return a
	}
	a.Answered = true
	return a
}

// consumeExternalAnswer records the batch as consumed and tells the UI.
func (g *Gate) consumeExternalAnswer(
	ctx context.Context, a *ExternalToolAnswer, bus *event.EventBus,
	sessionID, assistantMessageID, requestID string,
) {
	consumed, consumeErr := g.broker.Consume(ctx, a.PromptID, types.AgentPromptDone, a.owner)
	if !consumed && a.answered {
		a.Answered = false
		a.Response = types.ExternalToolCallPromptResponse{}
		a.Reason = "answer is no longer owned or was already consumed"
		if consumeErr == nil {
			a.Suspended = &types.PromptSuspension{
				PromptID: a.PromptID, Kind: types.AgentPromptKindExternalToolCall, Detached: true,
			}
			return
		}
		a.Reason = "could not confirm the client results"
	}
	if bus == nil {
		return
	}
	var response json.RawMessage
	if a.Answered {
		response, _ = json.Marshal(a.Response)
	}
	_ = bus.Emit(context.WithoutCancel(ctx), event.Event{
		ID:        a.PromptID + "-prompt-resolved",
		Type:      event.EventAgentPromptResolved,
		SessionID: sessionID,
		Data: event.AgentPromptResolvedData{
			PendingID: a.PromptID,
			Kind:      types.AgentPromptKindExternalToolCall,
			Answered:  a.Answered,
			Response:  response,
			Reason:    a.Reason,
			TimedOut:  a.TimedOut,
			Canceled:  a.Canceled,
		},
	})
}

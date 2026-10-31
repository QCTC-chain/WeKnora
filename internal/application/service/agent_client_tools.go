package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/agent/approval"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// registerClientTools registers the gateway caller's tool definitions as
// client tools (M3-1): each one suspends the run on an external_tool_call
// prompt instead of executing locally. A name that collides with an internal
// tool registers under client_<name> and keeps its wire name for the caller,
// so the model can never silently reroute a client call into a built-in.
// Returns the registered client tool names (also used by the client_only
// gateway tool policy to hide internal tools). gatewayToolPolicy "none"
// registers nothing.
func (s *agentService) registerClientTools(
	ctx context.Context, toolRegistry *tools.ToolRegistry, config *types.AgentConfig,
) []string {
	if config == nil || len(config.ClientTools) == 0 || config.GatewayToolPolicy == "none" {
		return nil
	}
	bridge, ok := s.toolApprovalGate.(approval.ExternalToolCaller)
	if !ok || bridge == nil {
		logger.Warnf(ctx, "[Agent] client tools requested but no external tool bridge is available")
		return nil
	}
	var names []string
	for i, spec := range config.ClientTools {
		if err := validateClientToolSpec(spec); err != nil {
			logger.Warnf(ctx, "[Agent] dropping client tool %d: %v", i, err)
			continue
		}
		name := spec.Name
		if _, takenErr := toolRegistry.GetTool(name); takenErr == nil {
			alt := "client_" + name
			if _, takenErr2 := toolRegistry.GetTool(alt); takenErr2 == nil {
				logger.Warnf(ctx, "[Agent] dropping client tool %s: collides with internal tools", spec.Name)
				continue
			}
			name = alt
		}
		params := json.RawMessage(spec.Parameters)
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		toolRegistry.RegisterTool(tools.NewClientTool(
			name, spec.Name, spec.Description, params, bridge,
		))
		names = append(names, name)
	}
	return names
}

// validateClientToolSpec enforces the same limits the gateway applies to the
// wire request, so a runtime-only mutation cannot widen them.
func validateClientToolSpec(spec types.ClientToolSpec) error {
	if runes := len([]rune(spec.Name)); runes == 0 || runes > types.MaxExternalToolNameRunes {
		return fmt.Errorf("tool name must be 1-%d runes", types.MaxExternalToolNameRunes)
	}
	if runes := len([]rune(spec.Description)); runes > types.MaxExternalToolDescRunes {
		return fmt.Errorf("tool %s: description exceeds %d runes", spec.Name, types.MaxExternalToolDescRunes)
	}
	if len(spec.Parameters) > types.MaxExternalToolParamsBytes {
		return fmt.Errorf("tool %s: parameters exceed %d bytes", spec.Name, types.MaxExternalToolParamsBytes)
	}
	if len(spec.Parameters) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(spec.Parameters, &probe); err != nil {
			return fmt.Errorf("tool %s: parameters are not a JSON object: %w", spec.Name, err)
		}
	}
	return nil
}

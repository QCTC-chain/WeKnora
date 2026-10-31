package router

import (
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/sandbox/allocation"
)

func deploymentCapabilitiesFromRouter(params RouterParams) handler.DeploymentCapabilitiesData {
	return handler.BuildDeploymentCapabilities(handler.Edition, handler.DeploymentFeatureAvailability{
		Organizations: params.OrganizationHandler != nil,
		Agents:        params.CustomAgentHandler != nil,
		IM:            params.IMHandler != nil,
		// Match RegisterEmbedChannelRoutes: management routes depend on handler only.
		Embed: params.EmbedChannelHandler != nil,
		// Match RegisterMCPEndpointRoutes / RegisterMCPServerRoutes.
		MCPServer: params.MCPEndpointHandler != nil && params.MCPServer != nil && params.MCPEndpointService != nil,
		API:       params.TenantHandler != nil && params.TenantAPIKeyService != nil,
		// Same gate as RegisterOpenAICompatRoutes (/v1 routes); nil-safe for
		// tests that build partial RouterParams.
		Gateway: params.Config != nil && params.Config.Gateway != nil && params.Config.Gateway.IsEnabled(),
		MCP: params.MCPServiceHandler != nil &&
			params.MCPCredentialsHandler != nil &&
			params.MCPOAuthHandler != nil,
		WebSearch: params.WebSearchHandler != nil &&
			params.WebSearchProviderHandler != nil &&
			params.WebSearchCredentialsHandler != nil,
		VectorStore:           params.VectorStoreHandler != nil,
		Storage:               params.StorageBackendHandler != nil,
		Sandbox:               params.SandboxConfigHandler != nil,
		SandboxDocker:         sandbox.DockerBackendEnabled(),
		SandboxHost:           params.HostSandbox.Manager != nil,
		SandboxRemote:         params.SandboxConfigHandler != nil && !params.HostSandbox.Desktop,
		SandboxUserAllocation: allocation.UserModeAvailable(),
	})
}

package telemetry

// Sproozi attribute keys. Run keys mirror the audit record's runID and runUID:
// the gateway's view of one visit (an AgentRun, or a Docker allocation).
const (
	ExecutionID     = "sproozi.execution.id"
	SessionID       = "sproozi.session.id"
	VisitID         = "sproozi.visit.id"
	RunID           = "sproozi.run.id"
	RunUID          = "sproozi.run.uid"
	Backend         = "sproozi.backend"
	Placement       = "sproozi.placement"
	Policy          = "sproozi.policy.name"
	Capability      = "sproozi.capability"
	Tier            = "sproozi.tier"
	Service         = "sproozi.service"
	Operation       = "sproozi.operation"
	Resource        = "sproozi.resource"
	Allowed         = "sproozi.allowed"
	DenyReason      = "sproozi.deny_reason"
	TelemetrySource = "sproozi.telemetry.source"
)

// OpenTelemetry GenAI and MCP semantic convention keys. Those conventions are
// still in development; these are the names GitHub Copilot also emits.
const (
	GenAIOperationName  = "gen_ai.operation.name"
	GenAIProviderName   = "gen_ai.provider.name"
	GenAIRequestModel   = "gen_ai.request.model"
	GenAIConversationID = "gen_ai.conversation.id"
	GenAIAgentName      = "gen_ai.agent.name"
	GenAIInputTokens    = "gen_ai.usage.input_tokens"
	GenAIOutputTokens   = "gen_ai.usage.output_tokens"
	GenAITokenType      = "gen_ai.token.type"
	GenAIToolName       = "gen_ai.tool.name"
	MCPMethodName       = "mcp.method.name"
	ErrorType           = "error.type"
	HTTPRequestMethod   = "http.request.method"
	HTTPResponseStatus  = "http.response.status_code"
	ServerAddress       = "server.address"
)

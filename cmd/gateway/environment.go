package main

// The environment variables the gateway reads. Each is named once here or
// beside the code that validates it (maxConnectionsEnv, openAIKeyEnv and the
// others in main.go), and gatewayEnvironment lists them all.
// environment_test.go checks that every read in this package names one of
// them and that docs/reference/required-secrets-and-config.md lists each.
const (
	enabledCapabilitiesEnv     = "SPROOZI_ENABLED_CAPABILITIES"
	gatewayHostEnv             = "SPROOZI_GATEWAY_HOST"
	gatewayCertFileEnv         = "SPROOZI_GATEWAY_CERT_FILE"
	gatewayKeyFileEnv          = "SPROOZI_GATEWAY_KEY_FILE"
	goMemLimitEnv              = "GOMEMLIMIT"
	budgetLedgerRetentionEnv   = "BUDGET_LEDGER_RETENTION"
	egressProfilesPathEnv      = "EGRESS_PROFILES_PATH"
	mcpServersPathEnv          = "MCP_SERVERS_PATH"
	modelPricingPathEnv        = "MODEL_PRICING_PATH"
	openAIUpstreamURLEnv       = "OPENAI_UPSTREAM_URL"
	anthropicUpstreamURLEnv    = "ANTHROPIC_UPSTREAM_URL"
	githubAppIDEnv             = "GITHUB_APP_ID"
	githubAppInstallationIDEnv = "GITHUB_APP_INSTALLATION_ID"
	githubAppPrivateKeyPathEnv = "GITHUB_APP_PRIVATE_KEY_PATH"
	agentNamespaceEnv          = "AGENT_NAMESPACE"
	credentialNamespaceEnv     = "CREDENTIAL_NAMESPACE"
	budgetNamespaceEnv         = "BUDGET_NAMESPACE"
	gitBaseURLEnv              = "GIT_BASE_URL"
	apiBaseURLEnv              = "API_BASE_URL"
	modelDiagnosticsEnv        = "MODEL_DIAGNOSTICS"
)

// gatewayEnvironment is every environment variable the gateway reads.
var gatewayEnvironment = []string{
	enabledCapabilitiesEnv, maxConnectionsEnv, maxSessionsPerExecutionEnv, maxModelCallEnv, memoryLimitEnv,
	goMemLimitEnv, budgetLedgerRetentionEnv,
	gatewayHostEnv, gatewayCertFileEnv, gatewayKeyFileEnv,
	egressProfilesPathEnv, mcpServersPathEnv, modelPricingPathEnv,
	openAIUpstreamURLEnv, anthropicUpstreamURLEnv,
	openAIKeyEnv, anthropicKeyEnv, modelAuthModeEnv, openAITokenEnv, anthropicTokenEnv, copilotTokenEnv,
	githubAppIDEnv, githubAppInstallationIDEnv, githubAppPrivateKeyPathEnv,
	agentNamespaceEnv, credentialNamespaceEnv, budgetNamespaceEnv,
	gitBaseURLEnv, apiBaseURLEnv, modelDiagnosticsEnv,
}

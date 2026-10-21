package manifests_test

// Names and verbs shared across the manifest tests.
const (
	kindClusterRole      = "ClusterRole"
	keyKind              = "kind"
	keyMetadata          = "metadata"
	keyName              = "name"
	keyNamespace         = "namespace"
	resourcePods         = "pods"
	resourceTokenReviews = "tokenreviews"
	verbDelete           = "delete"
	verbPatch            = "patch"
	whoCLI               = "cli"
	kindRoleBinding      = "RoleBinding"
	resourceConfigMaps   = "configmaps"
	resourceAgentRuns    = "agentruns"
	verbCreate           = "create"
	verbList             = "list"
	gatewayAccount       = "sproozi-shared-gateway"
	sessionsNamespace    = "sproozi-sessions"
	verbUpdate           = "update"
	groupSproozi         = "sproozi.com"
	whoExecutor          = "executor"
	whoGateway           = "gateway"
	// resourceBudgetLedgers is the gateway's ledger resource (ADR 0045).
	resourceBudgetLedgers = "budgetledgers"
)

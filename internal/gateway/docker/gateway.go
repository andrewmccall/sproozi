package docker

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"

	"github.com/andrewmccall/sproozi/internal/harness"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

const ModelAuthority = "api.openai.com:443"

// GatewayConfig fixes the model provider and named MCP registrations. The root
// service separately owns outer listener TLS and worker proxy/CA configuration.
// No workload-selectable upstream URL or provider credential is accepted.
type GatewayConfig struct {
	Harness      harness.NativeKind
	Certificates map[string]tls.Certificate
	Model        modelgateway.HandlerConfig
	MCPRegistry  *mcpgateway.Registry
	MCPAuthority string
	MCPClient    *http.Client
	AuditLogger  audit.Writer
}

func NewGateway(owner *Owner, config GatewayConfig) (*proxytransport.Gateway, error) {
	if config.Harness.Validate() != nil || owner == nil || config.Model.ProviderAuth == nil || config.Model.UpstreamURL == "" {
		return nil, errors.New("docker gateway: fixed model provider required")
	}
	if owner.policy.Spec.Budgets[api.CapabilityModelInference].MaxCostMicros > 0 && config.Model.Pricing == nil {
		return nil, errors.New("docker gateway: trusted model pricing required")
	}
	tracker := budget.NewTrackerWithLedger(owner.ledger)
	modelConfig := config.Model
	modelConfig.AuditLogger = config.AuditLogger
	authority := ModelAuthority
	handler := &modelgateway.Handler{Config: modelConfig}
	if config.Harness == harness.NativeClaude {
		authority = modelgateway.AnthropicAuthority
		handler = modelgateway.NewAnthropicHandler(modelConfig)
	}
	dispatch := gateway.Dispatcher{authority: {Capability: string(api.CapabilityModelInference), Tier: audit.TierSemantic, Handler: handler, Budgets: tracker}}
	if config.MCPRegistry != nil {
		host, port, err := net.SplitHostPort(config.MCPAuthority)
		if err != nil || host == "" || port != "443" || (config.MCPAuthority == ModelAuthority || config.MCPAuthority == modelgateway.AnthropicAuthority) {
			return nil, errors.New("docker gateway: exact MCP authority on port 443 required")
		}
		dispatch[config.MCPAuthority] = gateway.Route{Tier: audit.TierProtocol, Handler: mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: config.MCPRegistry, HTTPClient: config.MCPClient, Budgets: tracker, AuditLogger: config.AuditLogger})}
	}
	return proxytransport.New(proxytransport.Config{Certificates: config.Certificates, Authenticate: owner.Authenticate, Revalidate: owner.Revalidate, Handler: dispatch, AllowedAuthority: dispatch.Allows})
}

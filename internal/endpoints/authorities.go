// Package endpoints names the fixed semantic service authorities that every
// gateway composition reserves. Each endpoint package owns its own authority.
package endpoints

import (
	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/endpoints/github"
	"github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/endpoints/packages"
)

// SemanticAuthorities binds each semantic service authority to its capability.
// Compositions reserve every entry, even when its module is disabled, so that
// generic egress or an MCP registration can never become a weaker route.
var SemanticAuthorities = map[string]api.CapabilityKind{
	kubernetes.Authority:     api.CapabilityKubernetesRead,
	model.OpenAIAuthority:    api.CapabilityModelInference,
	model.AnthropicAuthority: api.CapabilityModelInference,
	// A signed-in client's model and sign-in hosts (ADR 0047).
	model.ChatGPTBackendAuthority:    api.CapabilityModelInference,
	model.OpenAISignInAuthority:      api.CapabilityModelInference,
	model.AnthropicSignInAuthority:   api.CapabilityModelInference,
	model.CopilotIndividualAuthority: api.CapabilityModelInference,
	model.CopilotBusinessAuthority:   api.CapabilityModelInference,
	model.CopilotEnterpriseAuthority: api.CapabilityModelInference,
	github.GitAuthority:              api.CapabilityGitHubPullRequest,
	github.APIAuthority:              api.CapabilityGitHubPullRequest,
	packages.IndexAuthority:          api.CapabilityPackagesInstall,
	packages.FilesAuthority:          api.CapabilityPackagesInstall,
}

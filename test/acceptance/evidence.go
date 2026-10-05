package acceptance

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	kubernetesCapability = "kubernetes.read"
	githubCapability     = "github.pull_request"
)

type pullRequestDocument struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	HeadRefName    string `json:"headRefName"`
	HeadRefOID     string `json:"headRefOid"`
	BaseRefName    string `json:"baseRefName"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
}

type auditEvidence struct {
	Operation        string `json:"operation"`
	Allowed          bool   `json:"allowed"`
	DenyReason       string `json:"denyReason,omitempty"`
	UpstreamStatus   int    `json:"upstreamStatus,omitempty"`
	Capability       string `json:"capability,omitempty"`
	SemanticLevel    string `json:"semanticLevel,omitempty"`
	Service          string `json:"service,omitempty"`
	Resource         string `json:"resource,omitempty"`
	ArtifactURL      string `json:"artifactURL,omitempty"`
	PolicyName       string `json:"policyName,omitempty"`
	PolicyGeneration int64  `json:"policyGeneration"`
	TokensUsed       int64  `json:"tokensUsed,omitempty"`
	TokensRemaining  int64  `json:"tokensRemaining"`
}

var commitOID = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// Demo proof is deliberately stricter than the generic AgentRun exit contract.
func validateDemoPullRequest(pr pullRequestDocument, repository, base, runUID string) error {
	if runUID == "" || pr.Number <= 0 || pr.URL != fmt.Sprintf("https://github.com/%s/pull/%d", repository, pr.Number) ||
		pr.HeadRepository == nil || !strings.EqualFold(pr.HeadRepository.NameWithOwner, repository) ||
		pr.HeadRefName != "sproozi/"+runUID+"/fix-demo" || pr.BaseRefName != base || !commitOID.MatchString(pr.HeadRefOID) {
		return fmt.Errorf("demo pull request does not match the run, repository, base or commit")
	}
	if len(pr.Files) != 1 || pr.Files[0].Path != "demo-workload.yaml" {
		return fmt.Errorf("demo pull request must change only demo-workload.yaml")
	}
	return nil
}

func validateDemoAudit(events []auditEvidence, prURL string) error {
	var kubernetesAllowed, kubernetesDenied, githubDenied, prCreated, modelUsed bool
	for _, event := range events {
		if event.PolicyName == "" || event.PolicyGeneration < 1 || event.SemanticLevel != "semantic" {
			continue
		}
		success := event.Allowed && event.UpstreamStatus >= 200 && event.UpstreamStatus < 300
		switch event.Capability {
		case kubernetesCapability:
			kubernetesAllowed = kubernetesAllowed || success
			kubernetesDenied = kubernetesDenied || (!event.Allowed &&
				event.Resource == "/api/v1/namespaces/sproozi-demo/secrets" &&
				event.DenyReason == "operation not permitted by policy")
		case githubCapability:
			githubDenied = githubDenied || (!event.Allowed &&
				event.DenyReason == "repository not permitted by policy" &&
				event.Resource == "sproozi-denied/not-authorized")
			prCreated = prCreated || (success && event.ArtifactURL == prURL && prURL != "")
		case "model.inference":
			modelUsed = modelUsed || (success && event.TokensUsed > 0)
		}
	}
	if !kubernetesAllowed || !kubernetesDenied || !githubDenied || !prCreated || !modelUsed {
		return fmt.Errorf("demo audit incomplete: kubernetes allowed=%t denied=%t, github denied=%t PR=%t, model usage=%t",
			kubernetesAllowed, kubernetesDenied, githubDenied, prCreated, modelUsed)
	}
	return nil
}

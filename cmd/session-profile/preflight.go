package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

// defaultGatewayUser is the installed gateway's ServiceAccount.
const defaultGatewayUser = "system:serviceaccount:sproozi-system:sproozi-shared-gateway"

// preflightTimeout bounds every startup access review together, so an
// unreachable API server fails startup instead of hanging it.
var preflightTimeout = 30 * time.Second

// access is one API permission a managed session needs from one identity.
type access struct {
	who, namespace, group, resource, verb string
}

// gatewayNamespace returns the namespace of the gateway's ServiceAccount user
// name, system:serviceaccount:NAMESPACE:NAME.
func gatewayNamespace(gatewayUser string) (string, error) {
	parts := strings.Split(gatewayUser, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" ||
		len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 {
		return "", fmt.Errorf("gatewayUser %q must be the gateway's ServiceAccount user name, "+
			"system:serviceaccount:NAMESPACE:NAME (default %s)", gatewayUser, defaultGatewayUser)
	}
	return parts[2], nil
}

// preflight refuses to start when the installed RBAC cannot carry a managed
// session: the executor registering runs and keeping their accounts, and the
// gateway reading those registrations and charging the same accounts. Without
// it a wrong run namespace or budget namespace starts cleanly and fails on
// every request. It also refuses the gateway's own namespace as the run
// namespace: the executor writes ConfigMaps there, and the gateway's
// configuration ConfigMaps live there (ADR 0041). Ledgers are BudgetLedger
// resources, so the budget namespace may be the gateway's (ADR 0045).
func preflight(ctx context.Context, clients kubernetes.Interface,
	runNamespace, budgetNamespace, gatewayUser string,
) error {
	system, err := gatewayNamespace(gatewayUser)
	if err != nil {
		return err
	}
	if runNamespace == system {
		return fmt.Errorf("%s %q is the gateway's namespace (from gatewayUser %s); the executor writes "+
			"ConfigMaps there, and the gateway's configuration lives there. Use a namespace of its own, such as "+
			"sproozi-sessions (cmd/session-profile/README.md)", fieldNamespace, runNamespace, gatewayUser)
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	const self, configMaps, create, update = "", "configmaps", "create", "update"
	const sproozi, ledgers = "sproozi.com", "budgetledgers"
	needs := []access{
		{self, runNamespace, "", configMaps, create},
		{self, runNamespace, "", configMaps, update},
		{self, runNamespace, sproozi, "agentruns", create},
		{self, budgetNamespace, sproozi, ledgers, create},
		{self, budgetNamespace, sproozi, ledgers, update},
		{gatewayUser, runNamespace, "", configMaps, "get"},
		// The gateway charges the accounts the executor initializes; a gateway
		// that cannot write them uses another BUDGET_NAMESPACE.
		{gatewayUser, budgetNamespace, sproozi, ledgers, update},
	}
	for _, need := range needs {
		attributes := &authorizationv1.ResourceAttributes{Namespace: need.namespace, Group: need.group,
			Resource: need.resource, Verb: need.verb}
		var allowed bool
		if need.who == self {
			review, err := clients.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
				&authorizationv1.SelfSubjectAccessReview{
					Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attributes},
				}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("check executor permissions: %w", err)
			}
			allowed = review.Status.Allowed
		} else {
			review, err := clients.AuthorizationV1().SubjectAccessReviews().Create(ctx,
				&authorizationv1.SubjectAccessReview{
					Spec: authorizationv1.SubjectAccessReviewSpec{User: need.who, ResourceAttributes: attributes},
				}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("check gateway permissions: %w", err)
			}
			allowed = review.Status.Allowed
		}
		if !allowed {
			return denied(need, budgetNamespace)
		}
	}
	return nil
}

func denied(need access, budgetNamespace string) error {
	if need.who != "" && need.namespace == budgetNamespace && need.resource == "budgetledgers" && need.verb == "update" {
		return fmt.Errorf("the gateway (%s) cannot update budgetledgers in namespace %s, so it charges run accounts "+
			"in another namespace; set the executor's \"budgetNamespace\" to the gateway's BUDGET_NAMESPACE "+
			"(cmd/session-profile/README.md)", need.who, need.namespace)
	}
	who := "this executor"
	if need.who != "" {
		who = need.who
	}
	return fmt.Errorf("%s cannot %s %s in namespace %s; managed sessions would fail. "+
		"Check the executor's \"namespace\" and \"budgetNamespace\" settings and the Roles in "+
		"cmd/session-profile/README.md", who, need.verb, qualified(need.group, need.resource), need.namespace)
}

func qualified(group, resource string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}

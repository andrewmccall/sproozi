package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

// fakeAuthorizer answers access reviews from a set of "who verb resource ns" keys.

// gatewaySystem is the installed gateway's namespace.
const gatewaySystem = "sproozi-system"

func fakeAuthorizer(allowed map[string]bool) *fake.Clientset {
	clients := fake.NewClientset()
	answer := func(who string, a *authorizationv1.ResourceAttributes) bool {
		return allowed[who+" "+a.Verb+" "+a.Resource+" "+a.Namespace]
	}
	clients.PrependReactor("create", "selfsubjectaccessreviews", func(
		action clienttesting.Action,
	) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = answer("self", review.Spec.ResourceAttributes)
		return true, review, nil
	})
	clients.PrependReactor("create", "subjectaccessreviews", func(
		action clienttesting.Action,
	) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = answer(review.Spec.User, review.Spec.ResourceAttributes)
		return true, review, nil
	})
	return clients
}

func TestPreflightNamesTheMissingPermission(t *testing.T) {
	all := map[string]bool{
		"self create configmaps sproozi-sessions": true, "self update configmaps sproozi-sessions": true,
		"self create agentruns sproozi-sessions":   true,
		"self create budgetledgers sproozi-system": true, "self update budgetledgers sproozi-system": true,
		defaultGatewayUser + " get configmaps sproozi-sessions":     true,
		defaultGatewayUser + " update budgetledgers sproozi-system": true,
	}
	ctx := context.Background()
	if err := preflight(ctx, fakeAuthorizer(all), "sproozi-sessions", gatewaySystem, defaultGatewayUser); err != nil {
		t.Fatalf("preflight with every permission: %v", err)
	}
	// A run namespace where the gateway cannot read registrations.
	err := preflight(context.Background(), fakeAuthorizer(all), "other-runs", gatewaySystem, defaultGatewayUser)
	if err == nil || !strings.Contains(err.Error(), "namespace other-runs") {
		t.Fatalf("preflight = %v, want the missing namespace named", err)
	}
	// A budget namespace where the executor cannot initialize accounts.
	delete(all, "self create budgetledgers sproozi-system")
	err = preflight(context.Background(), fakeAuthorizer(all), "sproozi-sessions", gatewaySystem, defaultGatewayUser)
	if err == nil || !strings.Contains(err.Error(), "budgetNamespace") ||
		!strings.Contains(err.Error(), "create budgetledgers") {
		t.Fatalf("preflight = %v, want the budget permission named", err)
	}
}

func allPermissions(gatewayUser string) map[string]bool {
	return map[string]bool{
		"self create configmaps sproozi-sessions": true, "self update configmaps sproozi-sessions": true,
		"self create agentruns sproozi-sessions":   true,
		"self create budgetledgers sproozi-system": true, "self update budgetledgers sproozi-system": true,
		gatewayUser + " get configmaps sproozi-sessions":     true,
		gatewayUser + " update budgetledgers sproozi-system": true,
	}
}

// An executor whose budgetNamespace differs from the gateway's BUDGET_NAMESPACE
// initializes accounts the gateway never charges; it may still hold grants in
// both namespaces, so only the gateway's own access shows the mismatch.
func TestPreflightRefusesABudgetNamespaceTheGatewayDoesNotCharge(t *testing.T) {
	allowed := allPermissions(defaultGatewayUser)
	delete(allowed, defaultGatewayUser+" update budgetledgers sproozi-system")
	err := preflight(t.Context(), fakeAuthorizer(allowed), "sproozi-sessions", gatewaySystem, defaultGatewayUser)
	if err == nil || !strings.Contains(err.Error(), "budgetNamespace") ||
		!strings.Contains(err.Error(), "BUDGET_NAMESPACE") || !strings.Contains(err.Error(), "namespace sproozi-system") {
		t.Fatalf("preflight = %v, want the gateway's budget namespace mismatch named", err)
	}
}

// The gateway's namespace holds its configuration ConfigMaps, which only the
// controller may write (ADR 0041). The executor writes ConfigMaps in its run
// namespace, so that may not be the gateway's, whatever RBAC allows. Ledgers
// are BudgetLedger resources, so the budget namespace may be (ADR 0045).
func TestPreflightRefusesTheGatewayNamespace(t *testing.T) {
	const custom = "system:serviceaccount:platform:gateway"
	for _, tc := range []struct {
		name, run, budget, gateway, field string
	}{
		{"run namespace", gatewaySystem, gatewaySystem, defaultGatewayUser, fieldNamespace},
		{"custom gateway namespace", "platform", gatewaySystem, custom, fieldNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := allPermissions(tc.gateway)
			for _, ns := range []string{tc.run, tc.budget} {
				for _, key := range []string{"self create configmaps ", "self update configmaps ",
					"self create agentruns ", "self create budgetledgers ", "self update budgetledgers ",
					tc.gateway + " get configmaps ", tc.gateway + " update budgetledgers "} {
					allowed[key+ns] = true
				}
			}
			err := preflight(t.Context(), fakeAuthorizer(allowed), tc.run, tc.budget, tc.gateway)
			if err == nil || !strings.Contains(err.Error(), tc.field+" \"") ||
				!strings.Contains(err.Error(), "gateway's namespace") {
				t.Fatalf("preflight = %v, want %s refused as the gateway's namespace", err, tc.field)
			}
		})
	}
	if err := preflight(t.Context(), fakeAuthorizer(allPermissions(custom)), "sproozi-sessions", gatewaySystem,
		custom); err != nil {
		t.Fatalf("custom gateway user: %v", err)
	}
	for _, user := range []string{"sproozi-shared-gateway", "system:serviceaccount:sproozi-system", "alice",
		"system:serviceaccount:Bad_NS:gateway"} {
		err := preflight(t.Context(), fakeAuthorizer(allPermissions(user)), "sproozi-sessions", gatewaySystem, user)
		if err == nil || !strings.Contains(err.Error(), "gatewayUser") {
			t.Fatalf("gatewayUser %q: %v", user, err)
		}
	}
}

// An API server that never answers fails startup instead of hanging it.
func TestPreflightGivesUpOnAnUnresponsiveAPIServer(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	clients, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	previous := preflightTimeout
	preflightTimeout = 200 * time.Millisecond
	defer func() { preflightTimeout = previous }()
	done := make(chan error, 1)
	go func() {
		done <- preflight(context.Background(), clients, "sproozi-sessions", gatewaySystem, defaultGatewayUser)
	}()
	select {
	case err = <-done:
		if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("preflight = %v, want a deadline error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("preflight did not give up on an unresponsive API server")
	}
}

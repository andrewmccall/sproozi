package mcp_test

import (
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func managedDockerGrant(grant *gateway.ExecutionGrant) {
	grant.Execution.Backend = gateway.BackendDocker
	grant.Authority = gateway.Reference{}
	grant.Managed = true
	grant.Allocations = map[api.CapabilityKind]gateway.Allocation{
		docsCapability:      {AccountKey: "docker-visit/mcp.docs", Ceiling: api.EndpointBudget{MaxUnits: 2}},
		inventoryCapability: {AccountKey: "docker-visit/mcp.inventory", Ceiling: api.EndpointBudget{MaxUnits: 2}},
	}
}

func TestManagedMCPIntersectsCurrentPolicyWithInitializedAllocation(t *testing.T) {
	f := newFixture(t, func(grant *gateway.ExecutionGrant) {
		managedDockerGrant(grant)
		grant.Policy.Spec.Budgets = map[api.CapabilityKind]api.EndpointBudget{docsCapability: {MaxUnits: 1}}
	})
	if err := f.budgets.Initialize("docker-visit/mcp.docs"); err != nil {
		t.Fatal(err)
	}
	session := f.connect(t, docsName)
	for _, want := range []bool{true, false} {
		result, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: searchTool, Arguments: map[string]any{collectionField: approvedCollection, queryField: testQuery}})
		allowed := err == nil && !result.IsError
		if allowed != want {
			t.Fatalf("allowed=%v want=%v result=%v err=%v", allowed, want, result, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("forwarded %d calls", f.calls.Load())
	}
	state, err := f.budgets.State("docker-visit/mcp.docs")
	if err != nil || state.UnitsUsed != 1 {
		t.Fatalf("account: %+v %v", state, err)
	}
}

func TestManagedMCPDeniesBeforeUpstreamDiscovery(t *testing.T) {
	for _, name := range []string{"missing account", "missing allocation", "exhausted"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, func(grant *gateway.ExecutionGrant) {
				managedDockerGrant(grant)
				if name == "missing allocation" {
					delete(grant.Allocations, docsCapability)
				}
				if name == "exhausted" {
					a := grant.Allocations[docsCapability]
					a.Exhausted = true
					grant.Allocations[docsCapability] = a
				}
			})
			session, err := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: fixtureVersion}, nil).Connect(t.Context(), &sdk.StreamableClientTransport{Endpoint: "https://" + fixtureHost + ":8443/mcp/" + docsName, HTTPClient: f.client, DisableStandaloneSSE: true}, nil)
			if session != nil {
				_ = session.Close()
			}
			if err == nil || f.requests.Load() != 0 || f.calls.Load() != 0 {
				t.Fatalf("allocation contacted upstream: requests=%d calls=%d err=%v", f.requests.Load(), f.calls.Load(), err)
			}
		})
	}
}

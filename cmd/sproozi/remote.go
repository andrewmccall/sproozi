package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// This file is the one seam between the CLI and how a cluster is found.
// Remote commands name a cluster with --context (a kubeconfig context name or
// kubeconfig file) and --namespace. Until kubeconfig contexts are supported,
// a context name selects an executor configured under "remote" or
// "workSources" in a hand-written configuration.

// defaultContext is the kubeconfig context remote commands use by default.
const defaultContext = "sproozi"

// remoteTarget is one cluster's remote executor: the name the service knows
// it by, for pull and move, and how the CLI reaches it.
type remoteTarget struct {
	Name   string
	Client profile.ClientConfig
}

// resolveRemote finds the remote executor for a context and namespace.
func resolveRemote(_ context.Context, fixed home.Config, kubeContext, namespace string) (remoteTarget, error) {
	if kubeContext == "" {
		kubeContext = defaultContext
	}
	if namespace != "" {
		return remoteTarget{}, fmt.Errorf("--namespace needs kubeconfig context support, which this build does not " +
			"have yet; name a configured cluster with --context")
	}
	if strings.ContainsAny(kubeContext, `/\`) {
		return remoteTarget{}, fmt.Errorf("--context %s: kubeconfig files are not supported by this build yet; "+
			"name a configured cluster", kubeContext)
	}
	client, ok := fixed.WorkConnections()[kubeContext]
	if !ok {
		configured := slices.Sorted(maps.Keys(fixed.WorkConnections()))
		if len(configured) == 0 {
			return remoteTarget{}, fmt.Errorf("no cluster named %q is set up; see %s", kubeContext,
				docURL("docs/guides/portable-workflows.md"))
		}
		return remoteTarget{}, fmt.Errorf("no cluster named %q is set up; use --context with one of: %s",
			kubeContext, strings.Join(configured, ", "))
	}
	return remoteTarget{Name: kubeContext, Client: client}, nil
}

// remoteTargets are the clusters "sessions" lists when no --context is given.
func remoteTargets(_ context.Context, fixed home.Config) []remoteTarget {
	targets := make([]remoteTarget, 0, len(fixed.WorkConnections()))
	for _, name := range slices.Sorted(maps.Keys(fixed.WorkConnections())) {
		targets = append(targets, remoteTarget{Name: name, Client: fixed.WorkConnections()[name]})
	}
	return targets
}

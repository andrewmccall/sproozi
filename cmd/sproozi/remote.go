package main

import (
	"context"
	"time"

	"github.com/andrewmccall/sproozi/internal/session/remote"
	"github.com/andrewmccall/sproozi/internal/work"
)

// This file is the one seam between the CLI and how a cluster is found.
// Remote commands name a cluster with --context (a kubeconfig context name or
// kubeconfig file) and --namespace; without --context they use the context
// named "sproozi". See docs/reference/remote-executors.md.

// remoteTarget is one cluster's remote executor: its own name, which session
// rows show, and its remote work catalogue.
type remoteTarget struct {
	Name   string
	Client *work.Client
	// Err says why the cluster could not be reached; Name is then the context.
	Err error
}

// selectRemote resolves --context and --namespace on this machine, without
// contacting the cluster. The service reaches the selection itself for move
// and pull, so exec credential plugins run in its environment.
func selectRemote(kubeContext, namespace string) (remote.Selection, error) {
	return remote.Select(kubeContext, namespace)
}

// resolveRemote reaches the remote executor a context and namespace select,
// for the commands the CLI runs against a cluster directly.
func resolveRemote(ctx context.Context, kubeContext, namespace string) (remoteTarget, error) {
	selection, err := selectRemote(kubeContext, namespace)
	if err != nil {
		return remoteTarget{}, err
	}
	executor, err := remote.Discover(ctx, selection)
	if err != nil {
		return remoteTarget{}, err
	}
	client, err := remote.NewClient(executor)
	if err != nil {
		return remoteTarget{}, err
	}
	return remoteTarget{Name: executor.Name, Client: work.ClientFor(client)}, nil
}

// defaultTargets are the clusters "sessions" lists besides this machine: the
// one the sproozi context reaches, when that context exists.
func defaultTargets(ctx context.Context) []remoteTarget {
	if _, err := selectRemote("", ""); err != nil {
		return nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	target, err := resolveRemote(resolveCtx, "", "")
	if err != nil {
		return []remoteTarget{{Name: remote.DefaultContext, Err: err}}
	}
	return []remoteTarget{target}
}

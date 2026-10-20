package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// TestAuthorityCacheServesAuthorityObjectsAndReadsMovesLive starts the
// gateway's watch cache against a real API server. Authority objects are
// cached, registrations and Pods are read live, and no other type silently
// starts an informer.
func TestAuthorityCacheServesAuthorityObjectsAndReadsMovesLive(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		dirs, _ := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*"))
		if len(dirs) == 0 {
			t.Skip("envtest binaries are unavailable; run make setup-envtest")
		}
		t.Setenv("KUBEBUILDER_ASSETS", dirs[0])
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	live, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const agents, other = "sproozi-agents", "default"
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agents}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: agents}},
	} {
		if err := live.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	cached, err := startAuthorityCache(ctx, config, agents)
	if err != nil {
		t.Fatal(err)
	}
	// Each authority type has a synced informer: an absent object is NotFound.
	for _, object := range []client.Object{&sprooziv1alpha1.AgentRun{}, &sprooziv1alpha1.AgentTemplate{},
		&sprooziv1alpha1.AgentPolicy{}} {
		if err := cached.Get(ctx, client.ObjectKey{Namespace: other, Name: "absent"}, object); !apierrors.IsNotFound(err) {
			t.Fatalf("cached %T read = %v, want NotFound", object, err)
		}
	}
	if err := cached.Get(ctx, client.ObjectKey{Namespace: agents, Name: "worker"}, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("cached ServiceAccount read: %v", err)
	}
	// ServiceAccounts are cached only in the agent namespace.
	outside := client.ObjectKey{Namespace: other, Name: "default"}
	if err := cached.Get(ctx, outside, &corev1.ServiceAccount{}); err == nil {
		t.Fatal("ServiceAccount outside the agent namespace was cached")
	}
	registration := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "registration", Namespace: other}}
	if err := live.Create(ctx, registration); err != nil {
		t.Fatal(err)
	}
	// Read immediately after the write: only a live read is guaranteed to see it.
	if err := cached.Get(ctx, client.ObjectKeyFromObject(registration), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("registration read was not live: %v", err)
	}
	var missing *cache.ErrResourceNotCached
	err = cached.Get(ctx, client.ObjectKey{Namespace: other, Name: "any"}, &corev1.Secret{})
	if !errors.As(err, &missing) {
		t.Fatalf("uncached type read = %v, want ErrResourceNotCached", err)
	}
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package main is the entry point for the internal webhook binary.
//
// +kubebuilder:rbac:groups=sproozi.com,resources=agentruns,verbs=create
// +kubebuilder:rbac:groups=sproozi.com,resources=agentpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=sproozi.com,resources=agenttemplates,verbs=get;list;watch
//
// HMAC Secret reads are granted only in the HMAC Secret namespace, by the
// hand-written Role in config/webhook-secrets, never cluster-wide or in the
// namespace that holds provider and GitHub credentials.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/telemetry"
	"github.com/andrewmccall/sproozi/internal/webhook"
)

const defaultListenAddr = ":8083"

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sprooziv1alpha1.AddToScheme(scheme))
}

func main() {
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	listenAddr := envOrDefault("LISTEN_ADDR", defaultListenAddr)
	telemetry.ShutdownOnSignal(telemetry.Setup(context.Background(), "sproozi-webhook"))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "Failed to get kubeconfig")
		os.Exit(1)
	}

	runtimeClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "Failed to create controller-runtime client")
		os.Exit(1)
	}

	replay := webhook.NewK8sReplayStore(runtimeClient, envOrDefault("REPLAY_NAMESPACE", "sproozi-webhook-state"))
	// Each accepted event leaves a replay record. Sweep once per replay window
	// so a record outlives its window by at most one more.
	go func() {
		for range time.Tick(webhook.DefaultReplayWindow) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if err := replay.SweepExpired(ctx); err != nil {
				log.Error(err, "Failed to sweep expired webhook replay records")
			}
			cancel()
		}
	}()

	h := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:          runtimeClient,
			Replay:          replay,
			AuditLogger:     telemetry.AuditWriter{Next: audit.NewLogger(os.Stdout)},
			SecretNamespace: envOrDefault("HMAC_SECRET_NAMESPACE", webhook.DefaultSecretNamespace),
		},
	}

	log.Info("Starting webhook", "addr", listenAddr)

	if err := newServer(listenAddr, h).ListenAndServe(); err != nil {
		log.Error(err, "Server exited")
		os.Exit(1)
	}
}

// maxHeaderBytes bounds the request headers a sender can make the receiver
// hold. Signed event requests carry only a few short headers.
const maxHeaderBytes = 64 << 10

// newServer bounds every phase of a connection. The receiver is reachable
// before authentication, so a slow or idle sender must not hold a connection
// or a request body open indefinitely.
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       45 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

func envOrDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}

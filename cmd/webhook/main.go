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
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
package main

import (
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

	h := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      runtimeClient,
			Replay:      webhook.NewK8sReplayStore(runtimeClient, envOrDefault("REPLAY_NAMESPACE", "sproozi-webhook-state")),
			AuditLogger: audit.NewLogger(os.Stdout),
		},
	}

	log.Info("Starting webhook", "addr", listenAddr)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error(err, "Server exited")
		os.Exit(1)
	}
}

func envOrDefault(key, def string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return def
}

package docker_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/session"
	"k8s.io/client-go/rest"
)

const (
	modelHostname = "api.openai.com"
)

func TestNativeSemanticGatewaysRetainScopeCredentialsAndUnitAccounting(t *testing.T) {
	f := newOwner(t)
	if err := f.owner.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.Policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 5}
	f.launch.Allowances[modelCapability] = session.Allowance{Units: session.Limit{Bounded: true, Value: 5}}
	f.config.NativeCapabilities = []api.CapabilityKind{api.CapabilityKubernetesRead, api.CapabilityGitHubPullRequest}
	f.config.Policy.Spec.AllowedCapabilities = append(f.config.Policy.Spec.AllowedCapabilities, f.config.NativeCapabilities...)
	f.config.Policy.Spec.KubernetesRead = api.KubernetesReadScope{Namespaces: []string{"ops"}, Resources: []api.KubernetesReadResource{api.KubernetesReadPods, api.KubernetesReadPodLogs}}
	f.config.Policy.Spec.GitHubPullRequest = api.GitHubPullRequestScope{Repositories: []string{"approved/repo"}, AllowedBaseBranches: []string{"main"}}
	for _, cap := range f.config.NativeCapabilities {
		f.config.Policy.Spec.Budgets[cap] = api.EndpointBudget{MaxUnits: 2}
		f.launch.Allowances[string(cap)] = session.Allowance{Units: session.Limit{Bounded: true, Value: 2}}
	}
	var err error
	f.owner, err = gatewaydocker.Open(f.config)
	if err != nil {
		t.Fatal(err)
	}
	token := f.initialized(t)
	if err = f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	var kubeCalls, gitCalls atomic.Int32
	kube := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kubeCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer restricted-kube-provider" || r.Header.Get("Impersonate-User") != "" {
			t.Error("Kubernetes did not use gateway-owned credential")
		}
		_, _ = io.WriteString(w, `{"kind":"PodList","items":[{"metadata":{"name":"real-fixture-pod"}}]}`)
	}))
	defer kube.Close()
	kubeConfig := &rest.Config{Host: kube.URL, BearerToken: "restricted-kube-provider", TLSClientConfig: rest.TLSClientConfig{CAData: pemCertificate(kube.Certificate().Raw)}}
	kubeClient, err := rest.HTTPClientFor(kubeConfig)
	if err != nil {
		t.Fatal(err)
	}
	kubeURL, _ := url.Parse(kube.URL)
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gitCalls.Add(1)
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != "restricted-github-provider" || r.Header.Get("Cookie") != "" {
			t.Error("Git did not use gateway-owned credential")
		}
		_, _ = io.WriteString(w, "approved clone discovery")
	}))
	defer git.Close()
	inspection := inspectionCertificate(t)
	proxy, err := gatewaydocker.NewGateway(f.owner, gatewaydocker.GatewayConfig{
		Harness: harness.NativeCodex, MCPAuthority: controlAuthority, Certificates: map[string]tls.Certificate{controlHost: inspection, modelHostname: inspection, "github.com": inspection, "api.github.com": inspection, "kubernetes.default.svc": inspection},
		Model:      modelgateway.HandlerConfig{UpstreamURL: "https://api.openai.com", ProviderAuth: modelauth.APIKey{Key: trustedProviderKey}},
		GitHub:     &githubgateway.HandlerConfig{TokenProvider: &githubgateway.FakeTokenProvider{GitHubToken: "restricted-github-provider"}, HTTPClient: git.Client(), GitBaseURL: git.URL, APIBaseURL: git.URL},
		Kubernetes: &kubernetesgateway.HandlerConfig{Upstream: kubeURL, Client: kubeClient, MaxResponseBytes: 8 << 20}, AuditLogger: audit.NewLogger(io.Discard),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(proxy)
	defer server.Close()
	proxyURL, _ := url.Parse(server.URL)
	proxyURL.User = url.UserPassword("sproozi", token)
	certificate, _ := x509.ParseCertificate(inspection.Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	roots.AddCert(certificate)
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, probe := range []struct {
		method, endpoint string
		status           int
		text             string
	}{
		{http.MethodGet, "https://kubernetes.default.svc/api/v1/namespaces/other/pods", 403, ""},
		{http.MethodGet, "https://kubernetes.default.svc/api/v1/namespaces/ops/secrets", 403, ""},
		{"PATCH", "https://kubernetes.default.svc/api/v1/namespaces/ops/pods/a", 403, ""},
		{http.MethodGet, "https://kubernetes.default.svc/api/v1/namespaces/ops/pods", 200, "real-fixture-pod"},
		{http.MethodGet, "https://kubernetes.default.svc/api/v1/namespaces/ops/pods/a/log", 200, "real-fixture-pod"},
		{http.MethodGet, "https://kubernetes.default.svc/api/v1/namespaces/ops/pods", 403, ""},
		{http.MethodGet, "https://github.com/unapproved/repo.git/info/refs?service=git-upload-pack", 403, ""},
		{http.MethodGet, "https://api.github.com/repos/approved/repo/issues", 403, ""},
		{http.MethodGet, "https://github.com/approved/repo.git/info/refs?service=git-upload-pack", 200, "approved clone discovery"},
	} {
		r, _ := http.NewRequest(probe.method, probe.endpoint, nil)
		r.Header.Set("Authorization", "Bearer forged-workload-key")
		r.Header.Set("Impersonate-User", "cluster-admin")
		r.Header.Set("Cookie", "worker-secret=do-not-forward")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != probe.status || !strings.Contains(string(body), probe.text) {
			t.Fatalf("%s %s got %d %q want %d %q", probe.method, probe.endpoint, response.StatusCode, body, probe.status, probe.text)
		}
	}
	if kubeCalls.Load() != 2 || gitCalls.Load() != 1 {
		t.Fatalf("denied operation reached providers: kube=%d git=%d", kubeCalls.Load(), gitCalls.Load())
	}
	if err := f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	usage, err := f.owner.Seal(t.Context(), f.launch, f.instance)
	if err != nil || usage["kubernetes.read"].Units != 2 || usage["github.pull_request"].Units != 1 {
		t.Fatalf("native accounting=%+v err=%v", usage, err)
	}
}

func TestNativeAllocationRequiresConfiguredHandlerAndRejectsMonetaryCeilings(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfigured", true: "no-native-pricing"}[configured], func(t *testing.T) {
			f := newOwner(t)
			_ = f.owner.Close()
			capability := api.CapabilityKubernetesRead
			f.config.Policy.Spec.AllowedCapabilities = append(f.config.Policy.Spec.AllowedCapabilities, capability)
			f.config.Policy.Spec.Budgets[capability] = api.EndpointBudget{MaxUnits: 2}
			f.launch.Allowances[string(capability)] = session.Allowance{Units: session.Limit{Bounded: true, Value: 1}}
			if configured {
				f.config.NativeCapabilities = []api.CapabilityKind{capability}
				f.launch.Allowances[string(capability)] = session.Allowance{Units: session.Limit{Bounded: true, Value: 1}, CostMicros: session.Limit{Bounded: true, Value: 1}}
			}
			var err error
			f.owner, err = gatewaydocker.Open(f.config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.owner.Initialize(t.Context(), f.launch, f.instance); err == nil {
				t.Fatal("native allocation admitted without configured authority or trusted pricing")
			}
		})
	}
}

func pemCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

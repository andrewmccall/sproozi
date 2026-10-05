// Command gateway is the single workload-facing authenticated proxy listener.
// It composes every semantic capability behind one inspected transport.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"

	"github.com/andrewmccall/sproozi/internal/audit"
	egress "github.com/andrewmccall/sproozi/internal/endpoints/destination"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	packagesgateway "github.com/andrewmccall/sproozi/internal/endpoints/packages"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	authentication "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultHost = "sproozi-gateway.sproozi-system.svc"

var scheme = runtime.NewScheme()

type startupConfig struct {
	enabled        map[sprooziv1alpha1.CapabilityKind]bool
	host           string
	listenerCert   tls.Certificate
	certificates   map[string]tls.Certificate
	profiles       *egress.ProfileStore
	pricing        *modelgateway.PricingTable
	openAIKey      string
	modelAuthMode  string
	appID          int64
	installationID int64
	privateKey     []byte
}

// semanticAuthorities binds concrete service modules to exact authorities.
var semanticAuthorities = map[string]sprooziv1alpha1.CapabilityKind{
	kubernetesgateway.Authority:  sprooziv1alpha1.CapabilityKubernetesRead,
	"api.openai.com:443":         sprooziv1alpha1.CapabilityModelInference,
	"github.com:443":             sprooziv1alpha1.CapabilityGitHubPullRequest,
	"api.github.com:443":         sprooziv1alpha1.CapabilityGitHubPullRequest,
	"pypi.org:443":               sprooziv1alpha1.CapabilityPackagesInstall,
	"files.pythonhosted.org:443": sprooziv1alpha1.CapabilityPackagesInstall,
}

func loadStartupConfig() (*startupConfig, error) {
	enabled, err := enabledCapabilities()
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(envOrDefault("SPROOZI_GATEWAY_HOST", defaultHost))
	certFile, keyFile := os.Getenv("SPROOZI_GATEWAY_CERT_FILE"), os.Getenv("SPROOZI_GATEWAY_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return nil, errors.New("gateway TLS certificate and key are required")
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load gateway TLS certificate: %w", err)
	}
	profileData := []byte("{}")
	if enabled[sprooziv1alpha1.CapabilityNetworkEgress] {
		profileData, err = os.ReadFile(envOrDefault("EGRESS_PROFILES_PATH", "/etc/sproozi/egress/profiles.json"))
		if err != nil {
			return nil, fmt.Errorf("read egress profiles: %w", err)
		}
	}
	profiles, err := egress.LoadProfileStore(profileData)
	if err != nil {
		return nil, err
	}
	var pricing *modelgateway.PricingTable
	openAIKey, modelAuthMode := "", ""
	if enabled[sprooziv1alpha1.CapabilityModelInference] {
		pricingFile, err := os.Open(envOrDefault("MODEL_PRICING_PATH", "/etc/sproozi/model/pricing.json"))
		if err != nil {
			return nil, fmt.Errorf("read model pricing: %w", err)
		}
		var pricingErr error
		pricing, pricingErr = modelgateway.LoadPricingTable(pricingFile)
		closeErr := pricingFile.Close()
		if pricingErr != nil {
			return nil, pricingErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close model pricing: %w", closeErr)
		}
		openAIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		modelAuthMode = envOrDefault("MODEL_AUTH_MODE", modelauth.APIKeyMode)
		if modelAuthMode != modelauth.APIKeyMode && modelAuthMode != modelauth.ChatGPTMode {
			return nil, errors.New("MODEL_AUTH_MODE must be api_key or chatgpt")
		}
		if modelAuthMode == modelauth.ChatGPTMode &&
			envOrDefault("OPENAI_UPSTREAM_URL", "https://api.openai.com") != "https://api.openai.com" {
			return nil, errors.New("ChatGPT plan usage requires the public OpenAI API upstream")
		}
		if modelAuthMode == modelauth.APIKeyMode && openAIKey == "" {
			return nil, errors.New("OPENAI_API_KEY is required")
		}
	}
	var appID, installationID int64
	var privateKey []byte
	if enabled[sprooziv1alpha1.CapabilityGitHubPullRequest] {
		appID, err = positiveEnvInteger("GITHUB_APP_ID")
		if err != nil {
			return nil, err
		}
		installationID, err = positiveEnvInteger("GITHUB_APP_INSTALLATION_ID")
		if err != nil {
			return nil, err
		}
		privateKey, err = os.ReadFile(requiredEnvValue("GITHUB_APP_PRIVATE_KEY_PATH"))
		if err != nil {
			return nil, fmt.Errorf("read GitHub App private key: %w", err)
		}
		if _, err := githubgateway.NewAppTokenProvider(appID, installationID, privateKey, http.DefaultClient); err != nil {
			return nil, fmt.Errorf("invalid GitHub App private key: %w", err)
		}
	}
	certificates := map[string]tls.Certificate{}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{host: true}
	for authority, cap := range semanticAuthorities {
		if enabled[cap] {
			name, _, _ := net.SplitHostPort(authority)
			hosts[name] = true
		}
	}
	for _, destination := range profiles.Destinations() {
		hosts[destination.Host] = true
	}
	for name := range hosts {
		if err := leaf.VerifyHostname(name); err != nil {
			return nil, fmt.Errorf("gateway certificate does not cover configured host %q", name)
		}
		certificates[name] = certificate
	}
	return &startupConfig{
		enabled: enabled,
		host:    host, listenerCert: certificate, certificates: certificates,
		profiles: profiles, pricing: pricing, openAIKey: openAIKey, modelAuthMode: modelAuthMode,
		appID: appID, installationID: installationID, privateKey: privateKey,
	}, nil
}

func enabledCapabilities() (map[sprooziv1alpha1.CapabilityKind]bool, error) {
	supported := []sprooziv1alpha1.CapabilityKind{
		sprooziv1alpha1.CapabilityKubernetesRead,
		sprooziv1alpha1.CapabilityModelInference,
		sprooziv1alpha1.CapabilityGitHubPullRequest,
		sprooziv1alpha1.CapabilityPackagesInstall,
		sprooziv1alpha1.CapabilityNetworkEgress,
	}
	enabled := map[sprooziv1alpha1.CapabilityKind]bool{}
	for item := range strings.SplitSeq(envOrDefault("SPROOZI_ENABLED_CAPABILITIES", "kubernetes.read"), ",") {
		cap := sprooziv1alpha1.CapabilityKind(strings.TrimSpace(item))
		if !slices.Contains(supported, cap) {
			return nil, fmt.Errorf("unknown enabled capability %q", cap)
		}
		enabled[cap] = true
	}
	return enabled, nil
}

func positiveEnvInteger(key string) (int64, error) {
	value, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func requiredEnvValue(key string) string { return os.Getenv(key) }

func composeDispatcher(
	cfg *startupConfig, handlers map[sprooziv1alpha1.CapabilityKind]http.Handler,
) gateway.Dispatcher {
	routes := gateway.Dispatcher{}
	for authority, cap := range semanticAuthorities {
		// Reserve semantic authorities even when a module is disabled. Generic
		// egress must never become a weaker fallback for a semantic service.
		routes[authority] = gateway.Route{Capability: string(cap), Tier: audit.TierSemantic}
		if cfg.enabled[cap] {
			route := routes[authority]
			route.Handler = handlers[cap]
			routes[authority] = route
		}
	}
	if cfg.enabled[sprooziv1alpha1.CapabilityNetworkEgress] {
		for _, destination := range cfg.profiles.Destinations() {
			authority := net.JoinHostPort(destination.Host, strconv.Itoa(destination.Port))
			if _, semantic := routes[authority]; semantic {
				continue
			}
			routes[authority] = gateway.Route{Capability: string(sprooziv1alpha1.CapabilityNetworkEgress),
				Tier:    audit.TierDestination,
				Handler: handlers[sprooziv1alpha1.CapabilityNetworkEgress]}
		}
	}
	return routes
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sprooziv1alpha1.AddToScheme(scheme))
}

func main() {
	cfg, err := loadStartupConfig()
	if err != nil {
		log.Fatal(err)
	}
	host := cfg.host

	config, err := ctrl.GetConfig()
	if err != nil {
		log.Fatal(err)
	}
	clientset, err := authentication.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	runtimeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatal(err)
	}
	agentNamespace := envOrDefault("AGENT_NAMESPACE", "sproozi-agents")
	validator := gateway.NewK8sTokenValidator(clientset, "sproozi-gateway")
	runFinder := gateway.NewK8sRunFinder(runtimeClient, agentNamespace)
	authenticator := &gateway.Authenticator{Tokens: validator, Runs: runFinder}
	authenticate := func(ctx context.Context, token string) (*gateway.RunIdentity, error) {
		return authenticator.Authenticate(ctx, token)
	}
	revalidate := func(ctx context.Context, identity *gateway.RunIdentity) error {
		fresh, findErr := runFinder.Find(ctx, gateway.ServiceAccountIdentity{
			Namespace: identity.SANamespace, Name: identity.SAName, UID: identity.SAUID,
		})
		if findErr != nil || fresh == nil || fresh.Run == nil || fresh.Policy == nil {
			return gateway.ErrRunNotRunning
		}
		if string(fresh.Run.UID) != string(identity.Run.UID) ||
			string(fresh.Policy.UID) != string(identity.Policy.UID) ||
			fresh.Policy.Generation != identity.Policy.Generation {
			return gateway.ErrRunNotRunning
		}
		return nil
	}
	httpClient := &http.Client{Timeout: 120 * time.Second}
	auditLogger := newAuditLogger(os.Stdout)
	// The shared gateway is the only component that receives Kubernetes API
	// credentials. Build the upstream transport from its own in-cluster
	// rest.Config so workload ServiceAccount tokens can never be forwarded to
	// Kubernetes and no workload-specific RBAC is required.
	kubeConfig := rest.CopyConfig(config)
	kubeConfig.Timeout = 120 * time.Second
	kubeClient, err := rest.HTTPClientFor(kubeConfig)
	if err != nil {
		log.Fatal(err)
	}
	kubeURL, err := url.Parse(kubeConfig.Host)
	if err != nil || kubeURL.Scheme == "" || kubeURL.Host == "" {
		log.Fatal("invalid in-cluster Kubernetes API host")
	}
	kube := kubernetesgateway.NewHandler(kubernetesgateway.HandlerConfig{Upstream: kubeURL,
		Client:           kubeClient,
		MaxResponseBytes: 8 << 20,
		AuditLogger:      auditLogger})
	handlers := map[sprooziv1alpha1.CapabilityKind]http.Handler{sprooziv1alpha1.CapabilityKubernetesRead: kube}
	if cfg.enabled[sprooziv1alpha1.CapabilityModelInference] {
		budgetNamespace := envOrDefault("BUDGET_NAMESPACE", "sproozi-system")
		var providerAuth modelauth.Authenticator = modelauth.APIKey{Key: cfg.openAIKey}
		if cfg.modelAuthMode == modelauth.ChatGPTMode {
			store := modelauth.KubernetesSessionStore{Client: runtimeClient, Namespace: budgetNamespace}
			session, loadErr := store.Load(context.Background())
			if loadErr != nil {
				log.Fatal("ChatGPT credential Secret is unavailable")
			}
			if err = session.Validate(); err != nil {
				log.Fatal(err)
			}
			oauthClient := &http.Client{
				Timeout:       30 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}
			providerAuth = modelauth.NewChatGPT(store, oauthClient)
		}
		model := &modelgateway.Handler{Config: modelgateway.HandlerConfig{
			Pricing: cfg.pricing, AuditLogger: auditLogger,
			UpstreamURL:  envOrDefault("OPENAI_UPSTREAM_URL", "https://api.openai.com"),
			ProviderAuth: providerAuth, HTTPClient: httpClient,
		}}
		if os.Getenv("MODEL_DIAGNOSTICS") == "true" {
			// Administrator opt-in: bounded provider errors only, separate from audit.
			// Never log prompts, model text, reasoning, raw bodies or auth headers.
			model.Config.Diagnostics = func(d modelgateway.ResponseDiagnostic) {
				_ = json.NewEncoder(os.Stderr).Encode(d)
			}
		}
		handlers[sprooziv1alpha1.CapabilityModelInference] = model
	}
	if cfg.enabled[sprooziv1alpha1.CapabilityGitHubPullRequest] {
		tokenProvider, err := githubgateway.NewAppTokenProvider(cfg.appID, cfg.installationID, cfg.privateKey, httpClient)
		if err != nil {
			log.Fatal(err)
		}
		provider := &githubgateway.Handler{Config: githubgateway.HandlerConfig{TokenProvider: tokenProvider,
			AuditLogger: auditLogger,
			GitBaseURL:  envOrDefault("GIT_BASE_URL", "https://github.com"),
			APIBaseURL:  envOrDefault("API_BASE_URL", "https://api.github.com"),
			HTTPClient:  httpClient}}
		handlers[sprooziv1alpha1.CapabilityGitHubPullRequest] = provider
	}
	handlers[sprooziv1alpha1.CapabilityPackagesInstall] = &packagesgateway.Handler{Config: packagesgateway.HandlerConfig{
		HTTPClient:  httpClient,
		AuditLogger: auditLogger}}
	raw := &egress.Handler{Config: egress.HandlerConfig{Profiles: cfg.profiles,
		AuditLogger: auditLogger,
		HTTPClient:  httpClient}}

	handlers[sprooziv1alpha1.CapabilityNetworkEgress] = raw

	// Unknown authorities are absent and are denied before an upstream call.
	dispatcher := composeDispatcher(cfg, handlers)
	budgets := budget.NewTrackerWithLedger(budget.NewKubernetesLedger(runtimeClient,
		envOrDefault("BUDGET_NAMESPACE", "sproozi-system")))
	for authority, route := range dispatcher {
		route.Budgets = budgets
		dispatcher[authority] = route
	}
	transport, err := proxytransport.New(proxytransport.Config{
		Certificates:     cfg.certificates,
		Authenticate:     authenticate,
		Revalidate:       revalidate,
		Handler:          dispatcher,
		AllowedAuthority: dispatcher.Allows,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("shared gateway listening on :8443 for %s", host)
	log.Fatal(newProxyServer(cfg.listenerCert, transport).ListenAndServeTLS("", ""))
}

func newAuditLogger(output io.Writer) audit.Writer {
	return audit.NewLogger(output)
}

func newProxyServer(certificate tls.Certificate, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":8443",
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// CONNECT inspection takes ownership of the HTTP/1.1 connection after
		// authentication. Disable HTTP/2 so every accepted proxy request has the
		// same hijackable transport semantics across clients.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{certificate},
		},
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

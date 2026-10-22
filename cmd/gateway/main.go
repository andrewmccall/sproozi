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
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	authentication "k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetkubernetes "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
	"github.com/andrewmccall/sproozi/internal/diagnose"
	"github.com/andrewmccall/sproozi/internal/endpoints"
	"github.com/andrewmccall/sproozi/internal/endpoints/destination"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	packagesgateway "github.com/andrewmccall/sproozi/internal/endpoints/packages"
	"github.com/andrewmccall/sproozi/internal/gateway"
	gatewaykubernetes "github.com/andrewmccall/sproozi/internal/gateway/kubernetes"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	modelauthkube "github.com/andrewmccall/sproozi/internal/modelauth/kubernetes"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

const defaultHost = "sproozi-gateway.sproozi-system.svc"

// Startup errors name the installed object that supplies each value so an
// administrator knows what to fix. ConfigMaps shipped by config/default carry
// its sproozi- name prefix; Secrets and ConfigMaps the administrator creates
// keep the names config/manager/gateway.yaml references. test/manifests checks
// every name here against the rendered install.
const (
	modelCredentialsSource = "Secret sproozi-system/model-gateway-credentials"
	modelAuthSource        = "ConfigMap sproozi-system/model-gateway-auth"
	githubAppSource        = "Secret sproozi-system/capability-proxy-github-app"
	gatewayTLSSource       = "Secret sproozi-system/sproozi-gateway-tls"
	chatGPTSessionSource   = "Secret sproozi-system/model-gateway-chatgpt"
	egressProfilesSource   = "ConfigMap sproozi-system/sproozi-egress-gateway-profiles"
	modelPricingSource     = "ConfigMap sproozi-system/sproozi-model-gateway-pricing"
	mcpServersSource       = "ConfigMap sproozi-system/sproozi-mcp-servers"
	openAIKeyName          = "openai-api-key"
	anthropicKeyName       = "anthropic-api-key"
)

// healthAddress serves only the readiness probe. It opens after startup
// validation and the TLS listener succeed, answers a fixed body, and the
// default-deny NetworkPolicy keeps it unreachable from other Pods.
const healthAddress = ":8081"

var scheme = runtime.NewScheme()

type startupConfig struct {
	enabled              map[sprooziv1alpha1.CapabilityKind]bool
	host                 string
	listenerCert         tls.Certificate
	certificates         map[string]tls.Certificate
	certFile, keyFile    string
	profiles             *destination.ProfileStore
	pricing              *modelgateway.PricingTable
	openAIKey            string
	anthropicKey         string
	openAIUpstreamURL    string
	anthropicUpstreamURL string
	modelAuthMode        string
	appID                int64
	installationID       int64
	privateKey           []byte
	mcpRegistry          *mcpgateway.Registry
	admission            connectionLimits
	maxModelCall         time.Duration
	ledgerRetention      time.Duration
}

// connectionLimits are the administrator-tunable gateway connection limits. They
// must fit the gateway's memory limit; see proxytransport.DefaultMaxConnections.
type connectionLimits struct {
	maxConnections          int
	maxSessionsPerExecution int
}

const (
	maxConnectionsEnv          = "SPROOZI_GATEWAY_MAX_CONNECTIONS"
	maxSessionsPerExecutionEnv = "SPROOZI_GATEWAY_MAX_SESSIONS_PER_EXECUTION"
)

func loadConnectionLimits() (connectionLimits, error) {
	connections, err := boundedInteger(maxConnectionsEnv,
		proxytransport.DefaultMaxConnections, proxytransport.MaxConnectionsLimit)
	if err != nil {
		return connectionLimits{}, err
	}
	sessions, err := boundedInteger(maxSessionsPerExecutionEnv,
		min(proxytransport.DefaultMaxSessionsPerExecution, connections), proxytransport.MaxSessionsPerExecutionLimit)
	if err != nil {
		return connectionLimits{}, err
	}
	if sessions > connections {
		return connectionLimits{}, fmt.Errorf("%s (%d) must not exceed %s (%d): "+
			"check the gateway Deployment environment", maxSessionsPerExecutionEnv, sessions, maxConnectionsEnv, connections)
	}
	return connectionLimits{maxConnections: connections, maxSessionsPerExecution: sessions}, nil
}

// maxModelCallEnv sets the longest one model call may stream, in seconds. A
// policy's maxModelCallSeconds can only lower it.
const maxModelCallEnv = "SPROOZI_GATEWAY_MAX_MODEL_CALL_SECONDS"

// loadGatewayLimits reads the connection limits and the model call maximum.
func loadGatewayLimits() (connectionLimits, time.Duration, error) {
	admission, err := loadConnectionLimits()
	if err != nil {
		return connectionLimits{}, 0, err
	}
	seconds, err := boundedInteger(maxModelCallEnv, int(proxytransport.DefaultMaxModelCallDuration/time.Second),
		int(proxytransport.MaxModelCallDurationLimit/time.Second))
	return admission, time.Duration(seconds) * time.Second, err
}

// memoryLimitEnv carries the container memory limit in bytes, from a
// resourceFieldRef on limits.memory. The gateway sets its Go memory limit to
// 90% of it, so the garbage collector works harder before the kernel would
// kill the process. An explicit GOMEMLIMIT takes precedence, but the variable
// is still validated so a broken Deployment does not go unnoticed.
const memoryLimitEnv = "SPROOZI_GATEWAY_MEMORY_LIMIT"

// minContainerMemoryLimit is the smallest container memory limit the gateway
// accepts. Below it a 90% Go memory limit would leave the garbage collector
// running continuously, and a mistyped value (bytes vs Mi) is the likely cause.
const minContainerMemoryLimit = 64 << 20

// memoryLimit is the Go memory limit the gateway runs with and where it came from.
type memoryLimit struct {
	// bytes is the limit to set; zero keeps the runtime's own setting.
	bytes  int64
	source string
}

// goMemoryLimit validates memoryLimitEnv and chooses the Go memory limit.
func goMemoryLimit() (memoryLimit, error) {
	raw := strings.TrimSpace(os.Getenv(memoryLimitEnv))
	var container int64
	if raw != "" {
		var err error
		container, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || container < minContainerMemoryLimit {
			return memoryLimit{}, fmt.Errorf("%s=%q must be the container memory limit in bytes, at least %d (64Mi): "+
				"check limits.memory on the gateway Deployment", memoryLimitEnv, raw, minContainerMemoryLimit)
		}
	}
	if explicit := os.Getenv("GOMEMLIMIT"); explicit != "" {
		return memoryLimit{source: "GOMEMLIMIT=" + explicit}, nil
	}
	if raw == "" {
		return memoryLimit{source: "the Go runtime default, as neither GOMEMLIMIT nor " + memoryLimitEnv + " is set"}, nil
	}
	return memoryLimit{bytes: container / 10 * 9, source: fmt.Sprintf("90%% of %s=%d", memoryLimitEnv, container)}, nil
}

func boundedInteger(key string, fallback, upper int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > upper {
		return 0, fmt.Errorf("%s=%q must be an integer from 1 to %d: check the gateway Deployment environment",
			key, raw, upper)
	}
	return value, nil
}

// loadGitHubApp reads and checks the GitHub App identity the pull request
// module signs in with.
func loadGitHubApp() (appID, installationID int64, privateKey []byte, err error) {
	appID, err = githubAppInteger("GITHUB_APP_ID", "app-id")
	if err != nil {
		return 0, 0, nil, err
	}
	installationID, err = githubAppInteger("GITHUB_APP_INSTALLATION_ID", "installation-id")
	if err != nil {
		return 0, 0, nil, err
	}
	privateKey, err = os.ReadFile(os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil, fmt.Errorf("GitHub App private key is missing: create key private-key.pem in %s",
			githubAppSource)
	}
	if err != nil {
		return 0, 0, nil, fmt.Errorf("read GitHub App private key (key private-key.pem in %s): %w", githubAppSource, err)
	}
	validation := newUpstreamClient(time.Minute)
	if _, err := githubgateway.NewAppTokenProvider(appID, installationID, privateKey, validation); err != nil {
		return 0, 0, nil, fmt.Errorf("invalid GitHub App private key (key private-key.pem in %s): %w",
			githubAppSource, err)
	}
	return appID, installationID, privateKey, nil
}

func loadStartupConfig() (*startupConfig, error) {
	enabled, err := enabledCapabilities()
	if err != nil {
		return nil, err
	}
	admission, maxModelCall, err := loadGatewayLimits()
	if err != nil {
		return nil, err
	}
	ledgerRetention, err := loadLedgerRetention()
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(envOrDefault("SPROOZI_GATEWAY_HOST", defaultHost))
	certFile, keyFile := os.Getenv("SPROOZI_GATEWAY_CERT_FILE"), os.Getenv("SPROOZI_GATEWAY_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return nil, errors.New("gateway TLS certificate and key are required: " +
			"set SPROOZI_GATEWAY_CERT_FILE and SPROOZI_GATEWAY_KEY_FILE to tls.crt and tls.key " +
			"from " + gatewayTLSSource)
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load gateway TLS certificate (keys tls.crt and tls.key in %s): %w", gatewayTLSSource, err)
	}
	profileData := []byte("{}")
	if enabled[sprooziv1alpha1.CapabilityNetworkEgress] {
		profileData, err = os.ReadFile(envOrDefault("EGRESS_PROFILES_PATH", "/etc/sproozi/egress/profiles.json"))
		if err != nil {
			return nil, fmt.Errorf("read egress profiles (key profiles.json in %s): %w", egressProfilesSource, err)
		}
	}
	profiles, err := destination.LoadProfileStore(profileData)
	if err != nil {
		return nil, err
	}
	mcpRegistry, err := loadMCPRegistry(host)
	if err != nil {
		return nil, err
	}
	var pricing *modelgateway.PricingTable
	openAIKey, anthropicKey, modelAuthMode := "", "", ""
	openAIUpstreamURL := envOrDefault("OPENAI_UPSTREAM_URL", "https://api.openai.com")
	anthropicUpstreamURL := envOrDefault("ANTHROPIC_UPSTREAM_URL", "https://api.anthropic.com")
	if enabled[sprooziv1alpha1.CapabilityModelInference] {
		pricingFile, err := os.Open(envOrDefault("MODEL_PRICING_PATH", "/etc/sproozi/model/pricing.json"))
		if err != nil {
			return nil, fmt.Errorf("read model pricing (key pricing.json in %s): %w", modelPricingSource, err)
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
		openAIKey, anthropicKey, modelAuthMode, err = loadModelCredentials(openAIUpstreamURL)
		if err != nil {
			return nil, err
		}
		if err := validateModelUpstream("OPENAI_UPSTREAM_URL", openAIUpstreamURL); err != nil {
			return nil, err
		}
		if err := validateModelUpstream("ANTHROPIC_UPSTREAM_URL", anthropicUpstreamURL); err != nil {
			return nil, err
		}
	}
	var appID, installationID int64
	var privateKey []byte
	if enabled[sprooziv1alpha1.CapabilityGitHubPullRequest] {
		appID, installationID, privateKey, err = loadGitHubApp()
		if err != nil {
			return nil, err
		}
	}
	certificates := map[string]tls.Certificate{}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, err
	}
	hosts := inspectionHosts(host, enabled, profiles, mcpRegistry)
	if enabled[sprooziv1alpha1.CapabilityModelInference] {
		if anthropicKey == "" && modelAuthMode != modelauth.WorkloadMode {
			delete(hosts, "api.anthropic.com")
		}
		if openAIKey == "" && modelAuthMode == modelauth.APIKeyMode {
			delete(hosts, "api.openai.com")
		}
	}
	for name := range hosts {
		if err := leaf.VerifyHostname(name); err != nil {
			return nil, fmt.Errorf("gateway certificate in %s does not cover configured host %q: "+
				"reissue the certificate with %q in its subjectAltName", gatewayTLSSource, name, name)
		}
		certificates[name] = certificate
	}
	return &startupConfig{
		enabled: enabled,
		host:    host, listenerCert: certificate, certificates: certificates, certFile: certFile, keyFile: keyFile,
		profiles: profiles, pricing: pricing, openAIKey: openAIKey, anthropicKey: anthropicKey, modelAuthMode: modelAuthMode,
		openAIUpstreamURL: openAIUpstreamURL, anthropicUpstreamURL: anthropicUpstreamURL,
		appID: appID, installationID: installationID, privateKey: privateKey,
		mcpRegistry: mcpRegistry, admission: admission, maxModelCall: maxModelCall,
		ledgerRetention: ledgerRetention,
	}, nil
}

func loadModelCredentials(openAIUpstreamURL string) (string, string, string, error) {
	openAIKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	anthropicKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	for _, credential := range []struct{ env, key, value string }{
		{"OPENAI_API_KEY", openAIKeyName, openAIKey},
		{"ANTHROPIC_API_KEY", anthropicKeyName, anthropicKey},
	} {
		if strings.ContainsAny(credential.value, "\r\n") {
			return "", "", "", errors.New(credential.env + " is invalid: key " + credential.key + " in " +
				modelCredentialsSource + " must be a single line")
		}
	}
	mode := envOrDefault("MODEL_AUTH_MODE", modelauth.APIKeyMode)
	if mode != modelauth.APIKeyMode && mode != modelauth.ChatGPTMode && mode != modelauth.WorkloadMode {
		return "", "", "", fmt.Errorf("MODEL_AUTH_MODE=%q must be api_key, chatgpt or workload: check key mode in %s",
			mode, modelAuthSource)
	}
	if mode == modelauth.WorkloadMode && (openAIKey != "" || anthropicKey != "") {
		return "", "", "", errors.New("MODEL_AUTH_MODE=workload forwards each harness's own sign-in: " +
			"remove keys openai-api-key and anthropic-api-key from " + modelCredentialsSource)
	}
	if mode == modelauth.ChatGPTMode && openAIUpstreamURL != "https://api.openai.com" {
		return "", "", "", errors.New("ChatGPT plan usage requires the public OpenAI API upstream")
	}
	if mode == modelauth.APIKeyMode && openAIKey == "" && anthropicKey == "" {
		return "", "", "", errors.New("OPENAI_API_KEY or ANTHROPIC_API_KEY is required in api_key mode: " +
			"set key openai-api-key or anthropic-api-key in " + modelCredentialsSource +
			", or set key mode to chatgpt in " + modelAuthSource)
	}
	return openAIKey, anthropicKey, mode, nil
}

func inspectionHosts(host string, enabled map[sprooziv1alpha1.CapabilityKind]bool,
	profiles *destination.ProfileStore, registry *mcpgateway.Registry,
) map[string]bool {
	hosts := map[string]bool{host: true}
	for authority, cap := range endpoints.SemanticAuthorities {
		if enabled[cap] {
			name, _, _ := net.SplitHostPort(authority)
			hosts[name] = true
		}
	}
	for _, dest := range profiles.Destinations() {
		if slices.Contains(registry.Authorities(), net.JoinHostPort(dest.Host, strconv.Itoa(dest.Port))) {
			continue // Remote MCP providers are not inspected workload routes.
		}
		hosts[dest.Host] = true
	}
	return hosts
}

func loadMCPRegistry(gatewayHost string) (*mcpgateway.Registry, error) {
	data := []byte(`{"servers":{}}`)
	if registryPath := os.Getenv("MCP_SERVERS_PATH"); registryPath != "" {
		var err error
		data, err = os.ReadFile(registryPath)
		if err != nil {
			return nil, errors.New("MCP registry is unavailable: check key servers.json in " + mcpServersSource)
		}
	}
	registry, err := mcpgateway.LoadRegistry(data, mcpgateway.CredentialDirectory)
	if err != nil {
		return nil, err
	}
	for _, authority := range registry.Authorities() {
		name, _, _ := net.SplitHostPort(authority)
		if name == gatewayHost {
			return nil, errors.New("MCP upstream cannot use the gateway host")
		}
	}
	return registry, nil
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
			return nil, fmt.Errorf("SPROOZI_ENABLED_CAPABILITIES lists unknown capability %q", cap)
		}
		enabled[cap] = true
	}
	return enabled, nil
}

func githubAppInteger(key, secretKey string) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		// The Deployment references the Secret as optional, so a missing Secret
		// or key arrives as an empty variable.
		return 0, fmt.Errorf("%s is missing: create key %s in %s, or remove github.pull_request "+
			"from SPROOZI_ENABLED_CAPABILITIES", key, secretKey, githubAppSource)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer: check key %s in %s", key, secretKey, githubAppSource)
	}
	return value, nil
}

// modelHandlers builds the OpenAI and Anthropic routes' handlers for the
// administrator's MODEL_AUTH_MODE; either is nil when its route is disabled.
func modelHandlers(cfg *startupConfig, auditLogger audit.Writer, modelClient *http.Client,
	drain *modelgateway.Drain, runtimeClient client.Client,
) (openAIHandler, anthropicHandler http.Handler) {
	workload := cfg.modelAuthMode == modelauth.WorkloadMode
	if cfg.enabled[sprooziv1alpha1.CapabilityModelInference] && (cfg.anthropicKey != "" || workload) {
		var anthropicAuth modelauth.Authenticator = modelauth.AnthropicAPIKey{Key: cfg.anthropicKey}
		if workload {
			anthropicAuth = modelauth.Workload{}
		}
		anthropicHandler = modelgateway.NewAnthropicHandler(modelgateway.HandlerConfig{
			Pricing: cfg.pricing, AuditLogger: auditLogger,
			UpstreamURL:  cfg.anthropicUpstreamURL,
			ProviderAuth: anthropicAuth, HTTPClient: modelClient, Drain: drain,
		})
	}
	openAIEnabled := cfg.openAIKey != "" || cfg.modelAuthMode == modelauth.ChatGPTMode || workload
	if cfg.enabled[sprooziv1alpha1.CapabilityModelInference] && openAIEnabled {
		// The ChatGPT credential stays in sproozi-system, where the gateway's
		// access to it is limited to the one named Secret.
		credentialNamespace := envOrDefault("CREDENTIAL_NAMESPACE", "sproozi-system")
		var providerAuth modelauth.Authenticator = modelauth.APIKey{Key: cfg.openAIKey}
		if workload {
			providerAuth = modelauth.Workload{}
		}
		if cfg.modelAuthMode == modelauth.ChatGPTMode {
			store := modelauthkube.SessionStore{Client: runtimeClient, Namespace: credentialNamespace}
			if err := checkChatGPTSession(context.Background(), store); err != nil {
				log.Fatal(err)
			}
			oauthClient := newUpstreamClient(30 * time.Second)
			providerAuth = modelauth.NewChatGPT(store, oauthClient)
		}
		model := &modelgateway.Handler{Config: modelgateway.HandlerConfig{
			Pricing: cfg.pricing, AuditLogger: auditLogger,
			UpstreamURL:  cfg.openAIUpstreamURL,
			ProviderAuth: providerAuth, HTTPClient: modelClient, Drain: drain,
		}}
		if os.Getenv("MODEL_DIAGNOSTICS") == "true" {
			// Administrator opt-in: bounded provider errors only, separate from audit.
			// Never log prompts, model text, reasoning, raw bodies or auth headers.
			model.Config.Diagnostics = func(d modelgateway.ResponseDiagnostic) {
				_ = json.NewEncoder(os.Stderr).Encode(d)
			}
		}
		openAIHandler = model
	}
	return openAIHandler, anthropicHandler
}

func composeDispatcher(
	cfg *startupConfig, handlers map[sprooziv1alpha1.CapabilityKind]http.Handler,
	mcpHandler http.Handler, anthropicHandler http.Handler,
) gateway.Dispatcher {
	routes := gateway.Dispatcher{}
	host := cfg.host
	if host == "" {
		host = defaultHost
	}
	// Named MCP capabilities and the Sproozi control connection share the
	// gateway's own authority. The module chooses the exact account only after
	// authenticating the requested path.
	for _, port := range []string{"443", "8443"} {
		routes[net.JoinHostPort(host, port)] = gateway.Route{Tier: audit.TierProtocol, Handler: mcpHandler}
	}
	if cfg.mcpRegistry != nil {
		for _, authority := range cfg.mcpRegistry.Authorities() {
			routes[authority] = gateway.Route{Tier: audit.TierProtocol}
		}
	}
	for authority, cap := range endpoints.SemanticAuthorities {
		// Reserve semantic authorities even when a module is disabled. Generic
		// egress must never become a weaker fallback for a semantic service.
		route := gateway.Route{Capability: string(cap), Tier: audit.TierSemantic}
		if cfg.enabled[cap] {
			route.Handler = handlers[cap]
		}
		routes[authority] = route
	}
	// The two fixed provider protocols share model.inference accounting, while
	// credentials and upstream transport remain administrator-owned.
	anthropic := routes[modelgateway.AnthropicAuthority]
	anthropic.Handler = anthropicHandler
	routes[modelgateway.AnthropicAuthority] = anthropic
	if cfg.modelAuthMode == modelauth.WorkloadMode {
		for _, authority := range []string{modelgateway.OpenAIAuthority, modelgateway.AnthropicAuthority} {
			route := routes[authority]
			route.ProviderCredential = true
			routes[authority] = route
		}
	}
	if cfg.enabled[sprooziv1alpha1.CapabilityNetworkEgress] {
		for _, dest := range cfg.profiles.Destinations() {
			authority := net.JoinHostPort(dest.Host, strconv.Itoa(dest.Port))
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
	memory, err := goMemoryLimit()
	if err != nil {
		log.Fatal(err)
	}
	if memory.bytes > 0 {
		debug.SetMemoryLimit(memory.bytes)
	}
	if current := debug.SetMemoryLimit(-1); current == math.MaxInt64 {
		log.Printf("Go memory limit is unlimited, from %s", memory.source)
	} else {
		log.Printf("Go memory limit is %d bytes, from %s", current, memory.source)
	}
	// The authority watch cache logs through controller-runtime.
	ctrl.SetLogger(zap.New())
	drain := modelgateway.NewDrain()

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
	authorityClient, err := startAuthorityCache(context.Background(), config, agentNamespace)
	if err != nil {
		log.Fatal(err)
	}
	authenticator := gatewaykubernetes.NewAuthenticator(clientset, "sproozi-gateway", runtimeClient, agentNamespace).
		WithWatchCache(authorityClient)
	httpClient := newUpstreamClient(upstreamCallTimeout)
	modelClient := newModelClient()
	// The journal forwards every audit record unchanged and retains recent
	// denials for each execution's own sproozi diagnose tool.
	auditLogger := diagnose.NewJournal(audit.NewLogger(os.Stdout), diagnose.Limits{})
	// The shared gateway is the only component that receives Kubernetes API
	// credentials. Build the upstream transport from its own in-cluster
	// rest.Config so workload ServiceAccount tokens can never be forwarded to
	// Kubernetes and no workload-specific RBAC is required.
	kubeConfig := rest.CopyConfig(config)
	// The handler clears this Timeout for watches, which it bounds by their
	// timeoutSeconds instead.
	kubeConfig.Timeout = kubernetesReadTimeout
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
	openAIHandler, anthropicHandler := modelHandlers(cfg, auditLogger, modelClient, drain, runtimeClient)
	if openAIHandler != nil {
		handlers[sprooziv1alpha1.CapabilityModelInference] = openAIHandler
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
	raw := &destination.Handler{Config: destination.HandlerConfig{Profiles: cfg.profiles,
		AuditLogger: auditLogger,
		HTTPClient:  httpClient}}

	handlers[sprooziv1alpha1.CapabilityNetworkEgress] = raw

	// Unknown authorities are absent and are denied before an upstream call.
	budgetNamespace := envOrDefault("BUDGET_NAMESPACE", "sproozi-system")
	budgets := budget.NewTrackerWithLedger(budgetkubernetes.NewLedger(runtimeClient, budgetNamespace))
	// Every current reservation carries a lease, so a pending reservation
	// without one can only come from a gateway from before leases. Settle
	// those before serving, then keep recovering expired leases.
	legacySweep := func(ctx context.Context) error {
		return budgetkubernetes.Sweep(ctx, runtimeClient, budgetNamespace, time.Now(), true)
	}
	if !sweepLegacyOnce(context.Background(), legacySweep) {
		go retryLegacySweep(context.Background(), legacySweep, time.Second, budgetkubernetes.SweepInterval)
	}
	startLedgerMaintenance(runtimeClient, budgetNamespace, cfg.ledgerRetention)
	warnSystemConfigWrites(context.Background(), clientset, os.Stderr)
	named := mcpgateway.NewHandler(mcpgateway.HandlerConfig{Registry: cfg.mcpRegistry,
		Budgets: budgets, AuditLogger: auditLogger})
	mcpHandler := diagnose.NewControl(auditLogger, named)
	dispatcher := composeDispatcher(cfg, handlers, mcpHandler, anthropicHandler)
	for authority, route := range dispatcher {
		route.Budgets, route.AuditLogger = budgets, auditLogger
		dispatcher[authority] = route
	}
	guard := gateway.NewLeakGuard(dispatcher, nil)
	serving, err := startCertificateReloader(cfg, host)
	if err != nil {
		log.Fatal(err)
	}
	transport, err := proxytransport.New(proxytransport.Config{
		Certificates:       cfg.certificates,
		Serving:            serving,
		Authenticate:       authenticator.Authenticate,
		Revalidate:         authenticator.Revalidate,
		Handler:            guard,
		AllowedAuthority:   guard.Allows,
		ProviderCredential: guard.ForwardsCredential,
		AuditLogger:        auditLogger,

		MaxConnections:          cfg.admission.maxConnections,
		MaxSessionsPerExecution: cfg.admission.maxSessionsPerExecution,
		MaxModelCallDuration:    cfg.maxModelCall,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("shared gateway listening on :8443 for %s", host)
	listener, err := net.Listen("tcp", ":8443")
	if err != nil {
		log.Fatal(err)
	}
	healthListener, err := net.Listen("tcp", healthAddress)
	if err != nil {
		log.Fatal(err)
	}
	go func() { log.Fatal(newHealthServer().Serve(healthListener)) }()
	server := proxytransport.NewServer(":8443", serving, transport)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	go drainOnSignal(signals, server.Close, drain, func() { os.Exit(0) })
	if err := server.ServeTLS(transport.LimitListener(listener), "", ""); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	select {} // drainOnSignal exits once model calls have settled
}

// sweepLegacyOnce runs one startup recovery of legacy reservations and
// reports whether it succeeded.
func sweepLegacyOnce(ctx context.Context, sweep func(context.Context) error) bool {
	attemptCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := sweep(attemptCtx); err != nil {
		log.Printf("Failed to recover legacy budget reservations; retrying: %v", err)
		return false
	}
	return true
}

// retryLegacySweep repeats the startup recovery of legacy reservations with
// backoff until it succeeds once. Every reservation this gateway makes has a
// lease, so the pass is safe while the gateway serves.
func retryLegacySweep(ctx context.Context, sweep func(context.Context) error, initial, ceiling time.Duration) {
	for delay := initial; ; delay = min(2*delay, ceiling) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if sweepLegacyOnce(ctx, sweep) {
			return
		}
	}
}

// sweepLedgers recovers expired reservation leases across every budget
// ledger at start and then every interval, so an idle account does not keep a
// reservation that a stopped gateway left unsettled.
func sweepLedgers(ctx context.Context, c client.Client, namespace string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		sweepCtx, cancel := context.WithTimeout(ctx, interval)
		if err := budgetkubernetes.Sweep(sweepCtx, c, namespace, time.Now(), false); err != nil {
			log.Printf("Failed to recover expired budget reservations: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startLedgerMaintenance recovers expired leases every minute and prunes
// settled ledgers of deleted runs every hour.
func startLedgerMaintenance(c client.Client, namespace string, retention time.Duration) {
	go sweepLedgers(context.Background(), c, namespace, budgetkubernetes.SweepInterval)
	go pruneLedgerLoop(context.Background(), c, namespace, retention, ledgerPruneInterval)
}

// ledgerPruneInterval is how often settled ledgers of deleted runs are removed.
const ledgerPruneInterval = time.Hour

// pruneLedgerLoop deletes the ledgers of runs that no longer exist once they
// are settled and older than retention, so ledger storage is bounded by the
// runs the cluster retains.
func pruneLedgerLoop(ctx context.Context, c client.Client, namespace string, retention, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		pruneCtx, cancel := context.WithTimeout(ctx, interval)
		if deleted, err := pruneLedgers(pruneCtx, c, namespace, time.Now(), retention); err != nil {
			log.Printf("Failed to prune budget ledgers: %v", err)
		} else if deleted > 0 {
			log.Printf("Pruned %d budget ledgers of deleted AgentRuns", deleted)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pruneLedgers reads every AgentRun first: a ledger is pruned only if its run
// is absent from that complete list. A failed list prunes nothing.
func pruneLedgers(ctx context.Context, c client.Client, namespace string, now time.Time,
	retention time.Duration,
) (int, error) {
	var runs sprooziv1alpha1.AgentRunList
	if err := c.List(ctx, &runs); err != nil {
		return 0, fmt.Errorf("list AgentRuns: %w", err)
	}
	live := make(map[string]bool, len(runs.Items))
	for i := range runs.Items {
		live[string(runs.Items[i].UID)] = true
	}
	return budgetkubernetes.Prune(ctx, c, namespace, now, retention, func(uid string) bool { return live[uid] })
}

// loadLedgerRetention reads BUDGET_LEDGER_RETENTION with the rest of the
// startup configuration, so a bad value stops the gateway before it serves.
// A period under an hour could delete a ledger before a run that was just
// deleted is gone from every component's view.
func loadLedgerRetention() (time.Duration, error) {
	retention, err := durationEnv("BUDGET_LEDGER_RETENTION", budgetkubernetes.DefaultRetention)
	if err != nil {
		return 0, err
	}
	if retention < time.Hour {
		return 0, fmt.Errorf("BUDGET_LEDGER_RETENTION must be at least 1h, got %s", retention)
	}
	return retention, nil
}

// durationEnv reads a positive Go duration from name, or returns fallback.
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", name, raw)
	}
	return value, nil
}

// drainTimeout bounds the wait for model calls to settle on shutdown. It
// fits inside terminationGracePeriodSeconds (45) in config/manager/gateway.yaml
// with room for the settlement writes themselves.
const drainTimeout = 30 * time.Second

// drainOnSignal stops the gateway on SIGTERM: it stops accepting connections,
// ends every model call as interrupted so each settles its reservation with
// an estimate, then exits. A call still unsettled at drainTimeout is left to
// the ledger's recovery of abandoned reservations.
func drainOnSignal(signals <-chan os.Signal, stopAccepting func() error, drain *modelgateway.Drain, exit func()) {
	<-signals
	log.Printf("Stopping: draining model calls for up to %s", drainTimeout)
	_ = stopAccepting()
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := drain.Shutdown(ctx); err != nil {
		log.Printf("Model calls did not settle before exit; the ledger will recover their reservations: %v", err)
	}
	exit()
}

// authorityCacheSyncTimeout bounds startup while the watch cache lists the
// authority objects. The gateway is not ready until the cache has synced.
const authorityCacheSyncTimeout = 2 * time.Minute

// upstreamCallTimeout bounds one GitHub, package or egress call, from request
// to the last byte of the response. A call cannot outlast the inspected
// connection that carries it, so this is the longest request that connection
// allows (4m50s).
const upstreamCallTimeout = proxytransport.MaxRequestDuration

// modelHeaderTimeout bounds the wait for a provider's response headers. A
// model call has no whole-call timeout: its response streams until its
// deadline, and the model handler ends a response idle for
// modelgateway.DefaultIdleTimeout. A non-streaming call sends its headers
// only once the model has finished, so this allows the longest request one
// inspected connection allows, as before.
const modelHeaderTimeout = proxytransport.MaxRequestDuration

// directTransport is the default transport without ambient proxy settings.
// Upstream calls carry injected provider credentials; an HTTPS_PROXY added to
// the gateway Pod by a mutating webhook or a debugging edit must not route them
// elsewhere. The MCP bridge does the same (ADR 0001).
func directTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

// newUpstreamClient returns a client for provider and service calls: no
// ambient proxy, and no redirects, which could carry an injected credential
// to a host the policy did not name.
func newUpstreamClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     directTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// newModelClient returns the provider client. A harness that disconnects
// cancels the upstream call.
func newModelClient() *http.Client {
	transport := directTransport()
	transport.ResponseHeaderTimeout = modelHeaderTimeout
	return &http.Client{Transport: transport}
}

// kubernetesReadTimeout bounds a get, list or log read. Watches last up to
// their timeoutSeconds (at most 300) plus a margin instead, cut short by the
// inspected connection's remaining request time.
const kubernetesReadTimeout = 120 * time.Second

// startAuthorityCache starts the watch cache that serves revalidation of open
// sessions, and returns a client that reads AgentRun, AgentTemplate,
// AgentPolicy and agent-namespace ServiceAccount objects from it. The existing
// shared-gateway ClusterRole already grants list and watch on these. Other
// types read through to the API server: managed-session registration
// ConfigMaps and sandbox Pods carry the source revocation of a session move,
// so a watch delay there could let source and destination overlap. Any other
// cached type fails instead of silently starting a cluster-wide informer.
func startAuthorityCache(ctx context.Context, config *rest.Config, agentNamespace string) (client.Client, error) {
	authorityCache, err := cache.New(config, cache.Options{
		Scheme:                      scheme,
		ReaderFailOnMissingInformer: true,
		DefaultTransform:            cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ServiceAccount{}: {Namespaces: map[string]cache.Config{agentNamespace: {}}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create authority watch cache: %w", err)
	}
	for _, object := range []client.Object{&sprooziv1alpha1.AgentRun{}, &sprooziv1alpha1.AgentTemplate{},
		&sprooziv1alpha1.AgentPolicy{}, &corev1.ServiceAccount{}} {
		if _, err := authorityCache.GetInformer(ctx, object); err != nil {
			return nil, fmt.Errorf("watch %T for gateway authority: %w", object, err)
		}
	}
	go func() {
		if err := authorityCache.Start(ctx); err != nil {
			log.Fatalf("authority watch cache stopped: %v", err)
		}
	}()
	syncCtx, cancel := context.WithTimeout(ctx, authorityCacheSyncTimeout)
	defer cancel()
	if !authorityCache.WaitForCacheSync(syncCtx) {
		return nil, errors.New("authority watch cache did not sync: check the shared-gateway ClusterRole " +
			"grants list and watch on agentruns, agenttemplates, agentpolicies and serviceaccounts")
	}
	return client.New(config, client.Options{Scheme: scheme, Cache: &client.CacheOptions{
		Reader: authorityCache, DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Pod{}},
	}})
}

// checkChatGPTSession fails startup with the Secret, key and command that fix
// a missing or unusable ChatGPT credential. Errors never include token material.
func checkChatGPTSession(ctx context.Context, store modelauth.SessionStore) error {
	session, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("ChatGPT credential Secret is unavailable (%v): create key %s in %s "+
			"from the credential file that `make demo-chatgpt-login` saves",
			err, modelauthkube.SessionSecretKey, chatGPTSessionSource)
	}
	if err := session.Validate(); err != nil {
		return fmt.Errorf("%w (key %s in %s): run `bash hack/demo/chatgpt-login.sh --reauthorize` "+
			"(or `make demo-chatgpt-login CHATGPT_REAUTHORIZE=1`) and load the new credential into that Secret",
			err, modelauthkube.SessionSecretKey, chatGPTSessionSource)
	}
	return nil
}

// newHealthServer answers the kubelet readiness probe. It runs only once
// startup validation has passed and the TLS listener is bound, so readiness
// means the gateway can accept workloads. It exposes nothing else.
func newHealthServer() *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 4 << 10}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Provider destinations are trusted operator configuration; workloads continue
// to use the reserved semantic authority and cannot choose an upstream. Both
// providers receive credentials, so each must be a bare https origin.
func validateModelUpstream(name, value string) error {
	upstream, err := url.Parse(value)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" ||
		upstream.User != nil || value != "https://"+upstream.Host {
		return fmt.Errorf("%s must be an https origin", name)
	}
	return nil
}

// warnSystemConfigWrites reports any grant that lets the gateway write the
// ConfigMaps in sproozi-system that hold its own configuration (ADR 0041). It
// changes nothing; an administrator removes the binding.
func warnSystemConfigWrites(ctx context.Context, clients authentication.Interface, out io.Writer) {
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		attributes := &authorizationv1.ResourceAttributes{Namespace: "sproozi-system", Resource: "configmaps", Verb: verb}
		review, err := clients.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
			&authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attributes},
			}, metav1.CreateOptions{})
		if err != nil {
			return
		}
		if review.Status.Allowed {
			_, _ = fmt.Fprintf(out, "WARNING: the gateway can %s ConfigMaps in sproozi-system, "+
				"which hold its own configuration; remove the Role or binding that grants it (ADR 0041)\n", verb)
			return
		}
	}
}

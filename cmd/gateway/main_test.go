package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetkubernetes "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
	"github.com/andrewmccall/sproozi/internal/endpoints/destination"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	modelgateway "github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	modelauthkube "github.com/andrewmccall/sproozi/internal/modelauth/kubernetes"
	"github.com/andrewmccall/sproozi/internal/proxytransport"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestGatewaySchemeSupportsRunIdentityAndBudgetObjects(t *testing.T) {
	for _, object := range []runtime.Object{&corev1.ServiceAccount{}, &corev1.ConfigMap{}} {
		if _, _, err := scheme.ObjectKinds(object); err != nil {
			t.Errorf("gateway cannot resolve its real API object %T: %v", object, err)
		}
	}
}

func validGatewayEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_SERVERS_PATH", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	// A matching certificate/key pair is sufficient for tls.LoadX509KeyPair;
	// certificate contents need not be trusted during configuration loading.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	certDER,
		err := x509.CreateCertificate(rand.Reader,
		&x509.Certificate{SerialNumber: bigOne,
			DNSNames: []string{defaultHost,
				"kubernetes.default.svc",
				"api.openai.com",
				"api.anthropic.com",
				"github.com",
				"api.github.com",
				"pypi.org",
				"files.pythonhosted.org",
				"registry.example"}},
		&x509.Certificate{SerialNumber: bigOne},
		&key.PublicKey,
		key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE",
			Bytes: certDER}),
		0600); err != nil {
		t.Fatal(err)
	}
	privateKeyFile := filepath.Join(dir, "github.pem")
	pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	if err := os.WriteFile(privateKeyFile,
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
			Bytes: pkcs1}),
		0600); err != nil {
		t.Fatal(err)
	}
	profiles := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(profiles, []byte(`{
  "pypi": [
    {
      "scheme": "https",
      "host": "pypi.org",
      "port": 443
    }
  ]
}`), 0600); err != nil {
		t.Fatal(err)
	}
	pricing := filepath.Join(dir, "pricing.json")
	if err := os.WriteFile(pricing, []byte(`{
  "models": {
    "gpt-5.3-codex": {
      "inputMicrosPerMillionTokens": 1750000,
      "cachedInputMicrosPerMillionTokens": 175000,
      "outputMicrosPerMillionTokens": 14000000
    }
  }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPROOZI_ENABLED_CAPABILITIES",
		"kubernetes.read,model.inference,github.pull_request,packages.install,network.egress")
	t.Setenv("SPROOZI_GATEWAY_CERT_FILE", certFile)
	t.Setenv("SPROOZI_GATEWAY_KEY_FILE", keyFile)
	t.Setenv("EGRESS_PROFILES_PATH", profiles)
	t.Setenv("MODEL_PRICING_PATH", pricing)
	t.Setenv("OPENAI_API_KEY", "model-secret")
	t.Setenv("MODEL_AUTH_MODE", "api_key")
	t.Setenv("OPENAI_UPSTREAM_URL", "")
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "456")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", privateKeyFile)
}

func TestRegisteredMCPProvidersAndGatewayNeverFallBackToDestinationRoutes(t *testing.T) {
	profiles, err := destination.LoadProfileStore([]byte(`{"weaker":[
 {"scheme":"https","host":"remote.example","port":443},
 {"scheme":"https","host":"sproozi-gateway.sproozi-system.svc","port":8443}]}`))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := mcpgateway.LoadRegistry(
		[]byte(`{"servers":{"docs":{"url":"https://remote.example.:0443/mcp"}}}`),
		mcpgateway.CredentialDirectory)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &startupConfig{
		host: defaultHost, enabled: map[api.CapabilityKind]bool{api.CapabilityNetworkEgress: true},
		profiles: profiles, mcpRegistry: registry,
	}
	handlers := map[api.CapabilityKind]http.Handler{api.CapabilityNetworkEgress: http.NotFoundHandler()}
	routes := composeDispatcher(cfg, handlers, nil, nil)
	if routes.Allows("remote.example:443") || routes.Allows(defaultHost+":8443") {
		t.Fatal("reserved MCP addresses acquired destination fallback")
	}
	routes = composeDispatcher(cfg, handlers, http.NotFoundHandler(), nil)
	if !routes.Allows(defaultHost + ":8443") {
		t.Fatal("MCP delivery route was not composed")
	}
}

var bigOne = func() *big.Int { return big.NewInt(1) }()

func TestModelUpstreamsAreAdministratorOwnedHTTPSOrigins(t *testing.T) {
	for _, key := range []string{"OPENAI_UPSTREAM_URL", "ANTHROPIC_UPSTREAM_URL"} {
		t.Run(key, func(t *testing.T) {
			validGatewayEnvironment(t)
			t.Setenv(key, "https://fixture.sproozi-system.svc:8443")
			cfg, err := loadStartupConfig()
			if err != nil {
				t.Fatal(err)
			}
			retained := map[string]string{
				"OPENAI_UPSTREAM_URL": cfg.openAIUpstreamURL, "ANTHROPIC_UPSTREAM_URL": cfg.anthropicUpstreamURL,
			}
			if retained[key] != "https://fixture.sproozi-system.svc:8443" {
				t.Fatalf("configured provider origin was not retained: %q", retained[key])
			}
			for _, value := range []string{
				"file:///credentials", "https://credential@provider.example", "https://provider.example?token=secret",
				"https://provider.example/v1/messages", "http://fixture.sproozi-system.svc:8080", "https://provider.example#",
			} {
				t.Setenv(key, value)
				if _, err := loadStartupConfig(); err == nil || strings.Contains(err.Error(), value) {
					t.Fatalf("invalid origin %q was accepted or disclosed: %v", value, err)
				}
			}
		})
	}
}

func TestLoadStartupConfigValidatesAllCompositionInputs(t *testing.T) {
	validGatewayEnvironment(t)
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatalf("loadStartupConfig: %v", err)
	}
	if cfg.openAIKey != "model-secret" || len(cfg.certificates) == 0 || cfg.profiles == nil || cfg.pricing == nil {
		t.Fatalf("incomplete startup config: %#v", cfg)
	}
	if _, err := cfg.pricing.Cost("gpt-5.3-codex", 1, 0, 1); err != nil {
		t.Fatalf("demo model is not priced: %v", err)
	}

}

func TestAdmissionLimitsAreConfigurableWithinBounds(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv(maxConnectionsEnv, "")
	t.Setenv(maxSessionsPerExecutionEnv, "")
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatal(err)
	}
	defaults := connectionLimits{proxytransport.DefaultMaxConnections, proxytransport.DefaultMaxSessionsPerExecution}
	if cfg.admission != defaults {
		t.Fatalf("default admission = %+v", cfg.admission)
	}
	t.Setenv(maxConnectionsEnv, "64")
	t.Setenv(maxSessionsPerExecutionEnv, "4")
	if cfg, err = loadStartupConfig(); err != nil || cfg.admission != (connectionLimits{64, 4}) {
		t.Fatalf("configured admission = %+v, %v", cfg.admission, err)
	}
	t.Setenv(maxConnectionsEnv, "8")
	t.Setenv(maxSessionsPerExecutionEnv, "")
	if cfg, err = loadStartupConfig(); err != nil || cfg.admission != (connectionLimits{8, 8}) {
		t.Fatalf("per-execution default above a small connection limit = %+v, %v", cfg.admission, err)
	}
	for name, values := range map[string][2]string{
		"zero connections":                {"0", ""},
		"connections above limit":         {"4097", ""},
		"connections not a number":        {"many", ""},
		"per-execution above limit":       {"4096", "129"},
		"per-execution above connections": {"8", "9"},
		"zero per-execution":              {"", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(maxConnectionsEnv, values[0])
			t.Setenv(maxSessionsPerExecutionEnv, values[1])
			_, err := loadStartupConfig()
			if err == nil || !strings.Contains(err.Error(), "SPROOZI_GATEWAY_MAX_") ||
				!strings.HasSuffix(err.Error(), ": check the gateway Deployment environment") {
				t.Fatalf("invalid admission accepted, unnamed or without the fix: %v", err)
			}
		})
	}
}

func TestGoMemoryLimitIsNinetyPercentOfTheContainerLimit(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv(memoryLimitEnv, "")
	if limit, err := goMemoryLimit(); limit.bytes != 0 || err != nil || !strings.Contains(limit.source, "default") {
		t.Fatalf("no container limit = %+v, %v; want the runtime default", limit, err)
	}
	t.Setenv(memoryLimitEnv, "2147483648") // 2Gi, as resourceFieldRef renders it
	if limit, err := goMemoryLimit(); err != nil || limit.bytes != 1932735276 ||
		limit.source != "90% of SPROOZI_GATEWAY_MEMORY_LIMIT=2147483648" {
		t.Fatalf("2Gi container limit = %+v, %v; want 90%%", limit, err)
	}
	t.Setenv(memoryLimitEnv, "67108864") // the 64Mi minimum
	if limit, err := goMemoryLimit(); err != nil || limit.bytes != 60397974 {
		t.Fatalf("64Mi container limit = %+v, %v", limit, err)
	}
	t.Setenv("GOMEMLIMIT", "1GiB")
	if limit, err := goMemoryLimit(); limit.bytes != 0 || err != nil || limit.source != "GOMEMLIMIT=1GiB" {
		t.Fatalf("explicit GOMEMLIMIT overridden: %+v, %v", limit, err)
	}
	// An explicit GOMEMLIMIT does not hide a broken container limit.
	for _, gomemlimit := range []string{"", "1GiB"} {
		t.Setenv("GOMEMLIMIT", gomemlimit)
		for _, invalid := range []string{"2Gi", "0", "-1", "1", "67108863"} {
			t.Setenv(memoryLimitEnv, invalid)
			_, err := goMemoryLimit()
			if err == nil || !strings.Contains(err.Error(), memoryLimitEnv+`="`+invalid+`"`) ||
				!strings.Contains(err.Error(), "64Mi") {
				t.Fatalf("invalid memory limit %q with GOMEMLIMIT %q accepted or not echoed: %v", invalid, gomemlimit, err)
			}
		}
	}
}

func TestGatewayEnvironmentErrorsEchoTheValue(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv(maxConnectionsEnv, "many")
	if _, err := loadStartupConfig(); err == nil ||
		!strings.Contains(err.Error(), `SPROOZI_GATEWAY_MAX_CONNECTIONS="many"`) {
		t.Fatalf("connection limit error = %v", err)
	}
	t.Setenv(maxConnectionsEnv, "")
	t.Setenv("SPROOZI_ENABLED_CAPABILITIES", "kubernetes.write")
	if _, err := loadStartupConfig(); err == nil || !strings.Contains(err.Error(), `"kubernetes.write"`) {
		t.Fatalf("capability error = %v", err)
	}
}

func TestLoadStartupConfigFailsClosedBeforeListener(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"unknown auth mode": func(t *testing.T) { t.Setenv("MODEL_AUTH_MODE", "workload-choice") },
		"model key":         func(t *testing.T) { t.Setenv("OPENAI_API_KEY", "") },
		"GitHub app id":     func(t *testing.T) { t.Setenv("GITHUB_APP_ID", "not-an-id") },
		"GitHub app material": func(t *testing.T) {
			t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH",
				filepath.Join(t.TempDir(),
					"missing"))
		},

		"egress profiles": func(t *testing.T) { t.Setenv("EGRESS_PROFILES_PATH", filepath.Join(t.TempDir(), "missing")) },
		"TLS certificate": func(t *testing.T) {
			t.Setenv("SPROOZI_GATEWAY_CERT_FILE",
				filepath.Join(t.TempDir(),
					"missing"))
		},

		"TLS key": func(t *testing.T) {
			t.Setenv("SPROOZI_GATEWAY_KEY_FILE",
				filepath.Join(t.TempDir(),
					"missing"))
		},

		"inspection host": func(t *testing.T) { t.Setenv("SPROOZI_ENABLED_CAPABILITIES", "unknown") },
		"model pricing":   func(t *testing.T) { t.Setenv("MODEL_PRICING_PATH", filepath.Join(t.TempDir(), "missing")) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			validGatewayEnvironment(t)
			mutate(t)
			errText := ""
			if _, err := loadStartupConfig(); err != nil {
				errText = err.Error()
			} else {
				t.Fatal("expected startup validation failure")
			}
			if strings.Contains(errText, "model-secret") {
				t.Fatalf("startup error leaked model secret: %q", errText)
			}
		})
	}
}

func TestStartupCredentialErrorsNameTheirSource(t *testing.T) {
	cases := map[string]struct {
		mutate func(t *testing.T)
		want   []string
	}{
		"model key": {func(t *testing.T) { t.Setenv("OPENAI_API_KEY", "") },
			[]string{modelCredentialsSource, openAIKeyName, anthropicKeyName, modelAuthSource}},
		"auth mode": {func(t *testing.T) { t.Setenv("MODEL_AUTH_MODE", "workload-choice") },
			[]string{modelAuthSource, "key mode"}},
		"GitHub app id": {func(t *testing.T) { t.Setenv("GITHUB_APP_ID", "not-an-id") },
			[]string{githubAppSource, "app-id"}},
		"GitHub installation id": {func(t *testing.T) { t.Setenv("GITHUB_APP_INSTALLATION_ID", "") },
			[]string{githubAppSource, "installation-id"}},
		"GitHub private key": {func(t *testing.T) {
			t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", filepath.Join(t.TempDir(), "missing"))
		}, []string{githubAppSource, "private-key.pem"}},
		"GitHub app id missing": {func(t *testing.T) { t.Setenv("GITHUB_APP_ID", "") },
			[]string{"GITHUB_APP_ID is missing", githubAppSource, "app-id"}},
		"GitHub private key missing": {func(t *testing.T) { t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "") },
			[]string{"private key is missing", githubAppSource, "private-key.pem"}},
		"MCP registry": {func(t *testing.T) { t.Setenv("MCP_SERVERS_PATH", filepath.Join(t.TempDir(), "missing")) },
			[]string{"ConfigMap sproozi-system/sproozi-mcp-servers", "servers.json"}},
		"egress profiles": {func(t *testing.T) { t.Setenv("EGRESS_PROFILES_PATH", filepath.Join(t.TempDir(), "missing")) },
			[]string{"ConfigMap sproozi-system/sproozi-egress-gateway-profiles", "profiles.json"}},
		"model pricing": {func(t *testing.T) { t.Setenv("MODEL_PRICING_PATH", filepath.Join(t.TempDir(), "missing")) },
			[]string{"ConfigMap sproozi-system/sproozi-model-gateway-pricing", "pricing.json"}},
		"OpenAI key with newline": {func(t *testing.T) { t.Setenv("OPENAI_API_KEY", "model-secret\nsecond-line") },
			[]string{"OPENAI_API_KEY is invalid", modelCredentialsSource, openAIKeyName, "single line"}},
		"Anthropic key with newline": {func(t *testing.T) { t.Setenv("ANTHROPIC_API_KEY", "anthropic\r\nsecond-line") },
			[]string{"ANTHROPIC_API_KEY is invalid", modelCredentialsSource, anthropicKeyName, "single line"}},
		"certificate coverage": {func(t *testing.T) { t.Setenv("SPROOZI_GATEWAY_HOST", "uncovered.example") },
			[]string{"Secret sproozi-system/sproozi-gateway-tls", `"uncovered.example"`, "subjectAltName"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			validGatewayEnvironment(t)
			tc.mutate(t)
			_, err := loadStartupConfig()
			if err == nil {
				t.Fatal("expected startup validation failure")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("startup error %q does not name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "positive integer") && strings.Contains(err.Error(), "missing") {
				t.Fatalf("missing value reported as malformed: %q", err)
			}
			if strings.Contains(err.Error(), "model-secret") || strings.Contains(err.Error(), "second-line") {
				t.Fatalf("startup error leaked credential material: %q", err)
			}
		})
	}
}

func TestChatGPTStartupDoesNotRequireAPIKeyAndPinsPublicUpstream(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv("MODEL_AUTH_MODE", "chatgpt")
	t.Setenv("OPENAI_API_KEY", "")
	config, err := loadStartupConfig()
	if err != nil || config.modelAuthMode != "chatgpt" {
		t.Fatalf("ChatGPT mode requires an API key: %v", err)
	}
	t.Setenv("OPENAI_UPSTREAM_URL", "https://chatgpt.com/backend-api")
	if _, err := loadStartupConfig(); err == nil {
		t.Fatal("ChatGPT credentials can be sent to a non-public upstream")
	}
}

func TestDisabledModulesDoNotRequireProviderCredentials(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv("SPROOZI_ENABLED_CAPABILITIES", "kubernetes.read")
	for _, key := range []string{"OPENAI_API_KEY",
		"GITHUB_APP_ID",
		"GITHUB_APP_INSTALLATION_ID",
		"GITHUB_APP_PRIVATE_KEY_PATH",
		"EGRESS_PROFILES_PATH",
		"MODEL_PRICING_PATH"} {
		t.Setenv(key, "")
	}
	if _, err := loadStartupConfig(); err != nil {
		t.Fatal(err)
	}
}
func TestConfiguredDestinationRoutesAndSemanticPrecedence(t *testing.T) {
	profiles, err := destination.LoadProfileStore([]byte(`{
  "custom": [
    {
      "scheme": "https",
      "host": "registry.example",
      "port": 8443
    },
    {
      "scheme": "https",
      "host": "api.github.com",
      "port": 443
    }
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &startupConfig{enabled: map[api.CapabilityKind]bool{api.CapabilityNetworkEgress: true}, profiles: profiles}
	routes := composeDispatcher(cfg,
		map[api.CapabilityKind]http.Handler{api.CapabilityNetworkEgress: http.NotFoundHandler()}, nil, nil)
	if !routes.Allows("registry.example:8443") || routes.Allows("registry.example:443") {
		t.Fatal("profile route did not preserve exact port")
	}
	if routes.Allows("api.github.com:443") {
		t.Fatal("disabled semantic service fell back to weaker egress")
	}
}

func TestAnthropicStartupAndReservedRoute(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-fixture-key")
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.anthropicKey != "anthropic-fixture-key" || cfg.openAIKey != "" {
		t.Fatal("provider selection not retained")
	}
	if _, ok := cfg.certificates["api.anthropic.com"]; !ok {
		t.Fatal("active Anthropic authority not inspected")
	}
	if _, ok := cfg.certificates["api.openai.com"]; ok {
		t.Fatal("inactive OpenAI authority requires inspection")
	}
	routes := composeDispatcher(cfg, nil, nil, http.NotFoundHandler())
	if !routes.Allows("api.anthropic.com:443") || routes.Allows("api.openai.com:443") {
		t.Fatal("provider activation incorrect")
	}
	cfg.anthropicKey = ""
	profiles, err := destination.LoadProfileStore([]byte(`{"weaker":[
 {"scheme":"https","host":"api.anthropic.com","port":443}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.profiles = profiles
	routes = composeDispatcher(cfg,
		map[api.CapabilityKind]http.Handler{api.CapabilityNetworkEgress: http.NotFoundHandler()}, nil, nil)
	if routes.Allows("api.anthropic.com:443") {
		t.Fatal("disabled Anthropic fell back to egress")
	}
}

func TestWorkloadModeForwardsEachHarnessSignIn(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv("MODEL_AUTH_MODE", "workload")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg, err := loadStartupConfig()
	if err != nil {
		t.Fatalf("workload mode needs no gateway key: %v", err)
	}
	for _, host := range []string{"api.openai.com", "api.anthropic.com"} {
		if _, ok := cfg.certificates[host]; !ok {
			t.Fatalf("%s is not inspected in workload mode", host)
		}
	}
	openAI, anthropic := modelHandlers(cfg, audit.NewLogger(io.Discard), http.DefaultClient, nil, nil)
	if openAI == nil || anthropic == nil {
		t.Fatal("both provider routes are enabled in workload mode")
	}
	handlers := map[api.CapabilityKind]http.Handler{api.CapabilityModelInference: openAI}
	routes := composeDispatcher(cfg, handlers, nil, anthropic)
	for _, authority := range []string{"api.openai.com:443", "api.anthropic.com:443"} {
		if !routes.Allows(authority) || !routes[authority].ProviderCredential {
			t.Fatalf("%s does not forward the harness's own credential", authority)
		}
	}
	if routes["api.github.com:443"].ProviderCredential {
		t.Fatal("a non-model route was marked as carrying a provider credential")
	}
	// A provider with a gateway key keeps it; the other forwards each
	// harness's own sign-in.
	t.Setenv("OPENAI_API_KEY", "openai-fixture-key")
	cfg, err = loadStartupConfig()
	if err != nil {
		t.Fatal(err)
	}
	openAI, anthropic = modelHandlers(cfg, audit.NewLogger(io.Discard), http.DefaultClient, nil, nil)
	handlers = map[api.CapabilityKind]http.Handler{api.CapabilityModelInference: openAI}
	routes = composeDispatcher(cfg, handlers, nil, anthropic)
	if routes["api.openai.com:443"].ProviderCredential || !routes["api.anthropic.com:443"].ProviderCredential {
		t.Fatal("a mixed gateway forwarded the wrong provider's credential")
	}
	if mode := openAI.(*modelgateway.Handler).Config.ProviderAuth.Mode(); mode != modelauth.APIKeyMode {
		t.Fatalf("the OpenAI route with a gateway key uses %s", mode)
	}
}

func TestGitHubAppAcceptsPKCS8PrivateKey(t *testing.T) {
	validGatewayEnvironment(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pkcs8.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", path)
	if _, err := loadStartupConfig(); err != nil {
		t.Fatalf("PKCS#8 GitHub App key rejected: %v", err)
	}
}

type fixtureSessionStore struct {
	session modelauth.Session
	err     error
}

func (s fixtureSessionStore) Load(context.Context) (modelauth.Session, error) {
	return s.session, s.err
}

func (fixtureSessionStore) Replace(context.Context, modelauth.Session, modelauth.Session) error {
	return errors.New("not used")
}

func TestChatGPTSessionErrorsNameSecretKeyAndCommand(t *testing.T) {
	cases := map[string]struct {
		store fixtureSessionStore
		want  []string
	}{
		"unavailable": {fixtureSessionStore{err: errors.New("could not read ChatGPT credential Secret")},
			[]string{"ChatGPT credential Secret is unavailable", "could not read",
				"Secret sproozi-system/model-gateway-chatgpt", "session.json",
				"from the credential file that `make demo-chatgpt-login` saves"}},
		"incomplete registration": {fixtureSessionStore{session: modelauth.Session{AccessToken: "fixture-access-token"}},
			[]string{"ChatGPT registration is incomplete", "Secret sproozi-system/model-gateway-chatgpt",
				"session.json", "hack/demo/chatgpt-login.sh --reauthorize",
				"make demo-chatgpt-login CHATGPT_REAUTHORIZE=1", "load the new credential into that Secret"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkChatGPTSession(t.Context(), tc.store)
			if err == nil {
				t.Fatal("expected ChatGPT credential failure")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("ChatGPT error %q does not name %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "fixture-access-token") {
				t.Fatalf("ChatGPT error leaked token material: %q", err)
			}
		})
	}
	if chatGPTSessionSource != "Secret sproozi-system/"+modelauthkube.SessionSecretName {
		t.Fatalf("ChatGPT source %q does not match the store's Secret", chatGPTSessionSource)
	}
}

func TestHealthServerAnswersOnlyReadiness(t *testing.T) {
	handler := newHealthServer().Handler
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/readyz", http.StatusOK},
		{http.MethodPost, "/readyz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.status {
			t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, recorder.Code, tc.status)
		}
		if tc.status == http.StatusOK && recorder.Body.String() != "ok\n" {
			t.Fatalf("readiness body = %q", recorder.Body.String())
		}
	}
}

// A model call has no whole-call timeout: it streams until its own deadline,
// past the inspected connection's lifetime. Only the wait for its response
// headers is bounded, by what the connection allows a non-streaming call.
func TestModelClientBoundsOnlyResponseHeaders(t *testing.T) {
	model := newModelClient()
	transport, ok := model.Transport.(*http.Transport)
	if ok && transport.Proxy != nil {
		t.Fatal("model client must not use an ambient proxy")
	}
	if model.Timeout != 0 || !ok || transport.ResponseHeaderTimeout != proxytransport.MaxRequestDuration {
		t.Fatalf("model client timeout %v, header timeout %v", model.Timeout, transport.ResponseHeaderTimeout)
	}
	if upstreamCallTimeout+proxytransport.ResponseMargin > proxytransport.MaxSessionLifetime {
		t.Fatalf("upstream call timeout %v does not fit the inspected connection", upstreamCallTimeout)
	}
}

func TestMaxModelCallIsConfigurableWithinBounds(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv(maxModelCallEnv, "")
	cfg, err := loadStartupConfig()
	if err != nil || cfg.maxModelCall != 20*time.Minute {
		t.Fatalf("default model call maximum = %v, %v", cfg.maxModelCall, err)
	}
	t.Setenv(maxModelCallEnv, "3600")
	if cfg, err = loadStartupConfig(); err != nil || cfg.maxModelCall != time.Hour {
		t.Fatalf("configured model call maximum = %v, %v", cfg.maxModelCall, err)
	}
	for _, invalid := range []string{"0", "3601", "20m", "-1"} {
		t.Setenv(maxModelCallEnv, invalid)
		_, err := loadStartupConfig()
		if err == nil || !strings.Contains(err.Error(), maxModelCallEnv+`="`+invalid+`" must be an integer from 1 to 3600`) {
			t.Fatalf("invalid model call maximum %q accepted or unexplained: %v", invalid, err)
		}
	}
}

// BUDGET_LEDGER_RETENTION is checked with the rest of the startup
// configuration, before any handler or server is built.
func TestLedgerRetentionIsValidatedAtStartup(t *testing.T) {
	validGatewayEnvironment(t)
	t.Setenv("BUDGET_LEDGER_RETENTION", "")
	cfg, err := loadStartupConfig()
	if err != nil || cfg.ledgerRetention != budgetkubernetes.DefaultRetention {
		t.Fatalf("default ledger retention = %v, %v", cfg.ledgerRetention, err)
	}
	t.Setenv("BUDGET_LEDGER_RETENTION", "2h")
	if cfg, err = loadStartupConfig(); err != nil || cfg.ledgerRetention != 2*time.Hour {
		t.Fatalf("configured ledger retention = %v, %v", cfg.ledgerRetention, err)
	}
	for _, invalid := range []string{"30m", "soon", "-1h"} {
		t.Setenv("BUDGET_LEDGER_RETENTION", invalid)
		if _, err := loadStartupConfig(); err == nil || !strings.Contains(err.Error(), "BUDGET_LEDGER_RETENTION") {
			t.Fatalf("invalid ledger retention %q accepted or unexplained: %v", invalid, err)
		}
	}
}

// SIGTERM stops accepting connections, drains model calls so each settles,
// then exits, instead of exiting with reservations still held.
func TestSignalDrainsModelCallsBeforeExit(t *testing.T) {
	signals := make(chan os.Signal, 1)
	drain := modelgateway.NewDrain()
	var stopped, exited bool
	done := make(chan struct{})
	go func() {
		drainOnSignal(signals, func() error { stopped = true; return nil }, drain, func() { exited = true })
		close(done)
	}()
	signals <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not finish")
	}
	if !stopped || !exited || !drain.Stopping() {
		t.Fatalf("stopped=%v exited=%v draining=%v", stopped, exited, drain.Stopping())
	}
}

// The gateway sweeps budget ledgers at start, so an idle account's expired
// lease is recovered without another call on that account.
func TestGatewaySweepsBudgetLedgers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ledger := budgetkubernetes.NewLedger(c, "budgets")
	if _, err := ledger.Reserve(t.Context(), "idle", budget.UsageReservation{MaxUnits: 100,
		Recovery: &budget.Recovery{ExpiresAt: time.Now().Add(-time.Second), Units: 40}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go sweepLedgers(ctx, c, "budgets", time.Hour)
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := ledger.State(t.Context(), "idle")
		if err == nil && state.UnitsUsed == 40 && state.ReservedUnits == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired lease not swept: state=%+v err=%v", state, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A failed startup recovery of legacy reservations is retried until it
// succeeds once, rather than left until the next restart.
func TestLegacySweepRetriesUntilItSucceeds(t *testing.T) {
	attempts := 0
	sweep := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("API unavailable")
		}
		return nil
	}
	if sweepLegacyOnce(t.Context(), sweep) {
		t.Fatal("a failed attempt reported success")
	}
	done := make(chan struct{})
	go func() { retryLegacySweep(t.Context(), sweep, time.Millisecond, 4*time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy recovery was not retried to success")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d", attempts)
	}
}

// A ledger is pruned only when its AgentRun is missing from the full list, and
// a failed list prunes nothing.
func TestGatewayPrunesLedgersOfDeletedRuns(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	live := &api.AgentRun{}
	live.Name, live.Namespace, live.UID = "live", "default", "live-uid"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
	ledger := budgetkubernetes.NewLedger(c, "budgets")
	for _, key := range []budget.AccountKey{"live-uid/model.inference", "gone-uid/model.inference"} {
		r, err := ledger.Reserve(t.Context(), key, budget.UsageReservation{MaxUnits: 10, Units: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.Settle(t.Context(), r, budget.TrustedUsage{Units: 1}); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(budgetkubernetes.DefaultRetention + time.Hour)
	deleted, err := pruneLedgers(t.Context(), c, "budgets", later, budgetkubernetes.DefaultRetention)
	if err != nil || deleted != 1 {
		t.Fatalf("pruneLedgers = %d, %v; want 1 deletion", deleted, err)
	}
	if _, err := ledger.State(t.Context(), "live-uid/model.inference"); err != nil {
		t.Fatalf("live run's ledger was pruned: %v", err)
	}

	// A failed AgentRun list must delete nothing, even a prunable ledger.
	listFails := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, runs := list.(*api.AgentRunList); runs {
				return errors.New("apiserver unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})
	r, err := ledger.Reserve(t.Context(), "gone-again/model.inference", budget.UsageReservation{MaxUnits: 10, Units: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Settle(t.Context(), r, budget.TrustedUsage{Units: 1}); err != nil {
		t.Fatal(err)
	}
	deleted, err = pruneLedgers(t.Context(), listFails, "budgets", later, budgetkubernetes.DefaultRetention)
	if err == nil || deleted != 0 {
		t.Fatalf("pruneLedgers with a failed list = %d, %v; want an error and no deletion", deleted, err)
	}
	if _, err := ledger.State(t.Context(), "gone-again/model.inference"); err != nil {
		t.Fatalf("ledger deleted although the AgentRun list failed: %v", err)
	}
}

// Upstream clients carry injected credentials, so they ignore ambient proxy
// settings and never follow redirects.
func TestUpstreamClientIgnoresProxyAndRedirects(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	c := newUpstreamClient(time.Second)
	transport, ok := c.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, want no proxy", c.Transport)
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("upstream client follows redirects")
	}
}

// The gateway warns, at startup, when any Role still lets it
// write its own configuration.
func TestGatewayWarnsAboutUpgradeResidue(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		clients := k8sfake.NewClientset()
		clients.PrependReactor("create", "selfsubjectaccessreviews", func(
			action k8stesting.Action,
		) (bool, runtime.Object, error) {
			review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			review.Status.Allowed = allowed
			return true, review, nil
		})
		var out strings.Builder
		warnSystemConfigWrites(t.Context(), clients, &out)
		if strings.Contains(out.String(), "WARNING") != allowed {
			t.Fatalf("allowed=%v: output %q", allowed, out.String())
		}
	}
}

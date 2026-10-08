package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	egress "github.com/andrewmccall/sproozi/internal/endpoints/destination"
	mcpgateway "github.com/andrewmccall/sproozi/internal/endpoints/mcp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/andrewmccall/sproozi/internal/audit"
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
	profiles, err := egress.LoadProfileStore([]byte(`{"weaker":[
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
	if !routes.Allows(defaultHost+":8443") || routes[defaultHost+":8443"].Tier != audit.TierProtocol {
		t.Fatal("MCP delivery route was not composed")
	}
}

var bigOne = func() *big.Int { return big.NewInt(1) }()

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

func TestProxyListenerRequiresTLSAndHijackableHTTP1(t *testing.T) {
	server := newProxyServer(tls.Certificate{}, http.NotFoundHandler())
	if server.TLSConfig == nil || server.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("proxy listener TLS config = %#v", server.TLSConfig)
	}
	if server.TLSNextProto == nil || len(server.TLSNextProto) != 0 {
		t.Fatalf("proxy listener must explicitly disable HTTP/2: %#v", server.TLSNextProto)
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

func TestNewAuditLoggerWritesStructuredSecretFreeJSON(t *testing.T) {
	var output bytes.Buffer
	logger := newAuditLogger(&output)
	logger.Log(audit.Event{
		RunID:      "sproozi-agents/demo",
		Operation:  "model.inference",
		Allowed:    true,
		TokensUsed: 42,
	})

	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatalf("audit output is not JSON: %v", err)
	}
	if event["runID"] != "sproozi-agents/demo" || event["operation"] != "model.inference" || event["allowed"] != true {
		t.Fatalf("audit output = %#v", event)
	}
	for _, forbidden := range []string{"authorization", "credentials", "prompt", "response", "apiKey"} {
		if _, exists := event[forbidden]; exists {
			t.Fatalf("audit output contains forbidden field %q", forbidden)
		}
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
	profiles, err := egress.LoadProfileStore([]byte(`{
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
	if routes["registry.example:8443"].Tier != audit.TierDestination ||
		routes["api.github.com:443"].Tier != audit.TierSemantic {
		t.Fatal("route tiers are incorrect")
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
	profiles, err := egress.LoadProfileStore([]byte(`{"weaker":[
 {"scheme":"https","host":"api.anthropic.com","port":443}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.profiles = profiles
	routes = composeDispatcher(cfg,
		map[api.CapabilityKind]http.Handler{api.CapabilityNetworkEgress: http.NotFoundHandler()}, nil, nil)
	if routes.Allows("api.anthropic.com:443") || routes["api.anthropic.com:443"].Tier != audit.TierSemantic {
		t.Fatal("disabled Anthropic fell back to egress")
	}
}

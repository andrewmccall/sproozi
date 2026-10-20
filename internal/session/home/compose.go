package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	githubgateway "github.com/andrewmccall/sproozi/internal/endpoints/github"
	kubernetesgateway "github.com/andrewmccall/sproozi/internal/endpoints/kubernetes"
	"github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/nativeconfig"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	runtimekubernetes "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

type authority struct {
	owner       *gatewaydocker.Owner
	endpoint    string
	trust       []byte
	model       string
	options     []string
	kind        harness.NativeKind
	mcpEndpoint string
}

const inspectionHost = "api.openai.com"

func inspectionHostFor(kind harness.NativeKind) string {
	if kind == harness.NativeClaude {
		return "api.anthropic.com"
	}
	return inspectionHost
}

func (a *authority) InitializeAllocation(ctx context.Context, l session.Launch, i session.Incarnation) (runtimedocker.NativeAccess, error) {
	token, err := a.owner.Initialize(ctx, l, i)
	if err != nil {
		return runtimedocker.NativeAccess{}, err
	}
	u, _ := url.Parse(a.endpoint)
	u.User = url.UserPassword("sproozi", token)
	env, err := harness.NativeBaseEnvironment(a.kind, i.ResourceUID)
	if err != nil {
		return runtimedocker.NativeAccess{}, err
	}
	env = append(env, "NO_PROXY=", "no_proxy=")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		env = append(env, key+"="+u.String())
	}
	capabilities := make([]api.CapabilityKind, 0, len(l.Allowances))
	for capability := range l.Allowances {
		capabilities = append(capabilities, api.CapabilityKind(capability))
	}
	clients, err := harness.NativeClients(a.kind, capabilities, a.mcpEndpoint)
	if err != nil {
		return runtimedocker.NativeAccess{}, err
	}
	return runtimedocker.NativeAccess{Env: env, TrustBundlePEM: bytes.Clone(a.trust), Model: a.model, Options: slices.Clone(a.options), ClientConfig: clients}, nil
}
func (a *authority) Activate(ctx context.Context, l session.Launch, i session.Incarnation) error {
	return a.owner.Activate(ctx, l, i)
}
func (a *authority) Revoke(ctx context.Context, l session.Launch, i session.Incarnation) error {
	return a.owner.Revoke(ctx, l, i)
}
func (a *authority) Seal(ctx context.Context, l session.Launch, i session.Incarnation) (map[string]budget.TrustedUsage, error) {
	return a.owner.Seal(ctx, l, i)
}
func (a *authority) BindNative(ctx context.Context, l session.Launch, i session.Incarnation, id string) error {
	_, err := a.owner.BindNative(ctx, l, i, id)
	return err
}

// Open constructs fixed production adapters. It performs no Docker launch or
// inference; Serve starts the listener before any control request can activate.
func Open(c Config, log io.Writer) (_ *Service, err error) {
	if err := errors.Join(nativeconfig.ValidateHarness(string(c.Local.Harness), c.Model, c.NativeOptions),
		validateProjectApprovals(c)); err != nil {
		return nil, err
	}
	if log == nil {
		log = io.Discard
	}
	if !filepath.IsAbs(c.Directory) {
		return nil, errors.New("home: absolute directory required")
	}
	// Each store under the directory takes its own owner lock; a held one
	// means another home daemon serves the same directory.
	defer func() {
		if errors.Is(err, privatefs.ErrLocked) {
			err = anotherDaemon(c.Directory)
		}
	}()
	if err = privatefs.EnsureDir(c.Directory); err != nil {
		return nil, err
	}
	duration, err := time.ParseDuration(c.MaxDuration)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return nil, errors.New("home: invalid duration")
	}
	if err = privatefs.EnsureDir(c.DockerConfig); err != nil {
		return nil, err
	}
	engine, err := runtimedocker.NewEngine(c.DockerBinary, c.DockerSocket, c.DockerConfig)
	if err != nil {
		return nil, err
	}
	ledger, err := budgetfile.NewLedger(filepath.Join(c.Directory, "accounting"))
	if err != nil {
		return nil, err
	}
	s := &Service{config: c, duration: duration, ledger: ledger, placements: map[string]session.Placement{}, log: log,
		images: engine, sweepStop: make(chan struct{})}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	// Recover reservations a previous daemon left unsettled, then keep
	// recovering any that expire on idle accounts.
	// Home is the ledger's only owner and has not reserved yet, so a
	// reservation without a lease belongs to a stopped daemon.
	s.sweepLedger()
	go s.sweepLedgerPeriodically()
	policy, err := approvedPolicy(c)
	if err != nil {
		return nil, err
	}
	s.owner, err = gatewaydocker.Open(gatewaydocker.Config{Directory: filepath.Join(c.Directory, "grants"), Ledger: ledger, Observer: engine, Policy: gateway.PolicySnapshot{Name: "home-policy", Spec: policy}, Template: gateway.TemplateSnapshot{Name: "home-template", Spec: api.AgentTemplateSpec{Instructions: c.Local.Instructions}}, NativeCapabilities: configuredNative(c)})
	if err != nil {
		return nil, err
	}
	certificate, trust, endpoint, err := gatewayTLS(c)
	if err != nil {
		return nil, err
	}
	auth := &authority{owner: s.owner, endpoint: endpoint, trust: trust, model: c.Model, options: c.NativeOptions, kind: c.Local.Harness, mcpEndpoint: "https://" + c.Gateway.MCPAuthority}
	// Assign only a placement that opened: a typed-nil *Placement in s.local
	// would pass Close's io.Closer check.
	local, err := runtimedocker.OpenPlacement(filepath.Join(c.Directory, "docker"), engine, c.Local, auth)
	if err != nil {
		return nil, err
	}
	s.local = local
	s.placements[c.Local.Name] = s.local
	if err = s.configureRemotes(); err != nil {
		return nil, err
	}
	if err = s.configureWorkSources(); err != nil {
		return nil, err
	}
	s.coordinator, err = session.Open(filepath.Join(c.Directory, "journal"), s.placements, c.Local.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	modelConfig, err := provider(c.Gateway, c.Local.Harness)
	if err != nil {
		return nil, err
	}
	s.drain = model.NewDrain()
	modelConfig.Drain = s.drain
	gatewayConfig := gatewaydocker.GatewayConfig{Harness: c.Local.Harness, Certificates: map[string]tls.Certificate{inspectionHostFor(c.Local.Harness): certificate}, Model: modelConfig, AuditLogger: audit.NewLogger(log),
		MaxModelCall: maxModelCall(c.Gateway)}
	if c.Gateway.MCPRegistryFile != "" {
		data, readErr := readSetting("gateway.mcpRegistryFile", c.Gateway.MCPRegistryFile, 128<<10)
		if readErr != nil {
			return nil, readErr
		}
		gatewayConfig.MCPRegistry, err = mcp.LoadRegistry(data, c.Gateway.MCPCredentialsDirectory)
		if err != nil {
			return nil, err
		}
		host, _, hostErr := net.SplitHostPort(c.Gateway.MCPAuthority)
		if hostErr != nil {
			return nil, hostErr
		}
		leaf, _ := x509.ParseCertificate(certificate.Certificate[0])
		if leaf.VerifyHostname(host) != nil {
			return nil, errors.New("home: inspection certificate must cover MCP authority")
		}
		gatewayConfig.Certificates[host] = certificate
		gatewayConfig.MCPAuthority = c.Gateway.MCPAuthority
	}
	if err = nativeServices(c, certificate, &gatewayConfig); err != nil {
		return nil, err
	}
	handler, err := gatewaydocker.NewGateway(s.owner, gatewayConfig)
	if err != nil {
		return nil, err
	}
	s.gateway = proxytransport.NewServer(c.Gateway.Listen, certificate, handler)
	s.config.Allowances = maps.Clone(c.Allowances)
	s.config.ProjectApprovals = cloneProjectApprovals(c.ProjectApprovals)
	return s, nil
}

// CheckDocker proves that dockerBinary reaches the engine at dockerSocket and
// that the engine holds local.nativeImageID. serve calls it once before Open:
// Open runs no Docker command but saves the placement settings, and a home
// that cannot reach Docker cannot start or stop any local execution.
func CheckDocker(ctx context.Context, c Config) error {
	return checkDocker(ctx, c, configuredEngine(c))
}

// configuredEngine builds the engine only once checkDocker has found
// dockerBinary and dockerSocket, so those checks name the setting first.
type configuredEngine Config

func (c configuredEngine) ImageID(ctx context.Context, reference string) (string, error) {
	if err := privatefs.EnsureDir(c.DockerConfig); err != nil {
		return "", err
	}
	engine, err := runtimedocker.NewEngine(c.DockerBinary, c.DockerSocket, c.DockerConfig)
	if err != nil {
		return "", err
	}
	return engine.ImageID(ctx, reference)
}

func (c configuredEngine) NetworkExists(ctx context.Context, name string) (bool, error) {
	engine, err := runtimedocker.NewEngine(c.DockerBinary, c.DockerSocket, c.DockerConfig)
	if err != nil {
		return false, err
	}
	return engine.NetworkExists(ctx, name)
}

// CheckDocker runs CheckDocker with the service's settings and engine.
func (s *Service) CheckDocker(ctx context.Context) error { return checkDocker(ctx, s.config, s.images) }

func checkDocker(ctx context.Context, c Config, images imageResolver) error {
	if info, err := os.Stat(c.DockerBinary); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("home: dockerBinary %s is not a file; set it to the Docker CLI's absolute path", c.DockerBinary)
	}
	if runtime.GOOS != goosWindows {
		if _, err := os.Stat(c.DockerSocket); err != nil {
			return fmt.Errorf("home: dockerSocket %s does not exist; start Docker, or set dockerSocket to "+
				"the engine's Unix socket", c.DockerSocket)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := images.ImageID(ctx, c.Local.NativeImageID)
	if err != nil {
		return fmt.Errorf("home: dockerBinary %s could not inspect local.nativeImageID %s through dockerSocket %s (%w); "+
			"check that Docker is running, that dockerBinary is the Docker CLI and that the image is loaded",
			c.DockerBinary, c.Local.NativeImageID, c.DockerSocket, err)
	}
	if id != c.Local.NativeImageID {
		return fmt.Errorf("home: Docker resolved local.nativeImageID %s to %s; set local.nativeImageID to the image ID",
			c.Local.NativeImageID, id)
	}
	exists, err := images.NetworkExists(ctx, c.Local.NetworkName)
	if err != nil {
		return fmt.Errorf("home: could not list Docker networks to check local.networkName %q (%w)", c.Local.NetworkName, err)
	}
	if !exists {
		return fmt.Errorf("home: local.networkName %q does not exist; create it with \"docker network create %s\"",
			c.Local.NetworkName, c.Local.NetworkName)
	}
	return nil
}

// listenerProblem names the setting that keeps the gateway listener from
// serving the Docker-reachable address local.gatewayIPv4:local.gatewayPort.
func listenerProblem(c Config, port string) error {
	if ip := net.ParseIP(c.Local.GatewayIPv4); ip == nil || ip.To4() == nil || ip.IsUnspecified() {
		return fmt.Errorf("home: local.gatewayIPv4 %q is not a specific IPv4 address; set the host address "+
			"Docker containers reach", c.Local.GatewayIPv4)
	}
	host, listenPort, err := net.SplitHostPort(c.Gateway.Listen)
	switch {
	case err != nil:
		return fmt.Errorf("home: gateway.listen %q is not HOST:PORT; use %s or %s",
			c.Gateway.Listen, net.JoinHostPort(c.Local.GatewayIPv4, port), net.JoinHostPort("0.0.0.0", port))
	case listenPort != port:
		return fmt.Errorf("home: gateway.listen %q uses port %s, but local.gatewayPort is %s; make them equal",
			c.Gateway.Listen, listenPort, port)
	case host != c.Local.GatewayIPv4 && host != "0.0.0.0":
		return fmt.Errorf("home: gateway.listen %q listens on %s, which containers cannot reach; "+
			"use local.gatewayIPv4 %s or 0.0.0.0", c.Gateway.Listen, host, c.Local.GatewayIPv4)
	}
	return nil
}

func gatewayTLS(c Config) (tls.Certificate, []byte, string, error) {
	var cert tls.Certificate
	key, err := readSetting("gateway.keyFile", c.Gateway.KeyFile, 64<<10)
	if err != nil {
		return cert, nil, "", err
	}
	data, err := readSetting("gateway.certificateFile", c.Gateway.CertificateFile, 64<<10)
	if err != nil {
		return cert, nil, "", err
	}
	cert, err = tls.X509KeyPair(data, key)
	if err != nil {
		return cert, nil, "", keyPairError(c.Gateway, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return cert, nil, "", fmt.Errorf("gateway.certificateFile %s: %w", c.Gateway.CertificateFile, err)
	}
	want := fmt.Sprint(c.Local.GatewayPort)
	if err = listenerProblem(c, want); err != nil {
		return cert, nil, "", err
	}
	if leaf.VerifyHostname(c.Local.GatewayIPv4) != nil {
		return cert, nil, "", fmt.Errorf("home: gateway.certificateFile %s does not name local.gatewayIPv4 %s; "+
			"issue it with that address as an IP subject alternative name", c.Gateway.CertificateFile, c.Local.GatewayIPv4)
	}
	if inspection := inspectionHostFor(c.Local.Harness); leaf.VerifyHostname(inspection) != nil {
		return cert, nil, "", fmt.Errorf("home: gateway.certificateFile %s does not name %s, the inspection host "+
			"for local.harness %s; issue it with that name as a DNS subject alternative name",
			c.Gateway.CertificateFile, inspection, c.Local.Harness)
	}
	trust, err := readSetting("gateway.trustBundleFile", c.Gateway.TrustBundleFile, 64<<10)
	if err != nil {
		return cert, nil, "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return cert, nil, "", fmt.Errorf("gateway.trustBundleFile %s: no PEM CA certificate found", c.Gateway.TrustBundleFile)
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		intermediate, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			return cert, nil, "", fmt.Errorf("gateway.certificateFile %s: %w", c.Gateway.CertificateFile, parseErr)
		}
		intermediates.AddCert(intermediate)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: c.Local.GatewayIPv4}); err != nil {
		return cert, nil, "", fmt.Errorf("gateway.certificateFile %s does not verify against gateway.trustBundleFile %s: %w",
			c.Gateway.CertificateFile, c.Gateway.TrustBundleFile, err)
	}
	return cert, trust, "https://" + net.JoinHostPort(c.Local.GatewayIPv4, want), nil
}

// keyPairError names the gateway file that tls.X509KeyPair could not use.
func keyPairError(g GatewayConfig, err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "certificate input"):
		return fmt.Errorf("gateway.certificateFile %s: %w", g.CertificateFile, err)
	case strings.Contains(message, "key input") || strings.Contains(message, "the private key") ||
		strings.Contains(message, "parse private key"):
		return fmt.Errorf("gateway.keyFile %s: %w", g.KeyFile, err)
	default:
		return fmt.Errorf("gateway.certificateFile %s and gateway.keyFile %s: %w", g.CertificateFile, g.KeyFile, err)
	}
}

func provider(c GatewayConfig, kind harness.NativeKind) (model.HandlerConfig, error) {
	var result model.HandlerConfig
	u, err := url.Parse(c.ProviderURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !approvedProviderURL(u) {
		return result, errors.New("home: fixed HTTPS provider or controlled loopback fixture required")
	}
	data, err := readSetting("gateway.providerTokenFile", c.ProviderTokenFile, 16<<10)
	if err != nil {
		return result, err
	}
	token := strings.TrimSpace(string(data))
	if placeholderToken(token) {
		return result, fmt.Errorf("home: gateway.providerTokenFile %s still holds the placeholder %s; "+
			"write the provider API key there", c.ProviderTokenFile, token)
	}
	if token == "" {
		return result, fmt.Errorf("home: gateway.providerTokenFile %s is empty; write the provider API key there", c.ProviderTokenFile)
	}
	if strings.ContainsAny(token, " \r\n\t\x00") {
		return result, fmt.Errorf("home: gateway.providerTokenFile %s has whitespace or NUL inside the key; "+
			"it must hold only the provider API key", c.ProviderTokenFile)
	}
	result.UpstreamURL = c.ProviderURL
	if kind == harness.NativeClaude {
		result.ProviderAuth = modelauth.AnthropicAPIKey{Key: token}
	} else {
		result.ProviderAuth = modelauth.APIKey{Key: token}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.ProviderCAFile != "" {
		data, err = readSetting("gateway.providerCAFile", c.ProviderCAFile, 64<<10)
		if err != nil {
			return result, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return result, fmt.Errorf("gateway.providerCAFile %s: no PEM CA certificate found", c.ProviderCAFile)
		}
		tlsConfig.RootCAs = roots
	}
	// A model call has no whole-call timeout: it streams until its own
	// deadline, and the model handler ends a response that goes idle.
	result.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 45 * time.Second, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	if c.PricingFile != "" {
		data, err = readSetting("gateway.pricingFile", c.PricingFile, 128<<10)
		if err != nil {
			return result, err
		}
		result.Pricing, err = model.LoadPricingTable(bytes.NewReader(data))
	}
	return result, err
}

func approvedPolicy(c Config) (api.AgentPolicySpec, error) {
	policy := api.AgentPolicySpec{Budgets: map[api.CapabilityKind]api.EndpointBudget{}, MCPServers: c.Gateway.MCPPolicy}
	if err := nativeScopes(c, &policy); err != nil {
		return api.AgentPolicySpec{}, err
	}
	for name, a := range c.Allowances {
		if !a.Units.Bounded || a.Units.Value <= 0 || a.CostMicros.Value < 0 || a.CostMicros.Bounded && a.CostMicros.Value == 0 || !a.CostMicros.Bounded && a.CostMicros.Value != 0 {
			return api.AgentPolicySpec{}, errors.New("home: finite positive unit ceilings required")
		}
		capability := api.CapabilityKind(name)
		_, mcpCapability := capability.MCPServerName()
		if capability != api.CapabilityModelInference && !mcpCapability && !slices.Contains(configuredNative(c), capability) {
			return api.AgentPolicySpec{}, errors.New("home: capability lacks configured gateway")
		}
		if capability != api.CapabilityModelInference && a.CostMicros.Bounded {
			return api.AgentPolicySpec{}, errors.New("home: native and MCP capabilities support unit ceilings only")
		}
		if mcpCapability && (c.Gateway.MCPRegistryFile == "" || c.Gateway.MCPAuthority == "") {
			return api.AgentPolicySpec{}, errors.New("home: MCP allocation lacks registered broker")
		}
		policy.AllowedCapabilities = append(policy.AllowedCapabilities, capability)
		policy.Budgets[capability] = api.EndpointBudget{MaxUnits: a.Units.Value, MaxCostMicros: a.CostMicros.Value}
	}
	slices.Sort(policy.AllowedCapabilities)
	clients := &harness.NativeClientConfig{}
	for _, capability := range policy.AllowedCapabilities {
		if name, named := capability.MCPServerName(); named {
			clients.MCPServers = append(clients.MCPServers, name)
		}
	}
	if len(clients.MCPServers) > 0 {
		clients.MCPGateway = "https://" + c.Gateway.MCPAuthority
	}
	if err := clients.Validate(c.Local.Harness); err != nil {
		return api.AgentPolicySpec{}, err
	}
	if fmt.Sprintf("%x", sha256.Sum256([]byte(c.Local.Instructions))) != c.Local.InstructionDigest {
		return api.AgentPolicySpec{}, errors.New("home: native instruction digest mismatch")
	}
	return policy, nil
}

func (s *Service) configureRemotes() error {
	for name, fixed := range s.config.Remote {
		if name == s.config.Local.Name || len(name) == 0 || len(name) > 128 {
			return errors.New("home: distinct named remote profile required")
		}
		if fixed.MaxCheckpointBytes != s.config.Local.MaxCheckpointBytes {
			return errors.New("home: profiles must share the checkpoint byte limit")
		}
		remote, clientErr := profile.NewClient(fixed)
		if clientErr != nil {
			return clientErr
		}
		s.remotes = append(s.remotes, remote)
		s.placements[name] = remote
	}
	return nil
}

func (s *Service) configureWorkSources() error {
	fixed := s.config.WorkConnections()
	sources := make(map[string]WorkSource, len(fixed))
	for name, config := range fixed {
		if name == "" || len(name) > 128 || config.MaxCheckpointBytes != s.config.Local.MaxCheckpointBytes {
			return errors.New("home: bounded named work source required")
		}
		source, err := work.NewClient(config)
		if err != nil {
			for _, opened := range sources {
				if closer, ok := opened.(io.Closer); ok {
					_ = closer.Close()
				}
			}
			return err
		}
		sources[name] = source
	}
	s.SetWorkSources(sources)
	return nil
}

func approvedProviderURL(u *url.URL) bool {
	return u.Scheme == "https" || u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()
}

func configuredNative(c Config) []api.CapabilityKind {
	var capabilities []api.CapabilityKind
	if c.Gateway.GitHub != nil {
		if _, enabled := c.Allowances[string(api.CapabilityGitHubPullRequest)]; enabled {
			capabilities = append(capabilities, api.CapabilityGitHubPullRequest)
		}
	}
	if c.Gateway.Kubernetes != nil {
		if _, enabled := c.Allowances[string(api.CapabilityKubernetesRead)]; enabled {
			capabilities = append(capabilities, api.CapabilityKubernetesRead)
		}
	}
	return capabilities
}

func nativeScopes(c Config, policy *api.AgentPolicySpec) error {
	if github := c.Gateway.GitHub; github != nil {
		if github.AppID <= 0 || github.InstallationID <= 0 || len(github.Scope.Repositories) == 0 || len(github.Scope.Repositories) > 32 || len(github.Scope.AllowedBaseBranches) == 0 || len(github.Scope.AllowedBaseBranches) > 32 {
			return errors.New("home: fixed GitHub App and repository/base branch scope required")
		}
		for _, repo := range github.Scope.Repositories {
			parts := strings.Split(repo, "/")
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, " \t\r\n?#\\") {
				return errors.New("home: invalid GitHub repository scope")
			}
		}
		policy.GitHubPullRequest = github.Scope
	}
	if kube := c.Gateway.Kubernetes; kube != nil {
		if len(kube.Scope.Namespaces) == 0 || len(kube.Scope.Namespaces) > 32 {
			return errors.New("home: bounded Kubernetes namespace scope required")
		}
		for _, namespace := range kube.Scope.Namespaces {
			if len(validation.IsDNS1123Label(namespace)) > 0 {
				return errors.New("home: invalid Kubernetes namespace scope")
			}
		}
		if _, err := runtimekubernetes.KubernetesReadRules(kube.Scope.Resources); err != nil {
			return err
		}
		policy.KubernetesRead = kube.Scope
	}
	return nil
}

func nativeServices(c Config, certificate tls.Certificate, target *gatewaydocker.GatewayConfig) error {
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return err
	}
	cover := func(hosts ...string) error {
		for _, host := range hosts {
			if leaf.VerifyHostname(host) != nil {
				return errors.New("home: inspection certificate lacks configured native authority")
			}
			target.Certificates[host] = certificate
		}
		return nil
	}
	if github := c.Gateway.GitHub; github != nil {
		if err := cover("github.com", "api.github.com"); err != nil {
			return err
		}
		key, err := readSetting("gateway.github.privateKeyFile", github.PrivateKeyFile, 64<<10)
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		provider, err := githubgateway.NewAppTokenProvider(github.AppID, github.InstallationID, key, client)
		if err != nil {
			return err
		}
		target.GitHub = &githubgateway.HandlerConfig{TokenProvider: provider, HTTPClient: client, GitBaseURL: "https://github.com", APIBaseURL: "https://api.github.com"}
	}
	if kube := c.Gateway.Kubernetes; kube != nil {
		if err := cover("kubernetes.default.svc"); err != nil {
			return err
		}
		endpoint, err := url.Parse(kube.UpstreamURL)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
			return errors.New("home: exact HTTPS Kubernetes API authority required")
		}
		token, err := readSetting("gateway.kubernetes.tokenFile", kube.TokenFile, 16<<10)
		if err != nil {
			return err
		}
		credential := strings.TrimSpace(string(token))
		if credential == "" || strings.ContainsAny(credential, " \t\r\n\x00") {
			return errors.New("home: invalid Kubernetes gateway credential")
		}
		ca, err := readSetting("gateway.kubernetes.caFile", kube.CAFile, 64<<10)
		if err != nil {
			return err
		}
		client, err := rest.HTTPClientFor(&rest.Config{Host: kube.UpstreamURL, BearerToken: credential, TLSClientConfig: rest.TLSClientConfig{CAData: ca}, Timeout: 120 * time.Second, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }})
		if err != nil {
			return err
		}
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		target.Kubernetes = &kubernetesgateway.HandlerConfig{Upstream: endpoint, Client: client, MaxResponseBytes: 8 << 20, MaxWatchSeconds: 300}
	}
	return nil
}

// maxModelCall is the configured model call maximum, or zero for the gateway
// default. ServeProblems has already bounded the value.
func maxModelCall(g GatewayConfig) time.Duration {
	if g.MaxModelCallSeconds == nil {
		return 0
	}
	return time.Duration(*g.MaxModelCallSeconds) * time.Second
}

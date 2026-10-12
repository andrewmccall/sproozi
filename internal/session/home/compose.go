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
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	"github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	gatewaydocker "github.com/andrewmccall/sproozi/internal/gateway/docker"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/nativeconfig"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

type authority struct {
	owner    *gatewaydocker.Owner
	endpoint string
	trust    []byte
	model    string
	options  []string
	kind     harness.NativeKind
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
	env := []string{"PATH=/opt/git/bin:/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1",
		"NO_PROXY=", "no_proxy="}
	if a.kind == harness.NativeClaude {
		env = append(env, "ANTHROPIC_API_KEY=sproozi-worker-placeholder")
	} else {
		env = append(env, "OPENAI_API_KEY=sproozi-worker-placeholder")
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		env = append(env, key+"="+u.String())
	}
	return runtimedocker.NativeAccess{Env: env, TrustBundlePEM: bytes.Clone(a.trust), Model: a.model, Options: slices.Clone(a.options)}, nil
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
	if err := nativeconfig.ValidateHarness(string(c.Local.Harness), c.Model, c.NativeOptions); err != nil {
		return nil, err
	}
	if log == nil {
		log = io.Discard
	}
	if !filepath.IsAbs(c.Directory) {
		return nil, errors.New("home: absolute directory required")
	}
	if err = os.MkdirAll(c.Directory, 0700); err != nil {
		return nil, err
	}
	if err = private(c.Directory, true); err != nil {
		return nil, err
	}
	duration, err := time.ParseDuration(c.MaxDuration)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return nil, errors.New("home: invalid duration")
	}
	if err = os.MkdirAll(c.DockerConfig, 0700); err != nil {
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
	s := &Service{config: c, duration: duration, ledger: ledger, initialDir: filepath.Join(c.Directory, "initial"), placements: map[string]session.Placement{}}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	if err = os.MkdirAll(s.initialDir, 0700); err != nil {
		return nil, err
	}
	if err = private(s.initialDir, true); err != nil {
		return nil, err
	}
	policy, err := approvedPolicy(c)
	if err != nil {
		return nil, err
	}
	s.owner, err = gatewaydocker.Open(gatewaydocker.Config{Directory: filepath.Join(c.Directory, "grants"), Ledger: ledger, Observer: engine, Policy: gateway.PolicySnapshot{Name: "home-policy", Spec: policy}, Template: gateway.TemplateSnapshot{Name: "home-template", Spec: api.AgentTemplateSpec{Instructions: c.Local.Instructions}}})
	if err != nil {
		return nil, err
	}
	certificate, trust, endpoint, err := gatewayTLS(c)
	if err != nil {
		return nil, err
	}
	auth := &authority{owner: s.owner, endpoint: endpoint, trust: trust, model: c.Model, options: c.NativeOptions, kind: c.Local.Harness}
	s.local, err = runtimedocker.OpenPlacement(filepath.Join(c.Directory, "docker"), engine, c.Local, auth)
	if err != nil {
		return nil, err
	}
	s.placements[c.Local.Name] = s.local
	if err = s.configureRemotes(); err != nil {
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
	gatewayConfig := gatewaydocker.GatewayConfig{Harness: c.Local.Harness, Certificates: map[string]tls.Certificate{inspectionHostFor(c.Local.Harness): certificate}, Model: modelConfig, AuditLogger: audit.NewLogger(log)}
	if c.Gateway.MCPRegistryFile != "" {
		data, readErr := readPrivate(c.Gateway.MCPRegistryFile, 128<<10)
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
	handler, err := gatewaydocker.NewGateway(s.owner, gatewayConfig)
	if err != nil {
		return nil, err
	}
	s.gateway = &http.Server{Addr: c.Gateway.Listen, Handler: handler, TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	s.config.Allowances = maps.Clone(c.Allowances)
	return s, nil
}

func gatewayTLS(c Config) (tls.Certificate, []byte, string, error) {
	var cert tls.Certificate
	key, err := readPrivate(c.Gateway.KeyFile, 64<<10)
	if err != nil {
		return cert, nil, "", err
	}
	data, err := readPrivate(c.Gateway.CertificateFile, 64<<10)
	if err != nil {
		return cert, nil, "", err
	}
	cert, err = tls.X509KeyPair(data, key)
	if err != nil {
		return cert, nil, "", err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return cert, nil, "", err
	}
	ip := net.ParseIP(c.Local.GatewayIPv4)
	host, port, err := net.SplitHostPort(c.Gateway.Listen)
	want := fmt.Sprint(c.Local.GatewayPort)
	if err != nil || ip == nil || ip.To4() == nil || ip.IsUnspecified() || port != want || (host != c.Local.GatewayIPv4 && host != "0.0.0.0") || leaf.VerifyHostname(c.Local.GatewayIPv4) != nil || leaf.VerifyHostname(inspectionHostFor(c.Local.Harness)) != nil {
		return cert, nil, "", errors.New("home: TLS listener must cover fixed Docker-reachable IPv4 and inspection authority")
	}
	trust, err := readPrivate(c.Gateway.TrustBundleFile, 64<<10)
	if err != nil {
		return cert, nil, "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return cert, nil, "", errors.New("home: invalid gateway CA bundle")
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		intermediate, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			return cert, nil, "", parseErr
		}
		intermediates.AddCert(intermediate)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: c.Local.GatewayIPv4}); err != nil {
		return cert, nil, "", err
	}
	return cert, trust, "https://" + net.JoinHostPort(c.Local.GatewayIPv4, want), nil
}

func provider(c GatewayConfig, kind harness.NativeKind) (model.HandlerConfig, error) {
	var result model.HandlerConfig
	u, err := url.Parse(c.ProviderURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !approvedProviderURL(u) {
		return result, errors.New("home: fixed HTTPS provider or controlled loopback fixture required")
	}
	data, err := readPrivate(c.ProviderTokenFile, 16<<10)
	if err != nil {
		return result, err
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \r\n\t\x00") {
		return result, errors.New("home: invalid provider credential")
	}
	result.UpstreamURL = c.ProviderURL
	if kind == harness.NativeClaude {
		result.ProviderAuth = modelauth.AnthropicAPIKey{Key: token}
	} else {
		result.ProviderAuth = modelauth.APIKey{Key: token}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.ProviderCAFile != "" {
		data, err = readPrivate(c.ProviderCAFile, 64<<10)
		if err != nil {
			return result, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return result, errors.New("home: invalid provider CA bundle")
		}
		tlsConfig.RootCAs = roots
	}
	result.HTTPClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 45 * time.Second, IdleConnTimeout: 30 * time.Second}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	if c.PricingFile != "" {
		data, err = readPrivate(c.PricingFile, 128<<10)
		if err != nil {
			return result, err
		}
		result.Pricing, err = model.LoadPricingTable(bytes.NewReader(data))
	}
	return result, err
}

func approvedPolicy(c Config) (api.AgentPolicySpec, error) {
	policy := api.AgentPolicySpec{Budgets: map[api.CapabilityKind]api.EndpointBudget{}, MCPServers: c.Gateway.MCPPolicy}
	for name, a := range c.Allowances {
		if !a.Units.Bounded || a.Units.Value <= 0 || a.CostMicros.Value < 0 || a.CostMicros.Bounded && a.CostMicros.Value == 0 || !a.CostMicros.Bounded && a.CostMicros.Value != 0 {
			return api.AgentPolicySpec{}, errors.New("home: finite positive unit ceilings required")
		}
		capability := api.CapabilityKind(name)
		policy.AllowedCapabilities = append(policy.AllowedCapabilities, capability)
		policy.Budgets[capability] = api.EndpointBudget{MaxUnits: a.Units.Value, MaxCostMicros: a.CostMicros.Value}
	}
	slices.Sort(policy.AllowedCapabilities)
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

func approvedProviderURL(u *url.URL) bool {
	return u.Scheme == "https" || u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback()
}

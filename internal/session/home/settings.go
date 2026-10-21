package home

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativeconfig"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
)

// These mirror the Docker placement's own validation, which Open still applies.
var (
	imageID    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	dockerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

const (
	minVolume, maxVolume = 16 << 20, 4 << 30
	maxCapture           = 64 << 20
	minMemory, maxMemory = 16 << 20, 16 << 30
	maxCPUMilli, maxPIDs = 16000, 1024
	goosWindows          = "windows"
)

// ServeProblems names, by JSON key, each setting that Open needs but the
// configuration leaves missing or out of range, so serve can report them all at
// once. It reads no files: Open still checks file contents and permissions.
func (c Config) ServeProblems() []string {
	var p []string
	add := func(bad bool, key, want string) {
		if bad {
			p = append(p, key+" ("+want+")")
		}
	}
	absolute := func(key, path string) { add(!filepath.IsAbs(path), key, "absolute path") }
	// A blank value is as missing as an empty one.
	blank := func(v string) bool { return strings.TrimSpace(v) == "" }
	// An omitted model lets the harness choose its own default.
	add(c.Model != "" && blank(c.Model), "model", modelWant)
	absolute("dockerBinary", c.DockerBinary)
	if runtime.GOOS == goosWindows {
		add(blank(c.DockerSocket), "dockerSocket", `local named pipe such as "npipe:////./pipe/docker_engine"`)
	} else {
		absolute("dockerSocket", c.DockerSocket)
	}
	absolute("dockerConfig", c.DockerConfig)
	if filepath.IsAbs(c.Directory) {
		if problem := controlPathProblem(socketPath(c.Directory)); problem != "" {
			p = append(p, "directory ("+problem+")")
		}
	}
	l := c.Local
	harnessErr := l.Harness.Validate()
	add(harnessErr != nil, "local.harness", managedChoices())
	if harnessErr == nil {
		if err := nativeconfig.ValidateHarness(string(l.Harness), c.Model, c.NativeOptions); err != nil {
			p = append(p, "model or nativeOptions ("+err.Error()+")")
		}
	}
	add(!dockerName.MatchString(l.Name), "local.name", "Docker resource name")
	for _, image := range []struct{ key, id string }{
		{"local.nativeImageID", l.NativeImageID}, {"local.helperImageID", l.HelperImageID},
		{"local.anchorImageID", l.AnchorImageID},
	} {
		add(!imageID.MatchString(image.id), image.key, "sha256: image ID")
	}
	add(!dockerName.MatchString(l.NetworkName) || l.NetworkName == "host" || l.NetworkName == "none",
		"local.networkName", "existing Docker bridge network")
	ip := net.ParseIP(l.GatewayIPv4)
	add(ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast(), "local.gatewayIPv4", "IPv4 address")
	add(l.GatewayPort == 0, "local.gatewayPort", "port")
	add(blank(l.Instructions), "local.instructions", "approved instructions")
	add(fmt.Sprintf("%x", sha256.Sum256([]byte(l.Instructions))) != l.InstructionDigest,
		"local.instructionDigest", "SHA-256 of local.instructions as 64 lowercase hex characters, without a sha256: prefix")
	for _, limits := range []struct {
		key   string
		limit runtimedocker.ResourceLimits
	}{{"local.agentLimits", l.AgentLimits}, {"local.helperLimits", l.HelperLimits}, {"local.anchorLimits", l.AnchorLimits}} {
		key, limit := limits.key, limits.limit
		add(limit.MemoryBytes < minMemory || limit.MemoryBytes > maxMemory || limit.CPUMilli < 1 ||
			limit.CPUMilli > maxCPUMilli || limit.PIDs < 1 || limit.PIDs > maxPIDs,
			key, "MemoryBytes 16 MiB to 16 GiB, CPUMilli 1 to 16000, PIDs 1 to 1024")
	}
	add(l.VolumeBytes < minVolume || l.VolumeBytes > maxVolume, "local.volumeBytes", "16 MiB to 4 GiB")
	add(l.MaxCheckpointBytes <= 0 || l.MaxCheckpointBytes > maxCapture, "local.maxCheckpointBytes", "1 byte to 64 MiB")
	add(l.RetainedBytes < 2*l.MaxCheckpointBytes || l.RetainedBytes <= 0,
		"local.retainedBytes", "at least twice local.maxCheckpointBytes")
	p = append(p, c.gatewayProblems()...)
	return append(p, c.AllowanceProblems()...)
}

// modelWant describes a valid model setting.
const modelWant = "model name the harness requests, or omit it for the harness's default"

// placeholderToken reports a setup guide's placeholder, such as
// REPLACE_WITH_PROVIDER_API_KEY, left in place of a provider credential.
func placeholderToken(token string) bool { return strings.HasPrefix(token, "REPLACE_WITH_") }

// gatewayProblems names the gateway and further harness settings that
// ServeProblems reports.
func (c Config) gatewayProblems() []string {
	var p []string
	add := func(bad bool, key, want string) {
		if bad {
			p = append(p, key+" ("+want+")")
		}
	}
	absolute := func(key, path string) { add(!filepath.IsAbs(path), key, "absolute path") }
	blank := func(v string) bool { return strings.TrimSpace(v) == "" }
	g := c.Gateway
	_, port, err := net.SplitHostPort(g.Listen)
	add(err != nil || port != strconv.Itoa(int(c.Local.GatewayPort)), "gateway.listen", "HOST:PORT on local.gatewayPort")
	// A home-owned certificate store replaces the three administrator files.
	if g.PKIDirectory != "" {
		absolute("gateway.pkiDirectory", g.PKIDirectory)
	} else {
		absolute("gateway.certificateFile", g.CertificateFile)
		absolute("gateway.keyFile", g.KeyFile)
		absolute("gateway.trustBundleFile", g.TrustBundleFile)
	}
	u, err := url.Parse(g.ProviderURL)
	add(err != nil || u.Host == "" || !approvedProviderURL(u), "gateway.providerURL", "HTTPS provider URL")
	absolute("gateway.providerTokenFile", g.ProviderTokenFile)
	// The MCP authority always serves the Sproozi control connection.
	mcpHost, mcpPort, err := net.SplitHostPort(g.MCPAuthority)
	add(err != nil || mcpHost == "" || net.ParseIP(mcpHost) != nil || mcpPort != "443", "gateway.mcpAuthority",
		"DNS name on port 443 that the gateway certificate covers, such as mcp.sproozi.internal:443")
	for i, h := range c.Harnesses {
		key := fmt.Sprintf("harnesses[%d].", i)
		add(h.Harness.Validate() != nil, key+"harness", managedChoices())
		add(!dockerName.MatchString(h.Name), key+"name", "Docker resource name")
		add(h.GatewayPort == 0, key+"gatewayPort", "port")
		add(h.Model != "" && blank(h.Model), key+"model", modelWant)
		u, err := url.Parse(h.ProviderURL)
		add(err != nil || u.Host == "" || !approvedProviderURL(u), key+"providerURL", "HTTPS provider URL")
		absolute(key+"providerTokenFile", h.ProviderTokenFile)
	}
	// Compare the integer before any conversion, which would overflow.
	add(g.MaxModelCallSeconds != nil && (*g.MaxModelCallSeconds < 1 ||
		int(*g.MaxModelCallSeconds) > int(proxytransport.MaxModelCallDurationLimit/time.Second)),
		"gateway.maxModelCallSeconds", fmt.Sprintf("1 to %d seconds; omit it for %d",
			int(proxytransport.MaxModelCallDurationLimit/time.Second), int(proxytransport.DefaultMaxModelCallDuration/time.Second)))
	return p
}

// ProviderTokenProblem names gateway.providerTokenFile when it still holds a
// placeholder, or returns "". It ignores a file it cannot read: Open reports that.
func (c Config) ProviderTokenProblem() string {
	path := c.Gateway.ProviderTokenFile
	if !filepath.IsAbs(path) {
		return ""
	}
	data, err := privatefs.ReadFile(path, 16<<10)
	if token := strings.TrimSpace(string(data)); err == nil && placeholderToken(token) {
		return fmt.Sprintf("gateway.providerTokenFile (replace %s in %s with the provider API key)", token, path)
	}
	if err == nil && strings.TrimSpace(string(data)) == "" {
		return fmt.Sprintf("gateway.providerTokenFile (%s is empty; write the provider API key there)", path)
	}
	return ""
}

// AllowanceProblems names, by JSON key, each allowance that Open would reject.
// It reads no files.
func (c Config) AllowanceProblems() []string {
	var p []string
	for _, name := range slices.Sorted(maps.Keys(c.Allowances)) {
		a, key := c.Allowances[name], "allowances."+name
		if !a.Units.Bounded || a.Units.Value <= 0 {
			p = append(p, key+`.units ("bounded": true with a positive "value")`)
		}
		capability := api.CapabilityKind(name)
		_, mcp := capability.MCPServerName()
		modelInference := capability == api.CapabilityModelInference
		switch {
		case a.CostMicros.Value < 0 || a.CostMicros.Bounded && a.CostMicros.Value == 0 ||
			!a.CostMicros.Bounded && a.CostMicros.Value != 0:
			p = append(p, key+`.costMicros ("bounded": false with "value": 0 for no cap, or "bounded": true `+
				`with a positive "value")`)
		case a.CostMicros.Bounded && !modelInference:
			p = append(p, key+`.costMicros ("bounded": false; only model.inference supports a cost cap)`)
		}
		switch {
		case modelInference:
		case mcp:
			if c.Gateway.MCPRegistryFile == "" || c.Gateway.MCPAuthority == "" {
				p = append(p, key+" (needs gateway.mcpAuthority and gateway.mcpRegistryFile)")
			}
		case !slices.Contains(configuredNative(c), capability):
			p = append(p, key+" (model.inference, mcp.NAME, or a capability whose gateway is configured)")
		}
	}
	return p
}

// managedChoices names the registered managed harnesses for a setting message,
// such as "codex" or "claude-code".
func managedChoices() string {
	kinds := harness.ManagedKinds()
	quoted := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		quoted = append(quoted, strconv.Quote(string(kind)))
	}
	return strings.Join(quoted, " or ")
}

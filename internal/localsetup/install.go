package localsetup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// Options configure one local provisioning run.
type Options struct {
	Root           string
	Harness        harness.NativeKind
	Model          string
	Image          ImageRequest
	ProviderURL    string
	ProviderCAFile string
	Credential     CredentialInput
	// CheckCredential validates the key with the provider's model listing.
	CheckCredential bool
	Port            uint16
	StartService    bool
	Executable      string
	Timeout         time.Duration
	Progress        io.Writer
}

// Result summarises a completed installation.
type Result struct {
	Root         string        `json:"root"`
	Config       string        `json:"config"`
	Agents       string        `json:"agents"`
	Installation string        `json:"installation"`
	Image        Image         `json:"image"`
	Network      Network       `json:"network"`
	Endpoint     Endpoint      `json:"endpoint"`
	Harnesses    []Harness     `json:"harnesses"`
	Service      ServiceStatus `json:"service"`
	Paths        []PathReport  `json:"paths,omitempty"`
	Ready        bool          `json:"ready"`
}

// maxSocketPath is the portable Unix socket path limit (macOS allows 104
// bytes including the terminator).
const maxSocketPath = 103

// DefaultPort is the first gateway port setup tries.
const DefaultPort uint16 = 18443

// DefaultInstructions are the approved developer instructions for local
// sessions. Administrators can replace them before the first session.
const DefaultInstructions = "You are working in a Sproozi-managed workspace at /workspace/session/workspace. " +
	"Network access goes through the Sproozi gateway, which allows only approved services. " +
	"Work in the repository, keep changes focused and explain what you changed."

// Init provisions or repairs the local installation. Every stage is recorded
// in installation.json; a repeated or interrupted run converges on the same
// network, CA, credentials and configuration without duplicating them.
func Init(ctx context.Context, o Options) (Result, error) {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if o.Progress == nil {
		o.Progress = io.Discard
	}
	if o.Harness == "" {
		o.Harness = harness.NativeCodex
	}
	if err := o.Harness.Validate(); err != nil {
		return Result{}, fail("harness", err, "use --harness codex or --harness claude-code")
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	root, err := ResolveRoot(o.Root)
	if err != nil {
		return Result{}, err
	}
	l := Layout{Root: root}
	if socket := home.SocketPath(l.State()); runtime.GOOS != "windows" && len(socket) > maxSocketPath {
		return Result{}, fail("root", fmt.Errorf("the control socket path %s exceeds the %d-byte Unix socket limit", socket, maxSocketPath), "choose a shorter --root")
	}
	if err = l.ensureDirectories(); err != nil {
		return Result{}, fail("root", err, "fix ownership or permissions, or choose --root")
	}
	unlock, err := l.lock()
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	if err = RememberRoot(root); err != nil {
		return Result{}, err
	}
	step(o, "Using private installation root %s; shared agent settings live in %s", root, l.Agents())
	inst, err := LoadInstallation(l)
	if err != nil {
		return Result{}, err
	}
	save := func(stage string) error {
		inst.done(stage)
		return l.save(inst)
	}
	if err = save("root"); err != nil {
		return Result{}, err
	}

	docker, err := DiscoverDocker(ctx, l.DockerCLI())
	if err != nil {
		return Result{}, err
	}
	inst.Docker = &docker
	step(o, "Docker engine at %s runs Linux containers", docker.Socket)
	if err = save("docker"); err != nil {
		return Result{}, err
	}

	requested := o.Image.Reference != "" || o.Image.BuildSource != ""
	if inst.Image == nil || requested || VerifyImage(ctx, docker, *inst.Image) != nil {
		if inst.Image != nil && requested && placementExists(l) {
			return Result{}, fail("image", errors.New("changing the native image of an installation with retained sessions is not supported yet"), "finish or cancel sessions, then move the state directory aside")
		}
		o.Image.Progress = o.Progress
		image, err := ResolveImage(ctx, docker, inst.ID, o.Image)
		if err != nil {
			return Result{}, err
		}
		inst.Image = &image
	}
	step(o, "Native image %s (%s)", inst.Image.ID, inst.Image.Source)
	if err = save("image"); err != nil {
		return Result{}, err
	}

	network, err := EnsureNetwork(ctx, docker, inst.ID)
	if err != nil {
		return Result{}, err
	}
	inst.Network = &network
	step(o, "Network %s (%s)", network.Name, network.Subnet)
	if err = save("network"); err != nil {
		return Result{}, err
	}

	config, existing, err := readExistingConfig(l)
	if err != nil {
		return Result{}, err
	}
	if inst.Endpoint == nil {
		port, err := freePort(o.Port, inst)
		if err != nil {
			return Result{}, err
		}
		endpoint, err := SelectEndpoint(ctx, docker, *inst.Image, network, port)
		if err != nil {
			return Result{}, err
		}
		inst.Endpoint = &endpoint
	}
	step(o, "Workers reach the gateway at %s (%s; listening on %s)", inst.Endpoint.GatewayIPv4, inst.Endpoint.Method, inst.Endpoint.ListenHost)
	if err = save("endpoint"); err != nil {
		return Result{}, err
	}

	selected := inst.harness(o.Harness)
	if selected == nil {
		start := o.Port
		if len(inst.Harnesses) > 0 {
			start = maxPort(inst) + 1
		}
		port, err := freePort(start, inst)
		if err != nil {
			return Result{}, err
		}
		if o.Model == "" {
			return Result{}, fail("harness", fmt.Errorf("no model selected for %s", o.Harness), "pass --model NAME")
		}
		provider := ProviderFor(o.Harness)
		name := "local"
		if len(inst.Harnesses) > 0 {
			directory, _ := harness.NativeDirectory(o.Harness)
			name = "local-" + directory
		}
		h := Harness{Harness: o.Harness, Name: name, Port: port, Model: o.Model, ProviderURL: provider.URL,
			ProviderCAFile: o.ProviderCAFile, Credential: filepath.Join(l.Credentials(), provider.Name)}
		if o.ProviderURL != "" {
			h.ProviderURL = o.ProviderURL
		}
		inst.Harnesses = append(inst.Harnesses, h)
		selected = &inst.Harnesses[len(inst.Harnesses)-1]
	} else if o.Model != "" && o.Model != selected.Model {
		return Result{}, fail("harness", fmt.Errorf("%s already uses model %s", o.Harness, selected.Model), "edit the model in "+l.Config())
	}
	provider := ProviderFor(selected.Harness)
	if !HasCredential(l, provider.Name) || o.Credential.File != "" || o.Credential.Stdin != nil {
		if _, err = StoreCredential(l, provider.Name, o.Credential); err != nil {
			return Result{}, err
		}
		step(o, "Stored the %s credential in %s", provider.Name, l.Credentials())
	}
	if o.CheckCredential {
		if err = CheckCredential(ctx, selected.Harness, selected.ProviderURL, selected.ProviderCAFile, selected.Credential); err != nil {
			return Result{}, err
		}
		step(o, "The %s credential is accepted", provider.Name)
	}
	if err = save("harness:" + string(selected.Harness)); err != nil {
		return Result{}, err
	}

	store, err := pki.Open(l.PKI(), pki.DefaultPolicy, nil)
	if err != nil {
		return Result{}, err
	}
	names := pki.Names{IPs: []string{inst.Endpoint.GatewayIPv4}}
	for _, h := range inst.Harnesses {
		names.DNS = append(names.DNS, inspectionHost(h.Harness))
	}
	if _, err = store.Ensure(names); err != nil {
		return Result{}, fail("tls", err, "run sproozi certificates status")
	}
	step(o, "Private gateway CA and serving certificate are in %s", l.PKI())
	if err = save("pki"); err != nil {
		return Result{}, err
	}

	rendered := renderConfig(l, inst)
	changed := false
	if existing {
		if config, changed, err = mergeConfig(config, rendered); err != nil {
			return Result{}, err
		}
	} else {
		config, changed = rendered, true
	}
	if changed {
		if err = writeConfig(l, config, existing); err != nil {
			return Result{}, err
		}
		step(o, "Wrote %s", l.Config())
	}
	if _, err = home.ReadConfig(l.Config()); err != nil {
		return Result{}, fail("config", err, "fix "+l.Config())
	}
	if err = save("config"); err != nil {
		return Result{}, err
	}

	result := Result{Root: root, Config: l.Config(), Agents: l.Agents(), Installation: inst.ID, Image: *inst.Image,
		Network: network, Endpoint: *inst.Endpoint, Harnesses: inst.Harnesses}
	if !o.StartService {
		result.Service = ServiceState(ctx, l, config)
		return result, nil
	}
	if changed {
		if status := ServiceState(ctx, l, config); status.Running {
			step(o, "Restarting the home service to load the new configuration")
			if _, err = StopService(ctx, l, config, true, time.Minute); err != nil {
				return result, err
			}
		}
	}
	if result.Service, err = StartService(ctx, l, config, o.Executable, 2*time.Minute); err != nil {
		return result, err
	}
	step(o, "Home service is running; log %s", l.ServiceLog())
	if err = save("service"); err != nil {
		return result, err
	}
	trust, err := privatefs.ReadFile(store.TrustPath(), 64<<10)
	if err != nil {
		return result, err
	}
	for _, h := range inst.Harnesses {
		report, err := ProbePath(ctx, docker, inst.ID, *inst.Image, network, inst.Endpoint.GatewayIPv4, h.Port, trust)
		result.Paths = append(result.Paths, report)
		if err != nil {
			return result, err
		}
		step(o, "Verified %s gateway TLS and denied direct paths from a restricted worker", h.Harness)
	}
	result.Ready = true
	return result, save("ready")
}

func step(o Options, format string, args ...any) {
	_, _ = fmt.Fprintf(o.Progress, "• "+format+"\n", args...)
}

func inspectionHost(kind harness.NativeKind) string {
	if kind == harness.NativeClaude {
		return "api.anthropic.com"
	}
	return "api.openai.com"
}

func maxPort(inst Installation) uint16 {
	port := uint16(0)
	for _, h := range inst.Harnesses {
		port = max(port, h.Port)
	}
	return port
}

// freePort returns the first port at or after start that no recorded harness
// uses and the host can bind.
func freePort(start uint16, inst Installation) (uint16, error) {
	for port := start; port != 0 && port < start+64; port++ {
		used := slices.ContainsFunc(inst.Harnesses, func(h Harness) bool { return h.Port == port })
		if !used && PortFree("127.0.0.1", port) && PortFree("0.0.0.0", port) {
			return port, nil
		}
	}
	return 0, fail("network", fmt.Errorf("no free gateway port near %d", start), "free a port or pass --port")
}

// placementExists reports retained local placement state.
func placementExists(l Layout) bool {
	_, err := os.Stat(filepath.Join(l.State(), "docker", "config.json"))
	return err == nil
}

// Default local limits, from the onboarding proposal's starter values.
var (
	agentLimits  = runtimedocker.ResourceLimits{MemoryBytes: 1 << 30, CPUMilli: 1000, PIDs: 256}
	helperLimits = runtimedocker.ResourceLimits{MemoryBytes: 512 << 20, CPUMilli: 500, PIDs: 128}
	anchorLimits = runtimedocker.ResourceLimits{MemoryBytes: 64 << 20, CPUMilli: 100, PIDs: 32}
)

// codexOptions point stock Codex at the OpenAI Responses API through the
// gateway with the worker's placeholder key, and trust the managed workspace.
var codexOptions = []string{
	`model_provider="sproozi"`,
	`model_providers.sproozi.name="OpenAI through Sproozi"`,
	`model_providers.sproozi.base_url="https://api.openai.com/v1"`,
	`model_providers.sproozi.env_key="OPENAI_API_KEY"`,
	`model_providers.sproozi.wire_api="responses"`,
	`model_providers.sproozi.supports_websockets=false`,
	`projects."/workspace/session/workspace".trust_level="trusted"`,
}

func renderConfig(l Layout, inst Installation) home.Config {
	primary := inst.Harnesses[0]
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(DefaultInstructions)))
	c := home.Config{
		Directory: l.State(), DockerBinary: inst.Docker.Binary, DockerSocket: inst.Docker.Socket, DockerConfig: l.DockerCLI(),
		Local: runtimedocker.PlacementConfig{Harness: primary.Harness, Name: primary.Name,
			NativeImageID: inst.Image.ID, HelperImageID: inst.Image.ID, AnchorImageID: inst.Image.ID,
			NetworkName: inst.Network.Name, GatewayIPv4: inst.Endpoint.GatewayIPv4, GatewayPort: primary.Port,
			Instructions: DefaultInstructions, InstructionDigest: digest,
			AgentLimits: agentLimits, HelperLimits: helperLimits, AnchorLimits: anchorLimits,
			VolumeBytes: 512 << 20, MaxCheckpointBytes: 8 << 20, RetainedBytes: 128 << 20},
		Allowances:  map[string]session.Allowance{"model.inference": {Units: session.Limit{Bounded: true, Value: 200000}}},
		MaxDuration: "30m", Model: primary.Model, NativeOptions: nativeOptions(primary.Harness),
		Gateway: home.GatewayConfig{Listen: fmt.Sprintf("%s:%d", inst.Endpoint.ListenHost, primary.Port), PKIDirectory: l.PKI(),
			ProviderURL: primary.ProviderURL, ProviderTokenFile: primary.Credential, ProviderCAFile: primary.ProviderCAFile},
	}
	for _, h := range inst.Harnesses[1:] {
		c.Harnesses = append(c.Harnesses, home.LocalHarness{Harness: h.Harness, Name: h.Name, GatewayPort: h.Port, Model: h.Model,
			NativeOptions: nativeOptions(h.Harness), ProviderURL: h.ProviderURL, ProviderTokenFile: h.Credential, ProviderCAFile: h.ProviderCAFile})
	}
	return c
}

func nativeOptions(kind harness.NativeKind) []string {
	if kind == harness.NativeCodex {
		return slices.Clone(codexOptions)
	}
	return nil
}

func readExistingConfig(l Layout) (home.Config, bool, error) {
	if _, err := os.Lstat(l.Config()); os.IsNotExist(err) {
		return home.Config{}, false, nil
	}
	c, err := home.ReadConfig(l.Config())
	if err != nil {
		return c, true, fail("config", err, "fix or move aside "+l.Config())
	}
	return c, true, nil
}

// mergeConfig keeps every user-tunable value in an existing configuration.
// Setup only adds missing harnesses. Fields that bind retained placement
// records must still match the installation; setup never rewrites them.
func mergeConfig(existing, rendered home.Config) (home.Config, bool, error) {
	pinned := func(c home.Config) []any {
		return []any{c.Directory, c.Local.Harness, c.Local.Name, c.Local.NativeImageID, c.Local.HelperImageID,
			c.Local.AnchorImageID, c.Local.NetworkName, c.Local.GatewayIPv4, c.Local.GatewayPort, c.Gateway.PKIDirectory}
	}
	if !reflect.DeepEqual(pinned(existing), pinned(rendered)) {
		return existing, false, fail("config", errors.New("the existing configuration's placement differs from this installation"),
			"run sproozi doctor; changing images, network or gateway address needs a drained migration")
	}
	changed := false
	for _, h := range rendered.Harnesses {
		if !slices.ContainsFunc(existing.Harnesses, func(e home.LocalHarness) bool { return e.Harness == h.Harness }) {
			existing.Harnesses = append(existing.Harnesses, h)
			changed = true
		}
	}
	return existing, changed, nil
}

// writeConfig backs up an existing configuration, then replaces it atomically.
func writeConfig(l Layout, c home.Config, backup bool) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if backup {
		previous, err := privatefs.ReadFile(l.Config(), 256<<10)
		if err != nil {
			return err
		}
		name := "config.json.backup-" + time.Now().UTC().Format("20060102T150405Z")
		if err = privatefs.Replace(l.Root, name, previous); err != nil {
			return err
		}
	}
	return privatefs.Replace(l.Root, filepath.Base(l.Config()), append(data, '\n'))
}

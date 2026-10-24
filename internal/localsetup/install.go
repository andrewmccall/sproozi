package localsetup

import (
	"cmp"
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

	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/pki"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// Options configure one local provisioning run.
type Options struct {
	Root    string
	Harness harness.NativeKind
	// Instructions replace DefaultInstructions when the installation's
	// configuration is first written. They are fixed afterwards because
	// retained sessions are bound to their digest.
	Instructions   string
	Model          string
	Image          ImageRequest
	ProviderURL    string
	ProviderCAFile string
	Credential     CredentialInput
	// CheckCredential validates the key with the provider's model listing.
	CheckCredential bool
	// Token stores the credential as a sign-in token the gateway attaches,
	// such as one from claude setup-token, instead of an API key.
	Token bool
	// ChatGPT signs the gateway in to a ChatGPT plan in the browser and keeps
	// that sign-in, refreshing it itself, for an OpenAI harness.
	ChatGPT bool
	// OpenBrowser opens a sign-in page; nil prints nothing and fails.
	OpenBrowser func(string) error
	// SignIn lets the harness sign in to its own provider account instead of
	// the gateway holding a key (ADR 0047). Without it or a key, a harness
	// keeps the choice it already has.
	SignIn       bool
	Port         uint16
	StartService bool
	Executable   string
	Timeout      time.Duration
	Progress     io.Writer
}

// Result summarises a completed installation.
type Result struct {
	Root         string        `json:"root"`
	Config       string        `json:"config"`
	Profile      string        `json:"profile"`
	Installation string        `json:"installation"`
	Image        Image         `json:"image"`
	Network      Network       `json:"network"`
	Endpoint     Endpoint      `json:"endpoint"`
	Harnesses    []Harness     `json:"harnesses"`
	Service      ServiceStatus `json:"service"`
	Paths        []PathReport  `json:"paths,omitempty"`
	Ready        bool          `json:"ready"`
	// Warning says what still needs doing when setup saved its changes but
	// could not apply them yet.
	Warning string `json:"warning,omitempty"`
}

// goosWindows names the host where automatic setup is unsupported.
const goosWindows = "windows"

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
	if runtime.GOOS == goosWindows {
		// Create the private root, so a hand-written configuration has a
		// safe home, then explain that the rest is manual.
		root, err := ResolveRoot(o.Root)
		if err == nil {
			err = privatefs.EnsureDir(root)
		}
		if err != nil {
			return Result{}, fail("root", err, "choose another --root")
		}
		return Result{}, fail("platform", errors.New("automatic local setup is not yet supported on Windows"),
			"write config.json in the private directory "+root+" by hand, then run \"sproozi service run\"; "+
				"see docs/guides/windows.md")
	}
	o = o.withDefaults()
	if err := o.Harness.Validate(); err != nil {
		return Result{}, fail("harness", err, "use --harness codex or --harness claude-code")
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	i, unlock, err := openInstaller(o)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	for _, stage := range []func(context.Context) error{i.findDocker, i.prepareImage, i.prepareNetwork, i.findEndpoint, i.prepareHarness, i.prepareCertificates, i.writeConfiguration} {
		if err = stage(ctx); err != nil {
			return Result{}, err
		}
	}
	return i.serve(ctx)
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Progress == nil {
		o.Progress = io.Discard
	}
	if o.Harness == "" {
		o.Harness = harness.NativeCodex
	}
	if o.Instructions == "" {
		o.Instructions = DefaultInstructions
	}
	return o
}

// installer carries one provisioning run's journal and resolved resources.
type installer struct {
	o        Options
	l        Layout
	inst     Installation
	docker   Docker
	selected *Harness
	store    *pki.Store
	config   home.Config
	changed  bool
	// credential records that this run stored a provider credential or CA
	// bundle, which a running home daemon reads only at startup.
	credential bool
	// providerCA is the CA bundle this run stored for the selected harness.
	providerCA string
}

// openInstaller validates the root, takes the setup lock and loads the journal.
func openInstaller(o Options) (*installer, func(), error) {
	root, err := ResolveRoot(o.Root)
	if err != nil {
		return nil, nil, err
	}
	l := Layout{Root: root}
	if socket := home.SocketPath(l.State()); len(socket) > maxSocketPath {
		return nil, nil, fail("root", fmt.Errorf("the control socket path %s exceeds the %d-byte Unix socket limit",
			socket, maxSocketPath), "choose a shorter --root")
	}
	if err = l.checkRoot(); err != nil {
		return nil, nil, err
	}
	if err = l.ensureDirectories(); err != nil {
		return nil, nil, fail("root", err, "fix ownership or permissions, or choose --root")
	}
	unlock, err := l.lock()
	if err != nil {
		return nil, nil, err
	}
	i := &installer{o: o, l: l}
	if err = RememberRoot(root); err == nil {
		i.inst, err = LoadInstallation(l)
	}
	if err == nil {
		i.step("Using local installation %s; your profile is %s", root, l.Profile())
		err = i.save("root")
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return i, unlock, nil
}

func (i *installer) save(stage string) error {
	i.inst.done(stage)
	return i.l.save(i.inst)
}

func (i *installer) step(format string, args ...any) { step(i.o, format, args...) }

func (i *installer) findDocker(ctx context.Context) error {
	docker, err := DiscoverDocker(ctx, i.l.DockerCLI())
	if err != nil {
		return err
	}
	i.docker, i.inst.Docker = docker, &docker
	i.step("Docker engine at %s runs Linux containers", docker.Socket)
	return i.save("docker")
}

func (i *installer) prepareImage(ctx context.Context) error {
	requested := i.o.Image.Reference != "" || i.o.Image.BuildSource != ""
	// Repeating setup with the recorded image is not a change.
	if recorded := i.inst.Image; recorded != nil && i.o.Image.BuildSource == "" &&
		(i.o.Image.Reference == recorded.Reference || i.o.Image.Reference == recorded.ID) {
		requested = false
	}
	if i.inst.Image == nil || requested || VerifyImage(ctx, i.docker, *i.inst.Image) != nil {
		if i.inst.Image != nil && requested && placementExists(i.l) {
			return fail("image", errors.New("changing the native image of an installation with retained sessions is not supported yet"),
				"finish or cancel sessions, then move the state directory aside")
		}
		request := i.o.Image
		request.Progress = i.o.Progress
		image, err := ResolveImage(ctx, i.docker, i.inst.ID, request)
		if err != nil {
			return err
		}
		i.inst.Image = &image
	}
	i.step("Native image %s (%s)", i.inst.Image.ID, i.inst.Image.Source)
	return i.save("image")
}

func (i *installer) prepareNetwork(ctx context.Context) error {
	network, err := EnsureNetwork(ctx, i.docker, i.inst.ID)
	if err != nil {
		return err
	}
	i.inst.Network = &network
	i.step("Network %s (%s)", network.Name, network.Subnet)
	return i.save("network")
}

func (i *installer) findEndpoint(ctx context.Context) error {
	if i.inst.Endpoint == nil {
		port, err := freePort(cmp.Or(i.o.Port, DefaultPort), i.inst)
		if err != nil {
			return err
		}
		endpoint, err := SelectEndpoint(ctx, i.docker, *i.inst.Image, *i.inst.Network, port)
		if err != nil {
			return err
		}
		i.inst.Endpoint = &endpoint
	}
	e := i.inst.Endpoint
	i.step("Workers reach the gateway at %s (%s; listening on %s)", e.GatewayIPv4, e.Method, e.ListenHost)
	return i.save("endpoint")
}

// prepareHarness adds the requested harness with its own port, or reuses it, then
// stores and optionally validates its provider credential.
func (i *installer) prepareHarness(ctx context.Context) error {
	o := i.o
	i.selected = i.inst.harness(o.Harness)
	switch {
	case i.selected == nil:
		if err := i.addHarness(); err != nil {
			return err
		}
	case o.Model != "" && o.Model != i.selected.Model:
		current := "the harness's default model"
		if i.selected.Model != "" {
			current = "model " + i.selected.Model
		}
		return fail("harness", fmt.Errorf("%s already uses %s", o.Harness, current), "edit the model in "+i.l.Config())
	}
	keyGiven := o.Credential.File != "" || o.Credential.Stdin != nil
	if o.SignIn && !SignsIn(i.selected.Harness) {
		return fail("harness", fmt.Errorf("%s can't sign in to its provider itself yet", o.Harness),
			"leave out --sign-in and give it a provider key")
	}
	mechanism := chooseMechanism(o, *i.selected, keyGiven)
	provider := ProviderFor(i.selected.Harness, mechanism)
	i.selected.ProviderURL = providerURL(o.ProviderURL, i.selected.ProviderURL, provider)
	// An explicit CA bundle replaces the stored one on every run.
	if o.ProviderCAFile != "" {
		path, err := StoreProviderCA(i.l, provider.Name, o.ProviderCAFile)
		if err != nil {
			return err
		}
		i.selected.ProviderCAFile, i.providerCA, i.credential = path, path, true
		i.step("Stored the %s endpoint's CA bundle in %s", provider.Name, path)
	}
	if o.ChatGPT {
		return i.signInChatGPT(ctx, provider)
	}
	if mechanism == harness.CredentialSignIn {
		// The harness signs in in its first session; the gateway forwards
		// that sign-in and stores no key (ADR 0047).
		if i.selected.Credential != "" {
			i.step("%s now signs in to %s itself; the stored key is no longer used", o.Harness, provider.Name)
		}
		i.selected.Credential, i.selected.Kind = "", ""
		return i.save("harness:" + string(i.selected.Harness))
	}
	i.selected.Credential = filepath.Join(i.l.Credentials(), provider.Name)
	switch {
	case mechanism == harness.CredentialToken:
		i.selected.Kind = modelauth.TokenMode
	case keyGiven:
		i.selected.Kind = ""
	}
	if !HasCredential(i.l, provider.Name) || o.Credential.File != "" || o.Credential.Stdin != nil {
		if _, err := StoreCredential(i.l, provider.Name, o.Credential); err != nil {
			return err
		}
		i.credential = true
		i.step("Stored the %s credential in %s", provider.Name, i.l.Credentials())
	}
	if o.CheckCredential && i.selected.Kind == "" {
		h := i.selected
		if err := CheckCredential(ctx, h.Harness, h.ProviderURL, h.ProviderCAFile, h.Credential); err != nil {
			return err
		}
		i.step("The %s credential is accepted", provider.Name)
	}
	return i.save("harness:" + string(i.selected.Harness))
}

// chooseMechanism is how the harness will reach its provider after this run:
// what the flags ask for, or what it already uses.
func chooseMechanism(o Options, current Harness, keyGiven bool) harness.NativeCredential {
	switch {
	case o.ChatGPT:
		return harness.CredentialKey
	case o.SignIn || (current.Credential == "" && !keyGiven && !o.Token && SignsIn(current.Harness)):
		return harness.CredentialSignIn
	case o.Token:
		return harness.CredentialToken
	case keyGiven:
		return harness.CredentialKey
	}
	if mechanism := current.Mechanism(); mechanism != harness.CredentialSignIn {
		return mechanism
	}
	return harness.CredentialKey
}

// providerURL is the harness's provider URL: the flag's, or a default that
// follows the mechanism, such as Copilot's plan host when it signs in. A
// private endpoint stays as configured.
func providerURL(flag, current string, provider Provider) string {
	switch {
	case flag != "":
		return flag
	case slices.Contains([]string{openAIURL, anthropicURL, copilotURL}, current):
		return provider.URL
	}
	return current
}

// signInChatGPT keeps a gateway-held ChatGPT plan sign-in for an OpenAI
// harness (ADR 0047), reusing a valid stored one.
func (i *installer) signInChatGPT(ctx context.Context, provider Provider) error {
	if provider.Name != string(harness.ProtocolOpenAI) {
		return fail("harness", fmt.Errorf("%s doesn't use OpenAI, so it can't use a ChatGPT plan sign-in", i.o.Harness),
			"leave out --chatgpt")
	}
	stored := filepath.Join(i.l.Credentials(), "chatgpt-session.json")
	if _, err := modelauth.NewFileSessionStore(stored).Load(ctx); err != nil {
		if i.o.OpenBrowser == nil {
			return fail("credential", errors.New("a ChatGPT sign-in needs a browser on this machine"), "run sproozi init in a terminal")
		}
		i.step("Continue with ChatGPT: approve Sproozi's requested plan permissions in your browser")
		if _, err = modelauth.Login(ctx, stored, filepath.Join(i.l.Credentials(), "chatgpt-host.json"), i.o.OpenBrowser); err != nil {
			return fail("credential", err, "run sproozi init --chatgpt again to retry the sign-in")
		}
	}
	i.selected.Credential, i.selected.Kind = stored, modelauth.ChatGPTMode
	i.credential = true
	i.step("The gateway keeps the ChatGPT sign-in in %s and refreshes it itself", stored)
	return i.save("harness:" + string(i.selected.Harness))
}

func (i *installer) addHarness() error {
	o := i.o
	// --port picks where the search starts for this agent; without it, a
	// later agent takes the next port after the existing ones.
	start := cmp.Or(o.Port, DefaultPort)
	if o.Port == 0 && len(i.inst.Harnesses) > 0 {
		start = maxPort(i.inst) + 1
	}
	port, err := freePort(start, i.inst)
	if err != nil {
		return err
	}
	provider := ProviderFor(o.Harness, harness.CredentialKey)
	name := "local"
	if len(i.inst.Harnesses) > 0 {
		directory, _ := harness.NativeDirectory(o.Harness)
		name = "local-" + directory
	}
	h := Harness{Harness: o.Harness, Name: name, Port: port, Model: o.Model, ProviderURL: cmp.Or(o.ProviderURL, provider.URL),
		Credential: filepath.Join(i.l.Credentials(), provider.Name)}
	i.inst.Harnesses = append(i.inst.Harnesses, h)
	i.selected = &i.inst.Harnesses[len(i.inst.Harnesses)-1]
	return nil
}

func (i *installer) prepareCertificates(context.Context) error {
	store, err := pki.Open(i.l.PKI(), pki.DefaultPolicy, nil)
	if err != nil {
		return err
	}
	names := pki.Names{IPs: []string{i.inst.Endpoint.GatewayIPv4}}
	for _, h := range i.inst.Harnesses {
		names.DNS = append(names.DNS, inspectionHosts(h)...)
	}
	if _, err = store.State(); errors.Is(err, pki.ErrNotInitialized) {
		// Create the authority only on the first run. A journal that already
		// completed this stage means the state was lost, not never made.
		if _, done := i.inst.Stages["pki"]; done {
			return fail("tls", errors.New("the certificate authority's state is missing"),
				"restore "+i.l.PKI()+" from a backup, or run sproozi certificates replace-ca")
		}
		if err = store.Initialize(names); err != nil {
			return fail("tls", err, "run sproozi certificates status")
		}
	}
	if _, err = store.Ensure(names); err != nil {
		return fail("tls", err, "run sproozi certificates status")
	}
	i.store = store
	i.step("Private gateway CA and serving certificate are in %s", i.l.PKI())
	return i.save("pki")
}

// writeConfiguration writes a new configuration, or merges a new harness into an existing
// one while keeping the user's edits.
func (i *installer) writeConfiguration(context.Context) error {
	existing, found, err := readExistingConfig(i.l)
	if err != nil {
		return err
	}
	rendered := renderConfig(i.l, i.inst, i.o.Instructions)
	i.config, i.changed = rendered, true
	if found {
		if i.config, i.changed, err = mergeConfig(existing, rendered); err != nil {
			return err
		}
		if i.providerCA != "" && setProviderCA(&i.config, i.selected.Harness, i.providerCA) {
			i.changed = true
		}
	}
	if i.changed {
		if err = writeConfig(i.l, i.config, found); err != nil {
			return err
		}
		i.step("Wrote %s", i.l.Config())
	}
	if _, err = home.ReadConfig(i.l.Config()); err != nil {
		return fail("config", err, "fix "+i.l.Config())
	}
	return i.save("config")
}

// serve starts or restarts the home daemon, then proves each harness's
// restricted worker path before reporting ready.
func (i *installer) serve(ctx context.Context) (Result, error) {
	l, inst := i.l, i.inst
	result := Result{Root: l.Root, Config: l.Config(), Profile: l.Profile(), Installation: inst.ID, Image: *inst.Image,
		Network: *inst.Network, Endpoint: *inst.Endpoint, Harnesses: inst.Harnesses}
	if !i.o.StartService {
		result.Service = ServiceState(ctx, l, i.config)
		return result, nil
	}
	if i.changed || i.credential {
		restarted, err := RestartIdle(ctx, l, i.config, i.o.Executable)
		var active *SessionsActiveError
		if errors.As(err, &active) {
			// Setup is saved; the running service adopts it after a restart,
			// so the readiness probes would test the previous settings.
			result.Service, result.Warning = ServiceState(ctx, l, i.config), err.Error()
			return result, nil
		}
		if err != nil {
			result.Service = ServiceState(ctx, l, i.config)
			return result, err
		}
		if restarted {
			i.step("Restarted the service to load the new configuration and credentials")
		}
	}
	var err error
	if result.Service, err = StartService(ctx, l, i.config, i.o.Executable, 2*time.Minute); err != nil {
		return result, err
	}
	i.step("The service is running; log %s", l.ServiceLog())
	if err = i.save("service"); err != nil {
		return result, err
	}
	trust, err := privatefs.ReadFile(i.store.TrustPath(), 64<<10)
	if err != nil {
		return result, err
	}
	for _, h := range inst.Harnesses {
		report, err := ProbePath(ctx, i.docker, inst.ID, *inst.Image, *inst.Network, inst.Endpoint.GatewayIPv4, h.Port, trust)
		result.Paths = append(result.Paths, report)
		if err != nil {
			return result, err
		}
		i.step("Verified %s gateway TLS and denied direct paths from a restricted worker", h.Harness)
	}
	result.Ready = true
	return result, i.save("ready")
}

func step(o Options, format string, args ...any) {
	_, _ = fmt.Fprintf(o.Progress, "• "+format+"\n", args...)
}

// inspectionHosts are the provider hosts a harness's gateway inspects for
// its mechanism, such as Copilot's plan and GitHub hosts when it signs in.
func inspectionHosts(h Harness) []string {
	registered, _ := harness.Managed(h.Harness)
	mechanism := h.Mechanism()
	p, _ := model.ProviderFor(registered.Provider(mechanism != harness.CredentialKey))
	return p.Hosts(mechanism == harness.CredentialSignIn || p.Models[0].SignedIn)
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

// Default local limits, from the onboarding proposal's starter values. The
// checkpoint limit also bounds a workspace's expanded Git objects, so it is
// 32 MiB rather than the proposal's 8 MiB; see ADR 0029.
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
	codexTrust,
}

// codexTrust trusts the session workspace. A Codex that signs in itself keeps
// only this option and uses its built-in OpenAI provider, which reaches the
// ChatGPT backend or the API to match its sign-in (ADR 0047).
const codexTrust = `projects."/workspace/session/workspace".trust_level="trusted"`

// MCPAuthority is the name workers use for the gateway's MCP endpoint, which
// always serves the Sproozi control connection. The home's certificate
// authority issues the gateway a certificate that covers it.
const MCPAuthority = "mcp.sproozi.internal:443"

func renderConfig(l Layout, inst Installation, instructions string) home.Config {
	primary := inst.Harnesses[0]
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(instructions)))
	c := home.Config{
		Directory: l.State(), DockerBinary: inst.Docker.Binary, DockerSocket: inst.Docker.Socket, DockerConfig: l.DockerCLI(),
		Local: runtimedocker.PlacementConfig{Harness: primary.Harness, Name: primary.Name,
			NativeImageID: inst.Image.ID, HelperImageID: inst.Image.ID, AnchorImageID: inst.Image.ID,
			NetworkName: inst.Network.Name, GatewayIPv4: inst.Endpoint.GatewayIPv4, GatewayPort: primary.Port,
			Instructions: instructions, InstructionDigest: digest,
			AgentLimits: agentLimits, HelperLimits: helperLimits, AnchorLimits: anchorLimits,
			VolumeBytes: 512 << 20, MaxCheckpointBytes: 32 << 20, RetainedBytes: 256 << 20},
		Allowances:  map[string]session.Allowance{"model.inference": {Units: session.Limit{Bounded: true, Value: 200000}}},
		MaxDuration: "30m", Model: primary.Model, NativeOptions: nativeOptions(primary.Harness, primary.Credential == ""),
		Gateway: home.GatewayConfig{Listen: fmt.Sprintf("%s:%d", inst.Endpoint.ListenHost, primary.Port), PKIDirectory: l.PKI(),
			ProviderURL: primary.ProviderURL, ProviderTokenFile: primary.Credential, ProviderTokenKind: tokenKind(primary),
			ProviderCAFile: primary.ProviderCAFile,
			MCPAuthority:   MCPAuthority},
	}
	for _, h := range inst.Harnesses[1:] {
		c.Harnesses = append(c.Harnesses, home.LocalHarness{Harness: h.Harness, Name: h.Name, GatewayPort: h.Port, Model: h.Model,
			NativeOptions: nativeOptions(h.Harness, h.Credential == ""), ProviderURL: h.ProviderURL, ProviderTokenFile: h.Credential,
			ProviderTokenKind: tokenKind(h), ProviderCAFile: h.ProviderCAFile})
	}
	return c
}

// tokenKind marks a stored sign-in token for the gateway.
func tokenKind(h Harness) string {
	if h.Credential == "" {
		return ""
	}
	return h.Kind
}

func nativeOptions(kind harness.NativeKind, signIn bool) []string {
	if kind == harness.NativeCodex && signIn {
		return []string{codexTrust}
	}
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
	// The existing approved instructions are kept; only an explicit
	// different request conflicts with them.
	if rendered.Local.Instructions != DefaultInstructions && rendered.Local.Instructions != existing.Local.Instructions {
		return existing, false, fail("config", errors.New("the installation already has different approved instructions"),
			"edit them in the configuration before the first session, or omit --instructions-file")
	}
	if !reflect.DeepEqual(pinned(existing), pinned(rendered)) {
		return existing, false, fail("config", errors.New("the existing configuration's placement differs from this installation"),
			"run sproozi doctor; changing images, network or gateway address needs a drained migration")
	}
	changed := false
	if existing.Gateway.MCPAuthority == "" {
		existing.Gateway.MCPAuthority, changed = rendered.Gateway.MCPAuthority, true
	}
	for _, h := range rendered.Harnesses {
		if !slices.ContainsFunc(existing.Harnesses, func(e home.LocalHarness) bool { return e.Harness == h.Harness }) {
			existing.Harnesses = append(existing.Harnesses, h)
			changed = true
		}
	}
	return existing, syncCredentialMode(&existing, rendered) || changed, nil
}

// syncCredentialMode follows a harness's switch between a gateway-held key and
// its own sign-in (ADR 0047), keeping an edited key file path otherwise.
func syncCredentialMode(c *home.Config, rendered home.Config) bool {
	c.Harnesses = slices.Clone(c.Harnesses)
	changed := false
	follow := func(current *string, want string) {
		if (*current == "") != (want == "") {
			*current, changed = want, true
		}
	}
	// Native options still at the previous mode's defaults switch with it.
	switchOptions := func(kind harness.NativeKind, current *[]string, signIn bool) {
		if previous := nativeOptions(kind, !signIn); slices.Equal(*current, previous) && !slices.Equal(previous, nativeOptions(kind, signIn)) {
			*current, changed = nativeOptions(kind, signIn), true
		}
	}
	if signIn := rendered.Gateway.ProviderTokenFile == ""; (c.Gateway.ProviderTokenFile == "") != signIn {
		switchOptions(c.Local.Harness, &c.NativeOptions, signIn)
	}
	follow(&c.Gateway.ProviderTokenFile, rendered.Gateway.ProviderTokenFile)
	if c.Gateway.ProviderTokenKind != rendered.Gateway.ProviderTokenKind {
		c.Gateway.ProviderTokenKind, changed = rendered.Gateway.ProviderTokenKind, true
	}
	for _, r := range rendered.Harnesses {
		if i := slices.IndexFunc(c.Harnesses, func(e home.LocalHarness) bool { return e.Harness == r.Harness }); i >= 0 {
			if signIn := r.ProviderTokenFile == ""; (c.Harnesses[i].ProviderTokenFile == "") != signIn {
				switchOptions(r.Harness, &c.Harnesses[i].NativeOptions, signIn)
			}
			follow(&c.Harnesses[i].ProviderTokenFile, r.ProviderTokenFile)
			if c.Harnesses[i].ProviderTokenKind != r.ProviderTokenKind {
				c.Harnesses[i].ProviderTokenKind, changed = r.ProviderTokenKind, true
			}
		}
	}
	return changed
}

// setProviderCA points the harness's gateway at a stored CA bundle and
// reports whether that changed the configuration.
func setProviderCA(c *home.Config, kind harness.NativeKind, path string) bool {
	target := &c.Gateway.ProviderCAFile
	if c.Local.Harness != kind {
		index := slices.IndexFunc(c.Harnesses, func(h home.LocalHarness) bool { return h.Harness == kind })
		if index < 0 {
			return false
		}
		target = &c.Harnesses[index].ProviderCAFile
	}
	changed := *target != path
	*target = path
	return changed
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

package harness

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// Harness is one registered agent program and every fact Sproozi's Go code
// needs about it. The registry below is the single list of harnesses: the
// AgentRun tier is every entry, the managed tier is the entries with a
// Native kind. Guard tests compare it with the CRD enum and with each
// package that still keeps its own per-harness code, and name the file to
// change. docs/contributing/add-a-harness.md is the checklist.
type Harness struct {
	// Selector is the AgentRuntime spec.clientConfig.harness value.
	Selector string
	// MCP renders the run's MCP connections, including the sproozi control
	// connection, in the harness's own configuration format. Every harness
	// has one: ADR 0039 requires an MCP client.
	MCP MCPFormat
	// Loader says what reads MCP.File: the stock client itself, or an MCP
	// bridge shipped as one of LaunchFiles.
	Loader MCPLoader
	// LaunchFiles are extra contract files, by ConfigMap key, that the
	// harness's launch command reads. Each is a file in
	// internal/harness/launchfiles, embedded in the binary and shipped in the
	// image build context; use launchFilesNamed.
	LaunchFiles map[string]string
	// RequestFiles adds instructions.txt and request.json to the contract for
	// launch commands that read the request from files.
	RequestFiles bool
	// RunKeyEnv is the variable in which an AgentRun Pod's workload
	// containers get the public key placeholder, for a client that reads its
	// key from the Pod's environment. Empty when the harness's launch file or
	// runtime command sets its own placeholder: the Pod then gets none. A
	// runtime with the harness's own sign-in gets none either way (ADR 0047).
	RunKeyEnv string
	// Native is the managed-session kind, or empty when the harness runs only
	// as an AgentRun.
	Native NativeKind
	// NativeDirectory is the managed kind's native home under
	// /workspace/session and in checkpoints.
	NativeDirectory string
	// Protocol is the model API the managed kind speaks through the gateway.
	Protocol ModelProtocol
	// SignInFile is where the stock client keeps its own sign-in, relative to
	// NativeDirectory (ADR 0047). The placement persists only this file when
	// the harness signs in itself; the provider's routes are shared by every
	// harness that uses it.
	SignInFile string
	// SignInProvider is the model provider a gateway-held sign-in token or
	// the harness's own sign-in reaches, when it isn't Protocol's: Copilot
	// calls OpenAI with an API key but GitHub's Copilot service when signed
	// in. Empty means Protocol.
	SignInProvider string
	// StateProvesCompletion: the native state a checkpoint captures proves
	// its last turn completed, as Codex's rollout and Copilot's journal do,
	// so a stopped or exited process is enough evidence. Without it, as for
	// Claude Code, a stop needs the supervisor's acknowledgement.
	StateProvesCompletion bool

	// The facts below are required for every managed harness (a Native kind);
	// the guard tests fail on an entry that leaves one out, so a new managed
	// harness never inherits another's value.

	// DisplayName is how messages name the harness, such as "Claude Code".
	DisplayName string
	// Product is the vendor's name for the program, shown beside the selector
	// when a person chooses a harness.
	Product string
	// Executable is the stock client's path inside the managed sandbox image.
	Executable string
	// InstructionsFile is the user-level instructions file name inside the
	// native home, and the name of the harness's profile instructions file.
	InstructionsFile string
	// KeyEnv is the environment variable that carries the public placeholder
	// where the gateway holds the harness's API key or sign-in token.
	KeyEnv string
	// RefusedFlags are the command-line options Sproozi controls; a user's
	// harness arguments may not set them (see ValidateNativeArgs).
	RefusedFlags []controlledFlag
	// ReservedEnv are the variables the supervisor sets for this harness and a
	// launch's explicit environment may not replace, besides the fixed ones
	// (HOME, PWD, KUBECONFIG). A harness may declare none only with a reason in
	// the guard test.
	ReservedEnv []string
	// RemoteExecutor reports that a remote executor configured for this
	// harness may start work for it: tasks and interactive sessions the cluster
	// owns. Its one reader is the executor's work catalogue
	// (internal/work/owner.go). A session of any managed kind moved to an
	// executor for its harness still runs there; the placement handler
	// (internal/session/profile) accepts every managed kind. A Claude Code
	// task's completion is bound to its prompt's turn by the supervisor's
	// UserPromptSubmit hook (ADR 0057).
	RemoteExecutor bool
	// CaptureState captures the kind's conversation from a stopped native
	// home, for the workspace at workspace, as the ConversationState a checkpoint
	// carries (internal/checkpoint).
	CaptureState func(home, workspace, threadID string, maxBytes int64) (ConversationState, error)

	// TakesTask: the supervisor submits a task as the launch's prompt and
	// binds completion to that prompt's turn: Codex by the task text in its
	// rollout, Claude Code by its UserPromptSubmit hook. False means the
	// harness is interactive only.
	TakesTask bool
	// Default marks the one managed harness Sproozi uses when none is named.
	Default bool

	// TokenEnv and TokenPlaceholder replace KeyEnv, and its placeholder, when
	// the gateway holds a sign-in token. Empty means the token travels in
	// KeyEnv, as for Codex and Claude Code.
	TokenEnv         string
	TokenPlaceholder string
	// SignInYieldsGitHubToken: the harness prefers GH_TOKEN to its own sign-in,
	// so a launch that signs in itself gets no GH_TOKEN placeholder in its
	// environment (its supervisor gives it to the agent's shell only).
	SignInYieldsGitHubToken bool
	// KeyOptions and SignInOptions are the native options a local installation
	// gives the harness: when the gateway holds its credential, and when it
	// signs in itself. Both may be empty.
	KeyOptions    []string
	SignInOptions []string
	// SignInChoices are the ways the setup wizard (cmd/sproozi/wizard.go)
	// offers for the harness to reach its model provider, most common first
	// (docs/guides/local-sessions.md#choose-how-an-agent-signs-in).
	SignInChoices []SignInChoice
}

// SignInChoice is one answer to the setup wizard's sign-in question.
type SignInChoice struct {
	// Label is the answer's wording.
	Label string
	// Answer is how the harness then reaches its provider.
	Answer SignInAnswer
}

// SignInAnswer is a way the setup wizard offers for a harness to reach its
// model provider; each is one of its flags.
type SignInAnswer string

const (
	// AnswerKey: an API key the gateway holds (no flag).
	AnswerKey SignInAnswer = "key"
	// AnswerChatGPT: the gateway signs in to a ChatGPT plan during setup and
	// holds that ChatGPT credential (--chatgpt).
	AnswerChatGPT SignInAnswer = "chatgpt"
	// AnswerToken: a sign-in token the gateway holds (--token).
	AnswerToken SignInAnswer = "token"
	// AnswerSignIn: the harness signs in itself (--sign-in).
	AnswerSignIn SignInAnswer = "sign-in"
)

// ProvesCompletion reports the kind's StateProvesCompletion.
func ProvesCompletion(kind NativeKind) bool {
	h, err := Managed(kind)
	return err == nil && h.StateProvesCompletion
}

// Provider is the model provider this harness reaches with an API key
// (signedIn false) or with a sign-in token or its own sign-in (signedIn true).
func (h Harness) Provider(signedIn bool) string {
	if signedIn && h.SignInProvider != "" {
		return h.SignInProvider
	}
	return string(h.Protocol)
}

// MCPFormat is a harness's MCP configuration file. A JSON file holds a map,
// under Field, from connection name to Entry's value; an empty Field selects
// Codex's TOML tables.
type MCPFormat struct {
	File  string
	Field string
	Entry func(name, address string) (string, any)
}

// MCPLoader declares how a harness loads its rendered MCP configuration.
// Exactly one of Native and Bridge is set.
type MCPLoader struct {
	// Native means the stock client reads MCP.File itself.
	Native bool
	// Bridge names the launch file, a key of LaunchFiles, that reads MCP.File
	// and exposes its connections to a client without its own MCP support.
	Bridge string
	// Runtime is the command that runs Bridge, such as ["node"]. The bridge
	// test hook runs it as Runtime... Bridge --sproozi-check CONFIG.
	Runtime []string
}

// NativeMCP is the loader of a client that reads its MCP file itself.
var NativeMCP = MCPLoader{Native: true}

// ModelProtocol names the provider API a managed harness calls.
type ModelProtocol string

const (
	ProtocolOpenAI    ModelProtocol = "openai"
	ProtocolAnthropic ModelProtocol = "anthropic"
)

// InspectionHost is the provider host the gateway inspects for the protocol.
func (p ModelProtocol) InspectionHost() string {
	if p == ProtocolAnthropic {
		return "api.anthropic.com"
	}
	return "api.openai.com"
}

// codexKeyOptions point stock Codex at the OpenAI Responses API through the
// gateway with the worker's placeholder key, and trust the managed workspace.
var codexKeyOptions = []string{
	`model_provider="sproozi"`,
	`model_providers.sproozi.name="OpenAI through Sproozi"`,
	`model_providers.sproozi.base_url="https://api.openai.com/v1"`,
	`model_providers.sproozi.env_key="` + nativeOpenAIKey + `"`,
	`model_providers.sproozi.wire_api="responses"`,
	`model_providers.sproozi.supports_websockets=false`,
	codexTrust,
}

// codexTrust trusts the session workspace. A Codex that signs in itself keeps
// only this option and uses its built-in OpenAI provider, which reaches the
// ChatGPT backend or the API to match its sign-in (ADR 0047).
const codexTrust = `projects."/workspace/session/workspace".trust_level="trusted"`

// The variables that carry the OpenAI and Anthropic placeholder keys, as the
// entries below name them.
const (
	nativeOpenAIKey    = "OPENAI_API_KEY"
	nativeAnthropicKey = "ANTHROPIC_API_KEY"
)

// NativeCopilotTokenPlaceholder stands in for a GitHub token the gateway holds:
// Copilot needs one to call its plan's service, and the gateway replaces it.
const NativeCopilotTokenPlaceholder = "gho_sprooziworkerplaceholder0000000000"

// mcpServersField is Claude Code's MCP configuration key.
const mcpServersField = "mcpServers"

var registry = []Harness{
	{Selector: string(NativeCodex), Native: NativeCodex, NativeDirectory: string(NativeCodex), Protocol: ProtocolOpenAI,
		SignInFile: "auth.json", StateProvesCompletion: true,
		DisplayName: "Codex", Product: "OpenAI Codex CLI", Executable: "/usr/local/bin/codex", InstructionsFile: "AGENTS.md",
		KeyEnv: nativeOpenAIKey, RunKeyEnv: nativeOpenAIKey, RefusedFlags: codexControlled, ReservedEnv: []string{"CODEX_HOME"},
		RemoteExecutor: true, TakesTask: true, Default: true, KeyOptions: codexKeyOptions, SignInOptions: []string{codexTrust},
		CaptureState: captureCodex,
		SignInChoices: []SignInChoice{
			{Label: "An OpenAI API key, held by the gateway", Answer: AnswerKey},
			{Label: "A ChatGPT plan, signed in now in your browser and held by the gateway",
				Answer: AnswerChatGPT},
			{Label: "Codex signs in itself with ChatGPT in its first session", Answer: AnswerSignIn},
		},
		MCP: MCPFormat{File: "codex-mcp.toml"}, Loader: NativeMCP},
	{Selector: string(NativeClaude), Native: NativeClaude, NativeDirectory: "claude", Protocol: ProtocolAnthropic,
		SignInFile:  ".credentials.json",
		DisplayName: "Claude Code", Product: "Anthropic Claude Code", Executable: "/usr/local/bin/claude", InstructionsFile: "CLAUDE.md",
		KeyEnv: nativeAnthropicKey, RefusedFlags: claudeControlled, RemoteExecutor: true, TakesTask: true,
		ReservedEnv: []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_AUTOUPDATER",
			"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS"},
		SignInChoices: []SignInChoice{
			{Label: "An Anthropic API key, held by the gateway", Answer: AnswerKey},
			{Label: `A sign-in token from "claude setup-token", held by the gateway`, Answer: AnswerToken},
			{Label: "Claude Code signs in itself with /login in its first session", Answer: AnswerSignIn},
		},
		CaptureState: captureClaude,
		RequestFiles: true, Loader: NativeMCP, LaunchFiles: launchFilesNamed("claude-launch.py"),
		MCP: MCPFormat{File: "claude-mcp.json", Field: mcpServersField, Entry: func(name, address string) (string, any) {
			return name, map[string]any{connectionType: httpTransport, connectionURL: address}
		}}},
	{Selector: "opencode", RequestFiles: true, Loader: NativeMCP, LaunchFiles: launchFilesNamed("opencode-launch.py"),
		MCP: MCPFormat{File: "opencode-mcp.json", Field: "mcp", Entry: func(name, address string) (string, any) {
			return name, map[string]any{connectionType: remoteName, connectionURL: address, "enabled": true, "oauth": false}
		}}},
	{Selector: string(NativeCopilot), Native: NativeCopilot, NativeDirectory: string(NativeCopilot),
		Protocol: ProtocolOpenAI, SignInProvider: "copilot", SignInFile: "config.json", StateProvesCompletion: true,
		DisplayName: "Copilot", Product: "GitHub Copilot CLI", Executable: "/usr/local/bin/copilot",
		InstructionsFile: "copilot-instructions.md", KeyEnv: "COPILOT_PROVIDER_API_KEY", RefusedFlags: copilotControlled,
		TokenEnv: "COPILOT_GITHUB_TOKEN", TokenPlaceholder: NativeCopilotTokenPlaceholder, SignInYieldsGitHubToken: true,
		SignInChoices: []SignInChoice{
			{Label: "Copilot signs in itself with /login and your Copilot plan in its first session",
				Answer: AnswerSignIn},
			{Label: "A GitHub token with Copilot access, held by the gateway", Answer: AnswerToken},
			{Label: "An OpenAI API key, held by the gateway (no Copilot plan needed)", Answer: AnswerKey},
		},
		CaptureState: captureCopilot,
		RequestFiles: true, Loader: NativeMCP,
		MCP: MCPFormat{File: "copilot-mcp.json", Field: mcpServersField, Entry: func(name, address string) (string, any) {
			return name, map[string]any{connectionType: httpTransport, connectionURL: address, "tools": []string{"*"}}
		}}},
	{Selector: "hermes", RequestFiles: true, Loader: NativeMCP, LaunchFiles: launchFilesNamed("hermes-launch.py"),
		MCP: MCPFormat{File: "hermes-mcp.json", Field: "mcp_servers", Entry: func(name, address string) (string, any) {
			// Native toolset aliases must not collide with built-ins such as all or terminal.
			return "sproozi-mcp-" + name, map[string]any{connectionURL: address, "enabled": true,
				"strict_redirect_headers": true, "connect_timeout": 15, "timeout": 60}
		}}},
}

// Default is the managed kind Sproozi uses when none is named: the entry
// marked Default.
func Default() NativeKind {
	for _, h := range registry {
		if h.Default {
			return h.Native
		}
	}
	return ""
}

// TaskKindList names the kinds that take a task for a message: "a or b".
func TaskKindList() string {
	var names []string
	for _, h := range registry {
		if h.Native != "" && h.TakesTask {
			names = append(names, h.DisplayName)
		}
	}
	return strings.Join(names, " or ")
}

// RemoteExecutorKind reports that a remote executor may run the kind.
func RemoteExecutorKind(kind NativeKind) bool {
	h, err := Managed(kind)
	return err == nil && h.RemoteExecutor
}

// Lookup returns the registered harness for an AgentRuntime selector.
func Lookup(selector string) (Harness, error) {
	i := slices.IndexFunc(registry, func(h Harness) bool { return h.Selector == selector })
	if i < 0 {
		return Harness{}, fmt.Errorf("unsupported harness %q; registered harnesses are %s", selector,
			strings.Join(Selectors(), ", "))
	}
	return registry[i], nil
}

// Selectors lists every registered selector: the AgentRun tier.
func Selectors() []string {
	out := make([]string, 0, len(registry))
	for _, h := range registry {
		out = append(out, h.Selector)
	}
	return out
}

// ManagedKinds lists the kinds of the managed tier, in registry order.
func ManagedKinds() []NativeKind {
	var out []NativeKind
	for _, h := range registry {
		if h.Native != "" {
			out = append(out, h.Native)
		}
	}
	return out
}

// ManagedKindNames lists the managed kinds as strings, in registry order.
func ManagedKindNames() []string {
	kinds := ManagedKinds()
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, string(kind))
	}
	return out
}

// ManagedKindList names the managed kinds for a message: "a, b or c".
func ManagedKindList() string {
	names := ManagedKindNames()
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// Managed returns the registered harness of a managed kind.
func Managed(kind NativeKind) (Harness, error) {
	i := slices.IndexFunc(registry, func(h Harness) bool { return h.Native != "" && h.Native == kind })
	if i < 0 {
		return Harness{}, fmt.Errorf("unsupported native harness %q", kind)
	}
	return registry[i], nil
}

// MCPConfigAt renders a harness's MCP file for the named connections and the
// sproozi control connection at base+"/mcp/<name>", such as a local fake MCP
// server, so a person can check that a real client loads it.
func MCPConfigAt(selected, base string, names ...string) (map[string]string, error) {
	h, err := Lookup(selected)
	if err != nil {
		return nil, err
	}
	names = append(slices.Clone(names), api.ControlMCPServer)
	return h.MCP.render(names, func(name string) string { return base + "/mcp/" + name })
}

// render writes connections, by name and address, in the harness's format.
func (f MCPFormat) render(names []string, address func(string) string) (map[string]string, error) {
	if f.Field == "" {
		var output strings.Builder
		for _, name := range names {
			_, _ = fmt.Fprintf(&output, "[mcp_servers.%s]\nurl = %s\nrequired = true\n\n", strconv.Quote(name),
				strconv.Quote(address(name)))
		}
		return map[string]string{f.File: output.String()}, nil
	}
	if f.Entry == nil || f.File == "" {
		return nil, fmt.Errorf("harness MCP format %q has no entry renderer", f.File)
	}
	connections := make(map[string]any, len(names))
	for _, name := range names {
		key, value := f.Entry(name, address(name))
		connections[key] = value
	}
	document, err := jsonDocument(map[string]any{f.Field: connections})
	if err != nil {
		return nil, err
	}
	return map[string]string{f.File: document}, nil
}

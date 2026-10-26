package harness

import (
	"fmt"
	"slices"
	"strings"
)

// Harness arguments are the user's own command-line arguments for one
// execution's stock harness. The supervisor appends them after the flags it
// sets itself, so a later repeat of a flag it owns could replace its value. The
// flags below are those Sproozi must keep: they select the workspace, sandbox
// and approval mode, provider routing, trusted instructions, the native
// conversation and the lifecycle hooks completion depends on. They are refused;
// every other argument reaches the harness unchanged. The gateway enforces
// network access and credentials whatever the harness is told.
const (
	// NativeArgsMax bounds how many harness arguments one execution carries.
	NativeArgsMax = 64
	// NativeArgMaxBytes bounds one harness argument.
	NativeArgMaxBytes = 4096
	// nativeModelFlag is the model option every managed harness takes.
	nativeModelFlag = "--model"
	// remoteName is Codex's and Copilot's remote-session flag and OpenCode's
	// remote MCP transport.
	remoteName = "remote"
)

// controlledFlag names one option a harness takes. value reports that it takes
// a value, either as the next argument or after "=" (or, for a short name,
// joined to it).
type controlledFlag struct {
	long  []string
	short string
	value bool
	why   string
}

const (
	whyApproval  = "Sproozi runs the harness unattended with its own sandbox and approval mode"
	whyWorkspace = "Sproozi fixes the managed workspace directory"
	whyProvider  = "Sproozi routes the model provider through its gateway"
	whyThread    = "Sproozi selects the native conversation to start or resume"
	whySetting   = "Sproozi supplies this setting itself"
	whyLifecycle = "Sproozi observes the interactive terminal and its lifecycle hooks to stop the session safely"
)

var codexControlled = []controlledFlag{
	{long: []string{"sandbox"}, short: "s", value: true, why: whyApproval},
	{long: []string{"ask-for-approval"}, short: "a", value: true, why: whyApproval},
	{long: []string{"dangerously-bypass-approvals-and-sandbox", "yolo"}, why: whyApproval},
	{long: []string{"approve-for-me", "not-so-yolo"}, why: whyApproval},
	{long: []string{"full-auto"}, why: whyApproval},
	{long: []string{"cd"}, short: "C", value: true, why: whyWorkspace},
	{long: []string{"worktree"}, why: whyWorkspace},
	{long: []string{"oss"}, why: whyProvider},
	{long: []string{"local-provider"}, value: true, why: whyProvider},
	{long: []string{"profile"}, short: "p", value: true, why: whyProvider},
	{long: []string{remoteName, "remote-auth-token-env"}, value: true, why: whyLifecycle},
	{long: []string{"no-alt-screen", "no-daemon"}, why: whySetting},
}

// codexControlledKeys are configuration keys a -c/--config override may not
// set, with every key below them.
var codexControlledKeys = []string{
	"developer_instructions", "instructions", "model_instructions_file", "experimental_instructions_file",
	"model_provider", "model_providers", "mcp_servers.sproozi", "projects", "sandbox_mode",
	"sandbox_workspace_write", "approval_policy", "approvals_reviewer", "check_for_update_on_startup",
	"profile", "profiles", "notice",
}

var claudeControlled = []controlledFlag{
	{long: []string{"permission-mode", "permission-prompts"}, value: true, why: whyApproval},
	{long: []string{"dangerously-skip-permissions", "allow-dangerously-skip-permissions"}, why: whyApproval},
	{long: []string{"tools", "allowedTools", "allowed-tools"}, value: true, why: whySetting},
	{long: []string{"mcp-config", "plugin-dir", "plugin-url", "settings", "setting-sources"}, value: true, why: whySetting},
	{long: []string{"strict-mcp-config", "bare", "safe-mode", "restricted"}, why: whySetting},
	{long: []string{"system-prompt", "system-prompt-file", "append-system-prompt", "append-system-prompt-file"},
		value: true, why: whySetting},
	{long: []string{"resume"}, short: "r", value: true, why: whyThread},
	{long: []string{"continue"}, short: "c", why: whyThread},
	{long: []string{"session-id", "from-pr", "teleport"}, value: true, why: whyThread},
	{long: []string{"fork-session", "no-session-persistence"}, why: whyThread},
	{long: []string{"print"}, short: "p", why: whyLifecycle},
	{long: []string{"output-format", "input-format", "remote-control", "cloud", "environment"}, value: true,
		why: whyLifecycle},
	{long: []string{"bg", "background", "desktop", "tmux"}, why: whyLifecycle},
	{long: []string{"worktree"}, short: "w", value: true, why: whyWorkspace},
}

var copilotControlled = []controlledFlag{
	{long: []string{"allow-all", "yolo", "allow-all-tools", "allow-all-paths", "allow-all-urls"}, why: whyApproval},
	{long: []string{"additional-mcp-config", "disable-mcp-server", "mcp-github-auth", "plugin-dir", "bash-env"},
		value: true, why: whySetting},
	{long: []string{"disable-builtin-mcps", "enable-all-github-mcp-tools", "no-custom-instructions", "no-auto-update",
		"no-bash-env", "trust-folder"}, why: whySetting},
	{long: []string{"add-github-mcp-tool", "add-github-mcp-toolset"}, value: true, why: whySetting},
	{long: []string{"resume"}, short: "r", value: true, why: whyThread},
	{long: []string{"continue"}, why: whyThread},
	{long: []string{"session-id", "connect"}, value: true, why: whyThread},
	{long: []string{"prompt"}, short: "p", value: true, why: whyLifecycle},
	{long: []string{"output-format", "stream", "share"}, value: true, why: whyLifecycle},
	{long: []string{"acp", remoteName, "remote-export", "silent", "share-gist"}, short: "s", why: whyLifecycle},
}

// ValidateNativeArgs refuses harness arguments that would replace a flag
// Sproozi controls, with a message naming the flag and why. A refusal is
// explicit; Sproozi never rewrites the user's arguments.
func ValidateNativeArgs(kind NativeKind, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if err := kind.Validate(); err != nil {
		return err
	}
	if len(args) > NativeArgsMax {
		return fmt.Errorf("harness arguments: at most %d are accepted", NativeArgsMax)
	}
	for _, arg := range args {
		if len(arg) > NativeArgMaxBytes || strings.ContainsAny(arg, "\x00") {
			return fmt.Errorf("harness arguments: each must be at most %d bytes without NUL", NativeArgMaxBytes)
		}
	}
	controlled := codexControlled
	switch kind {
	case NativeClaude:
		controlled = claudeControlled
	case NativeCopilot:
		controlled = copilotControlled
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			// The supervisor adds the conversation to resume after these
			// arguments; a bare "--" would turn it into positional input.
			return fmt.Errorf("harness argument %q is refused: Sproozi adds the conversation to resume after "+
				"the harness arguments; remove it", arg)
		}
		if kind == NativeCodex {
			key, consumed, ok := codexConfigKey(args, i)
			if ok {
				if refused := controlledKey(key); refused != "" {
					return fmt.Errorf("harness argument %q is refused: Sproozi sets %q for %s sessions; remove it",
						strings.Join(args[i:i+consumed+1], " "), refused, kind)
				}
				i += consumed
				continue
			}
		}
		for _, f := range controlled {
			if name, ok := f.matches(arg); ok {
				return fmt.Errorf("harness argument %q is refused: %s for %s sessions; remove %s", arg, f.why, kind, name)
			}
		}
	}
	return nil
}

// NativeArgsSelectModel reports that the user's harness arguments choose the
// model, so the configured default model is left out of the launch.
func NativeArgsSelectModel(kind NativeKind, args []string) bool {
	for i, arg := range args {
		if arg == nativeModelFlag || strings.HasPrefix(arg, nativeModelFlag+"=") ||
			kind == NativeCodex && (arg == "-m" || strings.HasPrefix(arg, "-m")) {
			return true
		}
		if kind == NativeCodex {
			if key, _, ok := codexConfigKey(args, i); ok && key == "model" {
				return true
			}
		}
	}
	return false
}

// matches reports whether arg is this option, in any of its spellings, and
// the spelling it used.
func (f controlledFlag) matches(arg string) (string, bool) {
	for _, name := range f.long {
		if arg == "--"+name || f.value && strings.HasPrefix(arg, "--"+name+"=") {
			return "--" + name, true
		}
	}
	if f.short != "" && !strings.HasPrefix(arg, "--") &&
		(arg == "-"+f.short || f.value && strings.HasPrefix(arg, "-"+f.short)) {
		return "-" + f.short, true
	}
	return "", false
}

// codexConfigKey reads the key of a Codex -c/--config override at args[i]
// and how many further arguments it consumed.
func codexConfigKey(args []string, i int) (string, int, bool) {
	arg := args[i]
	var value string
	consumed := 0
	switch {
	case arg == "-c" || arg == "--config":
		if i+1 >= len(args) {
			return "", 0, false
		}
		value, consumed = args[i+1], 1
	case strings.HasPrefix(arg, "--config="):
		value = strings.TrimPrefix(arg, "--config=")
	case strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--"):
		value = strings.TrimPrefix(arg, "-c")
	default:
		return "", 0, false
	}
	key, _, _ := strings.Cut(value, "=")
	return normalizeConfigKey(key), consumed, true
}

// normalizeConfigKey removes TOML quoting and spaces from a dotted key, so
// `"model_provider"` and `mcp_servers."sproozi".url` compare by name.
func normalizeConfigKey(key string) string {
	parts := strings.Split(strings.TrimSpace(key), ".")
	for i, part := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(part), `"'`)
	}
	return strings.Join(parts, ".")
}

func controlledKey(key string) string {
	if i := slices.IndexFunc(codexControlledKeys, func(c string) bool {
		return key == c || strings.HasPrefix(key, c+".")
	}); i >= 0 {
		return codexControlledKeys[i]
	}
	return ""
}

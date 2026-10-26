package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	copilotHome       = "/workspace/session/copilot"
	copilotExecutable = "/usr/local/bin/copilot"
	// copilotMCP and copilotBashEnv are launch-generated, outside the
	// workspace and portable state.
	copilotMCP     = "/tmp/sproozi-native/copilot-mcp.json"
	copilotBashEnv = "/tmp/sproozi-native/copilot-bash-env"
)

// prepareCopilotLaunch writes Copilot's launch files and returns its
// arguments (ADR 0048). thread is the session to start or resume: the
// supervisor names a new one itself, so it never adopts a session Copilot
// chose. Copilot keeps its login in config.json, so trust is merged into
// that file rather than replacing it.
func prepareCopilotLaunch(c config, thread string, env []string) ([]string, []string, error) {
	if thread == "" {
		return nil, nil, errors.New("copilot launch needs a session ID")
	}
	if err := os.MkdirAll(copilotHome, 0700); err != nil {
		return nil, nil, err
	}
	if err := trustCopilotWorkspace(filepath.Join(copilotHome, "config.json")); err != nil {
		return nil, nil, err
	}
	if err := linkCopilotApp(); err != nil {
		return nil, nil, err
	}
	profile, err := loadProfile(c)
	if err != nil {
		return nil, nil, fmt.Errorf("profile: %w", err)
	}
	mcp, err := c.ClientConfig.CopilotMCPConfig()
	if err != nil {
		return nil, nil, err
	}
	// Without a keychain Copilot asks before storing its own sign-in in
	// config.json; the login store is that file, so it may.
	settings, _ := json.Marshal(map[string]any{"storeTokenPlaintext": true})
	instructions := c.Instructions + "\n\n" + profileInstructions(profile, harness.NativeCopilot)
	if err = os.RemoveAll(filepath.Join(copilotHome, "skills")); err != nil {
		return nil, nil, err
	}
	if err = errors.Join(
		installManagedFile(filepath.Join(copilotHome, "settings.json"), settings, 0600),
		installManagedFile(filepath.Join(copilotHome, harness.NativeUserInstructionsFile(harness.NativeCopilot)),
			[]byte(instructions), 0400),
		installManagedSkill(copilotHome),
		installProfileSkills(copilotHome, profile),
		installManagedFile(copilotMCP, []byte(mcp), 0400)); err != nil {
		return nil, nil, err
	}
	env = append(env, "COPILOT_HOME="+copilotHome, "COPILOT_AUTO_UPDATE=false")
	args := []string{"--session-id", thread, "--allow-all", "--disable-builtin-mcps",
		"--additional-mcp-config", "@" + copilotMCP, "--no-auto-update"}
	model := c.Model
	switch {
	case slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "COPILOT_PROVIDER_API_KEY=") }):
		// A gateway-held key: bring-your-own-key mode on the OpenAI route,
		// offline from GitHub.
		env = append(env, "COPILOT_PROVIDER_BASE_URL=https://api.openai.com/v1", "COPILOT_PROVIDER_TYPE=openai",
			"COPILOT_PROVIDER_WIRE_API=responses", "COPILOT_OFFLINE=true")
		// Bring-your-own-key mode has no default model of its own.
		model = cmp.Or(model, copilotKeyModel)
	case !slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "COPILOT_GITHUB_TOKEN=") }):
		// Its own sign-in: Copilot prefers GH_TOKEN, or gh's own token, to
		// its /login, so only the agent's shell gets the gh placeholder.
		if err = installManagedFile(copilotBashEnv, []byte("export GH_TOKEN="+nativeGHPlaceholder+"\n"), 0400); err != nil {
			return nil, nil, err
		}
		env = append(env, "BASH_ENV="+copilotBashEnv)
		args = append(args, "--bash-env=on")
	}
	if model != "" && !harness.NativeArgsSelectModel(harness.NativeCopilot, c.Args) {
		args = append(args, "--model", model)
	}
	return append(args, c.Args...), env, nil
}

// copilotApp is the app the image unpacked (hack/clients/install.sh). The
// agent's home is a small noexec tmpfs where Copilot can't unpack it itself.
const copilotApp = "/opt/sproozi-clients/copilot-cache/copilot/pkg"

// linkCopilotApp points Copilot's cache at the image's unpacked app. An image
// without it leaves Copilot to unpack its own, as on a writable home.
func linkCopilotApp() error {
	if _, err := os.Stat(copilotApp); err != nil {
		return nil
	}
	cache := filepath.Join(agentHome, ".cache", "copilot")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	link := filepath.Join(cache, "pkg")
	if target, err := os.Readlink(link); err == nil && target == copilotApp {
		return nil
	}
	if err := os.RemoveAll(link); err != nil {
		return err
	}
	return os.Symlink(copilotApp, link)
}

// copilotKeyModel is the OpenAI model Copilot uses with a gateway-held key
// when none is configured, from the pinned Codex's model catalog; sproozi init
// --model or a harness --model chooses another.
const copilotKeyModel = "gpt-6.1-sol"

// nativeGHPlaceholder is the gh token the gateway replaces on GitHub routes.
const nativeGHPlaceholder = "sproozi-worker-placeholder"

// trustCopilotWorkspace adds the workspace to config.json's trustedFolders,
// keeping every other field, such as a restored sign-in. Copilot writes the
// file with a comment header, which is dropped.
func trustCopilotWorkspace(path string) error {
	document := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case len(data) > maxSignInBytes:
		return errors.New("copilot config.json exceeds its limit")
	default:
		var body []byte
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			if !bytes.HasPrefix(bytes.TrimSpace(line), []byte("//")) {
				body = append(body, line...)
				body = append(body, '\n')
			}
		}
		if len(bytes.TrimSpace(body)) != 0 {
			if err = json.Unmarshal(body, &document); err != nil {
				return fmt.Errorf("copilot config.json: %w", err)
			}
		}
	}
	var trusted []any
	if list, ok := document["trustedFolders"].([]any); ok {
		trusted = list
	}
	if slices.Contains(trusted, any(workspace)) && data != nil {
		return nil
	}
	document["trustedFolders"] = append(trusted, workspace)
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(encoded, '\n'))
}

// writePrivate replaces path with data, mode 0600, through a temporary file
// in its directory.
func writePrivate(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".sproozi-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// copilotSession is the session this visit runs: the restored one, or the
// one the supervisor named at launch.
func (s *server) copilotSession() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config.ThreadID != "" {
		return s.config.ThreadID
	}
	return s.launchID
}

// observeCopilotTurn reports the session's last completed turn from its
// journal. Terminal input must have settled first, so a prompt submitted
// just before a stop has reached the journal; an exited process has none.
func (s *server) observeCopilotTurn() (string, string, error) {
	id := s.copilotSession()
	s.mu.Lock()
	settled := s.inputGeneration == 0 || s.inputSettled() || s.state == harness.NativeStopped
	maxBytes := s.config.RolloutMaxBytes
	s.mu.Unlock()
	if id == "" || !settled {
		return "", "", harness.ErrCopilotTurnIncomplete
	}
	state, err := harness.ReadCopilotState(copilotHome, id, maxBytes)
	if err != nil {
		return "", "", err
	}
	turn, err := harness.CompletedCopilotTurn(state, maxBytes)
	return id, turn, err
}

// copilotTurnless reports a first visit with no Copilot conversation to keep.
func (s *server) copilotTurnless() bool {
	id := s.copilotSession()
	s.mu.Lock()
	maxBytes := s.config.RolloutMaxBytes
	s.mu.Unlock()
	state, err := harness.ReadCopilotState(copilotHome, id, maxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && harness.CopilotTurnless(state, maxBytes)
}

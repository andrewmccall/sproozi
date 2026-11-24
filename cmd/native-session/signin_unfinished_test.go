package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

// markedStore is a Copilot store whose writer stopped after its marker: no
// whole login, and not empty.
const markedStore = "#/ Copilot settings\n{\"logged_in_users\":[{\"login\":\"octocat\"}]}\n"

// copilotStartingAgainst is a Copilot session's login sync started against a
// store holding stored, after the supervisor's own start-up write: the
// workspace trust it merges into config.json before launch.
func copilotStartingAgainst(t *testing.T, stored string) *loginSync {
	t.Helper()
	store := t.TempDir()
	write(t, filepath.Join(store, "config.json"), stored)
	l := &loginSync{stored: filepath.Join(store, "config.json"), native: filepath.Join(t.TempDir(), "config.json"),
		interval: quietInterval, quit: make(chan struct{}), stopped: make(chan struct{}),
		diagnostics: &lockedBuffer{}, wait: testSaveWait}
	if err := l.begin(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.stop)
	if err := trustCopilotWorkspace(l.native); err != nil {
		t.Fatal(err)
	}
	l.settled()
	return l
}

// A Copilot session that starts while the store holds no whole login starts
// signed out, and the supervisor then writes workspace trust into its
// config.json. That config is not a change the harness made, so it never
// replaces the store, which every other session would copy in and lose its
// login. A sign-in the harness then makes does replace it.
func TestStartingAgainstAnUnfinishedStoreLeavesItForASignIn(t *testing.T) {
	l := copilotStartingAgainst(t, markedStore)
	for range 2 {
		if err := l.sync(); !errors.Is(err, errStoreNotWhole) {
			t.Fatalf("a sync of the unfinished store: %v", err)
		}
	}
	if got := content(t, l.stored); got != markedStore {
		t.Fatalf("the supervisor's own start-up write replaced the store: %q", got)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(content(t, l.native)), &config); err != nil {
		t.Fatal(err)
	}
	config["logged_in_users"] = []any{map[string]any{"login": "hubot"}} // The harness signs in.
	signedIn, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	write(t, l.native, string(signedIn))
	syncAll(t, l)
	if got := content(t, l.stored); got != string(signedIn) {
		t.Fatalf("a sign-in did not repair the store: %q", got)
	}
}

// With a whole store the session starts from its login, and the trust the
// supervisor merges is shared as before.
func TestStartingAgainstAWholeStoreSharesTheMergedConfig(t *testing.T) {
	const stored = "{\"logged_in_users\":[{\"login\":\"octocat\"}]}\n"
	l := copilotStartingAgainst(t, stored)
	syncAll(t, l)
	var config map[string]any
	if err := json.Unmarshal([]byte(content(t, l.stored)), &config); err != nil {
		t.Fatal(err)
	}
	if config["logged_in_users"] == nil || config["trustedFolders"] == nil {
		t.Fatalf("the store holds %v", config)
	}
}

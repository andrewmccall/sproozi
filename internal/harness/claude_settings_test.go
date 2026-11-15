package harness

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A managed launch runs unattended in a fresh home: the settings it passes
// with --settings must accept bypassPermissions there, or Claude Code waits at
// its confirmation before submitting the task.
func TestClaudeManagedSettingsSkipTheBypassConfirmation(t *testing.T) {
	data, err := json.Marshal(ClaudeManagedSettings("/usr/local/bin/native-session observe-claude"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Skip  *bool                       `json:"skipDangerousModePermissionPrompt"`
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err = json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Skip == nil || !*settings.Skip {
		t.Fatalf("settings do not skip the bypassPermissions confirmation: %s", data)
	}
	events := make([]string, 0, len(settings.Hooks))
	for _, event := range ClaudeHookEvents {
		if _, ok := settings.Hooks[event]; ok {
			events = append(events, event)
		}
	}
	if !reflect.DeepEqual(events, ClaudeHookEvents) || len(settings.Hooks) != len(ClaudeHookEvents) {
		t.Fatalf("hooks: %s", data)
	}
	var keys map[string]json.RawMessage
	if err = json.Unmarshal(data, &keys); err != nil || len(keys) != 2 {
		t.Fatalf("settings carry only hooks and the confirmation: %s", data)
	}
}

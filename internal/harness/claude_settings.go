package harness

// ClaudeHookEvents are the Claude Code lifecycle hooks a managed session's
// supervisor observes (ADR 0011).
var ClaudeHookEvents = []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure",
	claudeStopEvent, "StopFailure"}

// claudeStopEvent is the hook event that ends a completed turn.
const claudeStopEvent = "Stop"

// ClaudeManagedSettings is the settings file a managed Claude Code launch
// passes with --settings while every other setting source is disabled: each
// lifecycle hook runs command, and the bypassPermissions confirmation is
// already accepted. Claude Code 2.1.318 and later show that confirmation in a
// fresh home whatever .claude.json says, and wait at it before submitting the
// launch's prompt; a task runs unattended, so nobody would answer it. The
// launch's outer container and gateway are the confinement (ADR 0011).
func ClaudeManagedSettings(command string) map[string]any {
	hooks := map[string]any{}
	for _, event := range ClaudeHookEvents {
		hooks[event] = []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{
			"type": "command", "command": command, "timeout": 5}}}}
	}
	return map[string]any{"hooks": hooks, "skipDangerousModePermissionPrompt": true}
}

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
)

// claudePlugin holds the Sproozi plugin. Its name reserves the sproozi skill
// namespace; it lives outside the workspace and portable state.
const claudePlugin = "/tmp/sproozi-native/sproozi-plugin"

// installManagedCodex writes Codex's global instructions and the Sproozi skill
// into its home. Repository AGENTS.md files are added after, never instead.
func installManagedCodex(home string) error {
	return errors.Join(
		installManagedFile(filepath.Join(home, "AGENTS.md"), []byte(harness.ManagedInstructions)),
		installManagedSkill(home))
}

// installManagedClaudePlugin writes the plugin Claude loads with --plugin-dir,
// which works while every setting source stays disabled.
func installManagedClaudePlugin(dir string) error {
	manifest, err := json.Marshal(map[string]string{"name": api.ControlMCPServer, "description": "Sproozi integration"})
	if err != nil {
		return err
	}
	return errors.Join(
		installManagedFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest),
		installManagedSkill(dir))
}

// installManagedSkill writes skills/sproozi-access/SKILL.md under root.
func installManagedSkill(root string) error {
	return installManagedFile(managedSkillPath(root), harness.ManagedSkill(harness.ManagedSkillName))
}

func managedSkillPath(root string) string {
	return filepath.Join(root, "skills", harness.ManagedSkillName, "SKILL.md")
}

// installManagedFile replaces path with the supervisor's current content. The
// agent shares this UID and can still edit its copy; the gateway, not these
// files, enforces authority. Replacing keeps a stale or planted file from
// surviving into the next launch.
func installManagedFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.New("managed Sproozi file cannot be replaced")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0400)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
)

// claudePlugin holds the Sproozi plugin. Its name reserves the sproozi skill
// namespace; it lives outside the workspace and portable state. The profile
// plugin carries the user's skills under the profile namespace.
const (
	claudePlugin  = "/tmp/sproozi-native/sproozi-plugin"
	profilePlugin = "/tmp/sproozi-native/profile-plugin"
)

// Docker mounts the user's live profile folder read-only; Kubernetes mounts
// the profile archive recorded for the session.
var (
	profileDirectory = harness.NativeProfileDirectory
	profileArchive   = harness.NativeProfileArchive
)

// loadProfile reads the profile this launch received. Without either mount
// the session has an empty profile.
func loadProfile() (agentprofile.Profile, error) {
	data, err := os.ReadFile(profileArchive)
	if err == nil {
		return agentprofile.FromArchive(data)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return agentprofile.Profile{}, err
	}
	return agentprofile.LoadDirectory(profileDirectory)
}

// profileInstructions follows the Sproozi text with the user's instructions.
func profileInstructions(p agentprofile.Profile) string {
	if p.Instructions == "" {
		return harness.ManagedInstructions
	}
	return harness.ManagedInstructions + "\n\n" + p.Instructions
}

// installManagedCodex writes Codex's global instructions and skills into its
// home: the Sproozi text and skill, then the user's profile. Repository
// AGENTS.md files are added after, never instead. Rebuilding the skills
// directory drops skills removed from the profile since the last launch.
func installManagedCodex(home string, p agentprofile.Profile) error {
	if err := os.RemoveAll(filepath.Join(home, "skills")); err != nil {
		return err
	}
	return errors.Join(
		installManagedFile(filepath.Join(home, "AGENTS.md"), []byte(profileInstructions(p)), 0400),
		installManagedSkill(home),
		installProfileSkills(home, p))
}

// installProfilePlugin writes the user's skills as Claude's profile plugin and
// reports whether there is one to load.
func installProfilePlugin(dir string, p agentprofile.Profile) (bool, error) {
	if err := os.RemoveAll(dir); err != nil || len(p.Skills) == 0 {
		return false, err
	}
	manifest, err := json.Marshal(map[string]string{"name": "profile", "description": "Skills from your Sproozi profile"})
	if err != nil {
		return false, err
	}
	return true, errors.Join(
		installManagedFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest, 0400),
		installProfileSkills(dir, p))
}

func installProfileSkills(root string, p agentprofile.Profile) error {
	for _, s := range p.Skills {
		for _, f := range s.Files {
			mode := os.FileMode(0400)
			if f.Executable {
				mode = 0500
			}
			target := filepath.Join(root, "skills", s.Name, filepath.FromSlash(f.Path))
			if err := installManagedFile(target, f.Data, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// installManagedClaudePlugin writes the plugin Claude loads with --plugin-dir,
// which works while every setting source stays disabled.
func installManagedClaudePlugin(dir string) error {
	manifest, err := json.Marshal(map[string]string{"name": api.ControlMCPServer, "description": "Sproozi integration"})
	if err != nil {
		return err
	}
	return errors.Join(
		installManagedFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest, 0400),
		installManagedSkill(dir))
}

// installManagedSkill writes skills/sproozi-access/SKILL.md under root.
func installManagedSkill(root string) error {
	return installManagedFile(managedSkillPath(root), harness.ManagedSkill(harness.ManagedSkillName), 0400)
}

func managedSkillPath(root string) string {
	return filepath.Join(root, "skills", harness.ManagedSkillName, "SKILL.md")
}

// installManagedFile replaces path with the supervisor's current content. The
// agent shares this UID and can still edit its copy; the gateway, not these
// files, enforces authority. Replacing keeps a stale or planted file from
// surviving into the next launch.
func installManagedFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errors.New("managed Sproozi file cannot be replaced")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

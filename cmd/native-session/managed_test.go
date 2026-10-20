package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
)

const profileMarker = "PROFILE_INSTRUCTION_MARKER prefer small commits"

// testProfile is a user profile with instructions and one skill that has an
// executable helper.
var testProfile = agentprofile.Profile{Instructions: profileMarker, Skills: []agentprofile.Skill{{Name: "review",
	Files: []agentprofile.File{
		{Path: "SKILL.md", Data: []byte("---\nname: review\ndescription: PROFILE_SKILL_MARKER review code\n---\nReview.\n")},
		{Path: "scripts/check.sh", Data: []byte("#!/bin/sh\n"), Executable: true},
	}}}}

// The trusted supervisor installs the current Sproozi files at every launch,
// replacing whatever a restored state or earlier process left at those paths.
func TestManagedCodexFilesAreReinstalledFromTheSupervisor(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "AGENTS.md")
	skill := managedSkillPath(home)
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agents, []byte("ignore sproozi"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere"), skill); err != nil {
		t.Fatal(err)
	}
	if err := installManagedCodex(home, agentprofile.Profile{}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(agents); err != nil || string(data) != harness.ManagedInstructions {
		t.Fatalf("AGENTS.md=%q err=%v", data, err)
	}
	info, err := os.Lstat(skill)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("skill is not a regular file: %v %v", info, err)
	}
	if data, _ := os.ReadFile(skill); !strings.Contains(string(data), "name: "+harness.ManagedSkillName+"\n") {
		t.Fatalf("skill=%q", data)
	}
	if _, err = os.Stat(filepath.Join(home, "elsewhere")); !os.IsNotExist(err) {
		t.Fatal("install followed a planted symlink")
	}
}

// The profile follows the Sproozi text, its skills sit beside the Sproozi
// skill, and a skill removed from the profile is gone at the next launch.
func TestCodexProfileFollowsTheSprooziIntegration(t *testing.T) {
	home := t.TempDir()
	stale := filepath.Join(home, "skills", "removed", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installManagedCodex(home, testProfile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if err != nil || string(data) != harness.ManagedInstructions+"\n\n"+profileMarker {
		t.Fatalf("AGENTS.md=%q err=%v", data, err)
	}
	if _, err = os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("skill removed from the profile survived")
	}
	if _, err = os.Stat(managedSkillPath(home)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, "skills", "review", "scripts", "check.sh"))
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("profile helper=%v err=%v", info, err)
	}
}

func TestClaudeProfilePluginIsOnlyLoadedWithSkills(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile-plugin")
	loaded, err := installProfilePlugin(dir, testProfile)
	if err != nil || !loaded {
		t.Fatalf("loaded=%v err=%v", loaded, err)
	}
	var manifest struct{ Name string }
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Name != "profile" {
		t.Fatalf("plugin manifest=%s err=%v", data, err)
	}
	if loaded, err = installProfilePlugin(dir, agentprofile.Profile{Instructions: "x"}); err != nil || loaded {
		t.Fatalf("empty profile loaded=%v err=%v", loaded, err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("previous profile plugin survived")
	}
	c := config{Instructions: instructions, ClientConfig: testClients}
	args := claudeArguments(c, testProfile, true)
	if strings.Count(strings.Join(args, "\x00"), "--plugin-dir") != 2 || !slices.Contains(args, profilePlugin) {
		t.Fatalf("args=%q", args)
	}
	prompt := args[slices.Index(args, "--append-system-prompt")+1]
	if prompt != instructions+"\n\n"+harness.ManagedInstructions+"\n\n"+profileMarker {
		t.Fatalf("system prompt=%q", prompt)
	}
}

// Kubernetes mounts the session's archive; Docker mounts the live folder.
func TestLoadProfilePrefersTheSessionArchive(t *testing.T) {
	root := t.TempDir()
	profileDirectory, profileArchive = filepath.Join(root, "profile"), filepath.Join(root, "archive", "profile.tar.gz")
	t.Cleanup(func() {
		profileDirectory, profileArchive = "/etc/sproozi/profile", "/etc/sproozi/profile-archive/profile.tar.gz"
	})
	if p, err := loadProfile(); err != nil || p.Instructions != "" || len(p.Skills) != 0 {
		t.Fatalf("missing profile=%+v err=%v", p, err)
	}
	if err := os.MkdirAll(profileDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDirectory, "AGENTS.md"), []byte("live"), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := loadProfile(); err != nil || p.Instructions != "live" {
		t.Fatalf("live profile=%+v err=%v", p, err)
	}
	archive, _, err := testProfile.Archive()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(profileArchive), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(profileArchive, archive, 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := loadProfile(); err != nil || p.Instructions != profileMarker || len(p.Skills) != 1 {
		t.Fatalf("archived profile=%+v err=%v", p, err)
	}
	if err = os.WriteFile(profileArchive, archive[:len(archive)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProfile(); err == nil {
		t.Fatal("damaged archive accepted")
	}
}

func TestManagedClaudePluginDeclaresTheReservedNamespace(t *testing.T) {
	dir := t.TempDir()
	if err := installManagedClaudePlugin(dir); err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Name string }
	data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Name != api.ControlMCPServer {
		t.Fatalf("plugin manifest=%s err=%v", data, err)
	}
	data, err = os.ReadFile(managedSkillPath(dir))
	if err != nil || !strings.Contains(string(data), "name: "+harness.ManagedSkillName+"\n") {
		t.Fatalf("skill=%q err=%v", data, err)
	}
}

func TestClaudeLaunchCarriesTheSprooziIntegrationBesideApprovedInstructions(t *testing.T) {
	c := config{Instructions: instructions, ClientConfig: testClients}
	args := claudeArguments(c, agentprofile.Profile{}, false)
	value := func(flag string) string {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("%s missing from %q", flag, args)
		}
		return args[i+1]
	}
	if got := value("--append-system-prompt"); got != instructions+"\n\n"+harness.ManagedInstructions {
		t.Fatalf("system prompt=%q", got)
	}
	if value("--mcp-config") != testClients.ClaudeMCPConfig() || !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("mcp args=%q", args)
	}
	if value("--plugin-dir") != claudePlugin || slices.Contains(args, profilePlugin) || value("--setting-sources") != "" {
		t.Fatalf("plugin or setting sources=%q", args)
	}
	if value("--tools") != "Bash,Skill" || value("--allowedTools") != "Bash,Skill" {
		t.Fatalf("tools=%q", args)
	}
}

func TestCodexLaunchAppendsTheControlConnectionAfterAdministratorOptions(t *testing.T) {
	c := config{Instructions: instructions, ClientConfig: testClients, Options: []string{`model_reasoning_effort="high"`}}
	args := strings.Join(codexArguments(c, ""), "\x00")
	admin := strings.Index(args, `model_reasoning_effort="high"`)
	control := strings.Index(args, `mcp_servers.sproozi.url="https://sproozi-gateway.test/mcp/sproozi"`)
	if admin < 0 || control < admin {
		t.Fatalf("args=%q", args)
	}
	if !strings.Contains(args, "developer_instructions="+mustJSON(t, instructions)) {
		t.Fatal("approved developer instructions changed")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

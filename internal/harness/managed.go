package harness

import (
	_ "embed"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// ManagedSkillName is the reserved name of the Sproozi access-recovery skill.
// Claude receives it inside the reserved sproozi plugin as sproozi:access.
const ManagedSkillName = api.ControlMCPServer + "-access"

const managedSkillDescription = "Use when an external request (HTTP, git, package install, Kubernetes, MCP) " +
	"fails unexpectedly inside Sproozi: diagnose whether the gateway refused it and recover without bypassing it."

// ManagedInstructions are the always-loaded Sproozi environment instructions.
// They are delivered beside, never inside, the administrator's instructions,
// so the approved instruction digest and native continuity are unchanged.
//
//go:embed managed/instructions.md
var ManagedInstructions string

//go:embed managed/skill.md
var managedSkillBody string

// ManagedSkill renders the access-recovery SKILL.md under name, the harness's
// native skill identifier.
func ManagedSkill(name string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + managedSkillDescription + "\n---\n\n" + managedSkillBody)
}

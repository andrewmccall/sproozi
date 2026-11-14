// Command managedskill prints the SKILL.md the supervisor installs in the
// sproozi plugin, so the transcript capture loads the same skill.
package main

import (
	"os"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func main() {
	if _, err := os.Stdout.Write(harness.ManagedSkill(harness.ManagedSkillName)); err != nil {
		os.Exit(1)
	}
}

// Command managed prints a file the supervisor installs for a managed Claude
// Code launch, so the transcript capture launches the client with the same
// content: "skill" prints the sproozi plugin's SKILL.md, and "settings
// COMMAND" prints the --settings file whose hooks run COMMAND.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func main() {
	var data []byte
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "skill":
		data = harness.ManagedSkill(harness.ManagedSkillName)
	case len(os.Args) == 3 && os.Args[1] == "settings":
		data, err = json.Marshal(harness.ClaudeManagedSettings(os.Args[2]))
	default:
		err = fmt.Errorf("use: managed skill | managed settings COMMAND")
	}
	if err == nil {
		_, err = os.Stdout.Write(data)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

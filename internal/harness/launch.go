package harness

import (
	"embed"
	"io/fs"
)

// launchFiles holds every harness launch file. They live in one directory so
// the image build context, which .dockerignore limits to named paths, ships
// them all: a guard test checks the registry uses only files from here.
//
//go:embed launchfiles
var launchFiles embed.FS

// launchFile returns an embedded launch file's contents by name.
func launchFile(name string) string {
	data, err := fs.ReadFile(launchFiles, "launchfiles/"+name)
	if err != nil {
		panic("harness: launch file " + name + " is not in internal/harness/launchfiles: " + err.Error())
	}
	return string(data)
}

// launchFilesNamed maps each named launch file to its embedded contents.
func launchFilesNamed(names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = launchFile(name)
	}
	return out
}

// OpenCodeLaunch normalizes native session completion for stock OpenCode.
// It is mounted read-only with the run contract and needs Python 3 in the image.
var OpenCodeLaunch = launchFile("opencode-launch.py")

// HermesLaunch projects stock Hermes' terminal result into the bounded run result.
// It leaves the native model/tool loop intact and needs Python 3 in the image.
var HermesLaunch = launchFile("hermes-launch.py")

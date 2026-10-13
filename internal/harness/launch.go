package harness

import _ "embed"

// OpenCodeLaunch normalizes native session completion for stock OpenCode.
// It is mounted read-only with the run contract and needs Python 3 in the image.
//
//go:embed opencode-launch.py
var OpenCodeLaunch string

// HermesLaunch projects stock Hermes' terminal result into the bounded run result.
// It leaves the native model/tool loop intact and needs Python 3 in the image.
//
//go:embed hermes-launch.py
var HermesLaunch string

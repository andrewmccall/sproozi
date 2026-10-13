// Command native-checkpoint exercises the production native-state adapter.
// It emits checkpoint metadata only, never conversation text or configuration.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func main() {
	source := flag.String("source-home", "", "prepared source Codex home after its writer stopped")
	destination := flag.String("destination-home", "", "optional exclusively owned prepared destination Codex home")
	thread := flag.String("thread", "", "exact native thread UUID")
	maxBytes := flag.Int64("max-bytes", 8<<20, "maximum selected native rollout size")
	flag.Parse()
	if *source == "" || flag.NArg() != 0 {
		fail(fmt.Errorf("source home and an exact thread UUID are required"))
	}
	state, err := harness.CaptureCodexState(*source, *thread, *maxBytes)
	if err != nil {
		fail(err)
	}
	turn, err := harness.CompletedCodexTurn(state, *maxBytes)
	if err != nil {
		fail(err)
	}
	if *destination != "" {
		if err := harness.RestoreCodexState(*destination, state, *maxBytes); err != nil {
			fail(err)
		}
	}
	digest := sha256.Sum256(state.Rollout)
	metadata := struct {
		Version   string `json:"version"`
		ThreadID  string `json:"threadId"`
		TurnID    string `json:"turnId"`
		Workspace string `json:"originalWorkspace"`
		Path      string `json:"path"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
		Restored  bool   `json:"restored"`
	}{harness.CodexSessionVersion, state.ThreadID, turn, state.Workspace, state.Path,
		len(state.Rollout), hex.EncodeToString(digest[:]), *destination != ""}
	if err := json.NewEncoder(os.Stdout).Encode(metadata); err != nil {
		fail(err)
	}
}

func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "Native checkpoint operation failed:", err)
	os.Exit(1)
}

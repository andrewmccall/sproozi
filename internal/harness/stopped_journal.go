package harness

import "bytes"

// stoppedJournal returns the rows of a native journal its writer finished,
// for capture only. Every supported journal (Codex's rollout, Claude Code's
// transcript and Copilot's events) is append-only JSON lines, one row per
// line, so a final line without its newline is a row whose append was cut
// short, and the rows before it are exactly the journal at its last
// completed write. Capture runs only after the placement proved every writer
// stopped (ADR 0008), so that row can never be finished and is not part of
// the conversation. The live observers keep refusing it: while the writer may
// run, the row is still being written. The caller's validator still decides
// whether the rows kept hold a completed turn.
func stoppedJournal(data []byte) []byte {
	return data[:bytes.LastIndexByte(data, '\n')+1]
}

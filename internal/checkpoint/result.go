package checkpoint

import "strings"

// FinalText reads bounded untrusted workload output from a validated checkpoint.
// It does not establish successful execution, writer fencing or account usage.
func FinalText(data []byte, max int64) (string, bool, error) {
	_, a, err := inspect(data, max)
	if err != nil {
		return "", false, err
	}
	state, err := a.state()
	if err != nil {
		return "", false, err
	}
	text, err := state.FinalText(max)
	if err != nil {
		return "", false, err
	}
	const limit = 32 << 10
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "")
		return text, true, nil
	}
	return text, false, nil
}

package agentcontract

import "unicode/utf8"

// ResultVersion identifies the bounded final-answer publication protocol.
const ResultVersion = "sproozi.run-result/v1"

// MaxResultBytes leaves room beneath kubelet's per-container termination-message limit.
const MaxResultBytes = 3584

// Result is workload-controlled output, never an execution success signal.
type Result struct {
	Text      string
	Truncated bool
}

// resultEnvelope is private wire data. Pointer fields distinguish available
// empty output and false truncation from missing/null required members.
type resultEnvelope struct {
	Version string `json:"version"`
	RunUID  string `json:"runUID"`
	Result  *struct {
		Text      *string `json:"text"`
		Truncated *bool   `json:"truncated"`
	} `json:"result"`
}

// DecodeResult validates a single complete answer for the observed Run UID.
// Missing, malformed, oversized and foreign messages remain unavailable to callers.
func DecodeResult(data []byte, runUID string) (Result, error) {
	if !utf8.Valid(data) {
		return Result{}, invalid("result is not UTF-8")
	}
	var envelope resultEnvelope
	if err := decodeStrict(data, MaxResultBytes, &envelope); err != nil {
		return Result{}, err
	}
	if envelope.Version != ResultVersion || !uid.MatchString(runUID) || envelope.RunUID != runUID {
		return Result{}, invalid("result identity or version")
	}
	if envelope.Result == nil || envelope.Result.Text == nil || envelope.Result.Truncated == nil {
		return Result{}, invalid("missing result member")
	}
	return Result{Text: *envelope.Result.Text, Truncated: *envelope.Result.Truncated}, nil
}

package model

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ChatGPT plan usage has a narrower protocol contract than API-key inference.
// Reject unsupported requests before credentials, reservations or upstream I/O;
// do not silently remove fields that change the caller's requested semantics.
func validateChatGPTRequest(r *http.Request, body []byte, maxCostMicros int64) error {
	if maxCostMicros > 0 {
		return errors.New("ChatGPT plan usage requires a token-only policy; API dollar caps do not apply")
	}
	if r.URL.Path == modelsPath && r.Method == http.MethodGet {
		return nil
	}
	if r.URL.Path != responsesPath || r.Method != http.MethodPost {
		return errors.New("ChatGPT plan usage supports only Responses inference and model discovery")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return errors.New("invalid ChatGPT Responses request")
	}
	if string(fields["store"]) != "false" || string(fields["stream"]) != "true" {
		return errors.New("ChatGPT Responses requires store:false and stream:true")
	}
	var input []json.RawMessage
	if json.Unmarshal(fields["input"], &input) != nil || input == nil {
		return errors.New("ChatGPT Responses requires an input array")
	}
	for _, key := range []string{"background", "conversation", "max_output_tokens", "max_tool_calls",
		"metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier",
		"temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"} {
		if _, ok := fields[key]; ok {
			return errors.New("ChatGPT Responses request contains an unsupported field")
		}
	}
	return nil
}

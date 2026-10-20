package model

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func validOpenAIOperation(r *http.Request) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
		return false
	}
	switch r.URL.Path {
	case modelsPath:
		return r.Method == http.MethodGet
	case responsesPath, "/v1/chat/completions":
		return r.Method == http.MethodPost
	default:
		return false
	}
}

// prepareOpenAI checks price availability before cost-capped inference. This
// token-rate table does not price hosted tools or establish a provider spend cap.
func (h *Handler) prepareOpenAI(r *http.Request, body []byte, limits api.EndpointBudget) ([]byte, error) {
	if r.URL.Path == modelsPath || limits.MaxCostMicros == 0 {
		return body, nil
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return nil, fmt.Errorf("invalid OpenAI request")
	}
	var model string
	if json.Unmarshal(request["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("OpenAI model is required for cost-capped inference")
	}
	if h.Config.Pricing == nil {
		return nil, errPricingUnavailable
	}
	price, priced := h.Config.Pricing.Models[model]
	if !priced || price.InputMicrosPerMillionTokens <= 0 || price.CachedInputMicrosPerMillionTokens <= 0 || price.OutputMicrosPerMillionTokens <= 0 {
		return nil, errPricingUnavailable
	}
	if raw, present := request["service_tier"]; present {
		var tier string
		if json.Unmarshal(raw, &tier) != nil || tier != "default" {
			return nil, fmt.Errorf("OpenAI cost-capped inference requires the default service tier")
		}
	}
	// Omission defaults to the project's configured tier upstream. The retained
	// table supplies standard token prices, so select the standard tier explicitly.
	request["service_tier"] = json.RawMessage(`"default"`)
	return json.Marshal(request)
}

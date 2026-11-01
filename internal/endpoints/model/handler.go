/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

const (
	modelsPath     = "/v1/models"
	errorEventType = "error"
)

const maxModelBodyBytes = 1 << 20
const responsesPath = "/v1/responses"

// HandlerConfig holds all dependencies for Handler.
type HandlerConfig struct {

	// UpstreamPath maps an accepted operation's path to the provider's, such
	// as the ChatGPT backend's Responses path. Nil keeps the path.
	UpstreamPath func(string) string

	// KeepClientHeaders forwards a Messages client's own non-credential
	// headers, as a provider such as Copilot requires, instead of only the
	// Anthropic protocol headers.
	KeepClientHeaders bool

	// Pricing is administrator-owned and is required whenever a cost cap is set.
	Pricing *PricingTable

	// AuditLogger writes structured audit events for each gateway decision.
	AuditLogger audit.Writer

	// UpstreamURL is the base URL of the upstream model provider.
	UpstreamURL string

	// ProviderAuth is the single administrator-selected upstream authenticator.
	ProviderAuth modelauth.Authenticator

	// HTTPClient is the HTTP client used for upstream requests.
	HTTPClient *http.Client

	// Diagnostics is an administrator-enabled provider-response observer. It
	// receives bounded, redacted errors, never prompts or model output deltas.
	// This does not bypass usage verification or change budget settlement.
	Diagnostics func(ResponseDiagnostic)

	// Logger receives a line for each call charged an estimate or its whole
	// reservation instead of the provider's usage. Nil selects slog.Default().
	Logger *slog.Logger

	// OutputTokensPerSecond is R, the ceiling on output tokens per second
	// that prices the time an interrupted call ran. Zero selects
	// DefaultOutputTokensPerSecond.
	OutputTokensPerSecond int64

	// IdleTimeout ends a provider response that sends nothing for this long.
	// The provider client bounds only the wait for response headers. Zero
	// selects DefaultIdleTimeout.
	IdleTimeout time.Duration

	// Now is the clock that measures how long a call ran. Nil selects time.Now.
	Now func() time.Time

	// Drain, when set, lets a stopping gateway end and settle model calls.
	Drain *Drain
}

// Handler is the HTTP handler for the model gateway.
type Handler struct {
	// Config holds all dependencies and configuration for this handler.
	Config   HandlerConfig
	protocol providerProtocol
}

type providerProtocol uint8

const (
	openAIProtocol providerProtocol = iota
	anthropicProtocol
	// Provider authorities are reserved even when their provider is disabled.
	OpenAIAuthority    = "api.openai.com:443"
	AnthropicAuthority = "api.anthropic.com:443"
)

// NewAnthropicHandler uses the same authorization and accounting as OpenAI,
// with the native Messages protocol selected by trusted gateway composition.
func NewAnthropicHandler(config HandlerConfig) *Handler {
	return &Handler{Config: config, protocol: anthropicProtocol}
}

// requestStreamCheck is used to detect streaming requests before proxying.
type requestStreamCheck struct {
	Stream bool `json:"stream"`
}

// requestTokenBounds captures the bounded output-token fields used by the
// OpenAI chat-completions and responses APIs. A pointer distinguishes an
// omitted field from a field explicitly set to zero, although both result in
// a conservative reservation of all remaining policy capacity.
type requestTokenBounds struct {
	MaxTokens           *int64 `json:"max_tokens"`
	MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	MaxOutputTokens     *int64 `json:"max_output_tokens"`
}

// responseUsage is deliberately strict: a successful inference response must
// include total_tokens so the gateway can settle its admission accurately.
type responseUsage struct {
	Usage *tokenUsage `json:"usage"`
}

type tokenUsage struct {
	TotalTokens        *int64 `json:"total_tokens"`
	PromptTokens       *int64 `json:"prompt_tokens"`
	CompletionTokens   *int64 `json:"completion_tokens"`
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// openAIErrorBody is the OpenAI-compatible error envelope written on denied requests.
type openAIErrorBody struct {
	Error openAIErrorDetail `json:"error"`
}

// openAIErrorDetail carries the error detail fields in an OpenAI error envelope.
type openAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// ServeHTTP implements http.Handler. It validates the request, enforces capability
// and budget policies, proxies to the upstream model provider, and writes an audit event.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	e := audit.Event{Timestamp: time.Now().UTC(), Operation: r.URL.Path}

	identity, ok := gateway.IdentityFromContext(ctx)
	if !ok {
		h.deny(w, e, http.StatusUnauthorized, "gateway identity required", "auth_error", "missing_gateway_identity")
		return
	}
	e = gateway.AuditEvent(ctx, r.URL.Path)
	e.Service = ProviderOpenAI
	if h.protocol == anthropicProtocol {
		e.Service = ProviderAnthropic
	}

	limits, _, limitErr := identity.BudgetLimits(sprooziv1alpha1.CapabilityModelInference)
	if limitErr != nil {
		h.deny(w, e, http.StatusForbidden, "model allocation denied", "permission_error", "allocation_denied")
		return
	}
	e.TokenBudget = limits.MaxUnits

	// Resolve the supported operation before admission or provider credentials.
	if h.protocol == openAIProtocol && !validOpenAIOperation(r) {
		h.deny(w, e, http.StatusForbidden,
			"unsupported OpenAI operation", "permission_error", "path_not_allowed")
		return
	}

	// Responses and Messages SSE are relayed as they arrive. Other streaming
	// protocols are denied before reserving capacity.
	bodyBytes, ok := h.readBody(w, r, e)
	if !ok {
		return
	}
	var err error
	if len(bodyBytes) != 0 {
		if err := unambiguousJSON(bodyBytes); err != nil {
			message, code := jsonRefusal(err)
			h.deny(w, e, http.StatusBadRequest, message, "invalid_request_error", code)
			return
		}
	}
	var check requestStreamCheck
	if h.protocol == anthropicProtocol {
		bodyBytes, err = h.prepareAnthropic(r, bodyBytes, limits)
		if err != nil {
			h.deny(w, e, http.StatusForbidden, err.Error(), "permission_error", refusalCode(err))
			return
		}
	}
	if len(bodyBytes) != 0 && json.Unmarshal(bodyBytes, &check) != nil {
		h.deny(w, e, http.StatusBadRequest, "request body field stream must be a boolean",
			"invalid_request_error", "invalid_field")
		return
	}
	if check.Stream && r.URL.Path != responsesPath && h.protocol != anthropicProtocol {
		h.deny(w, e, http.StatusForbidden,
			"streaming responses are not supported by the model gateway",
			"permission_error", "streaming_not_supported")
		return
	}
	if err := detachedWork(bodyBytes); err != nil {
		h.deny(w, e, http.StatusBadRequest, err.Error(), "invalid_request_error", "unsupported_request")
		return
	}
	providerAuth := h.Config.ProviderAuth
	if providerAuth == nil {
		h.deny(w, e, http.StatusServiceUnavailable, "provider authentication is unavailable", "server_error", "provider_auth_unavailable")
		return
	}
	e.ProviderAuthMode = providerAuth.Mode()
	if providerAuth.Mode() == modelauth.WorkloadMode && !modelauth.HasWorkloadCredential(r.Header) {
		h.deny(w, e, http.StatusUnauthorized,
			"the harness sent no provider credential; sign the harness in to its model provider",
			"authentication_error", "provider_credential_missing")
		return
	}
	if providerAuth.Mode() == modelauth.ChatGPTMode {
		if err := validateChatGPTRequest(r, bodyBytes, limits.MaxCostMicros); err != nil {
			h.deny(w, e, http.StatusForbidden, err.Error(), "permission_error", "chatgpt_request_not_supported")
			return
		}
	} else if h.protocol == openAIProtocol {
		bodyBytes, err = h.prepareOpenAI(r, bodyBytes, limits)
		if err != nil {
			h.deny(w, e, http.StatusForbidden, err.Error(), "permission_error", refusalCode(err))
			return
		}
	}

	// The models discovery endpoint does not consume model tokens and
	// therefore has no reservation or model call deadline.
	if r.URL.Path == modelsPath {
		h.forward(w, r, e, identity, limits, bodyBytes, providerAuth, nil)
		return
	}
	// A model call may stream past the inspected connection's request
	// deadline, up to the gateway maximum, which the policy can only lower.
	var limit time.Duration
	if seconds := identity.Policy.Spec.MaxModelCallSeconds; seconds != nil && *seconds > 0 {
		limit = time.Duration(*seconds) * time.Second
	}
	// A stopping gateway refuses new calls and settles the ones in flight.
	leave, open := h.Config.Drain.enter()
	if !open {
		h.deny(w, e, http.StatusServiceUnavailable, gatewayStoppingMessage, "server_error", gatewayStoppingCode)
		return
	}
	defer leave()
	callCtx, endCall := proxytransport.ExtendModelCall(ctx, limit)
	defer endCall()
	if h.Config.Drain != nil {
		var stop context.CancelCauseFunc
		callCtx, stop = context.WithCancelCause(callCtx)
		defer stop(context.Canceled)
		defer context.AfterFunc(h.Config.Drain.ctx, func() { stop(errGatewayStopping) })()
	}
	// Reserve capacity atomically before forwarding. A self-contained request
	// with an output bound reserves a bounded amount and may run alongside
	// other bounded calls; any other request reserves all remaining capacity
	// and runs alone. The ledger can recover the reservation if this gateway
	// stops without settling it.
	plan := h.planReservation(bodyBytes, limits)
	call := &modelCall{h: h, ctx: callCtx, w: w, r: r, e: e, limits: limits,
		plan: plan, body: bodyBytes, stream: check.Stream}
	reservation, endAdmission, err := budget.FromContext(ctx).ReserveRequest(callCtx, plan.units, plan.costMicros,
		plan.bounded, call.recovery())
	if err != nil {
		h.refuseReservation(w, e, call, err)
		return
	}
	defer endAdmission()
	call.reservation = reservation
	h.forward(w, r, e, identity, limits, bodyBytes, providerAuth, call)
}

// refuseReservation answers a call whose reservation failed: it ended while
// it waited, too many calls are pending, or the budget is exhausted.
func (h *Handler) refuseReservation(w http.ResponseWriter, e audit.Event, call *modelCall, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// No policy refused the call: it ended while it waited. Audit how.
		e.Allowed, e.EndReason = true, call.cancelReason()
		h.Config.AuditLogger.Log(e)
		if e.EndReason == endGatewayRestart {
			h.writeError(w, http.StatusServiceUnavailable, gatewayStoppingMessage, "server_error", gatewayStoppingCode)
			return
		}
		h.writeError(w, http.StatusRequestTimeout, "model request cancelled while waiting for its budget",
			"server_error", "request_cancelled")
		return
	}
	if errors.Is(err, budget.ErrTooManyPending) {
		h.deny(w, e, http.StatusTooManyRequests, err.Error(), "rate_limit_error", "too_many_pending_calls")
		return
	}
	h.deny(w, e, http.StatusTooManyRequests, err.Error(), "budget_error", "budget_exhausted")
}

// forward sends the request upstream. Without a model call, as for model
// discovery, it relays the bounded response unchanged.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, e audit.Event, identity *gateway.ExecutionGrant,
	limits sprooziv1alpha1.EndpointBudget, bodyBytes []byte, providerAuth modelauth.Authenticator, call *modelCall,
) {
	ctx := r.Context()
	var reservation *budget.Reservation
	if call != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(call.ctx)
		defer cancel()
		call.cancelUpstream, reservation = cancel, call.reservation
	}
	release := func() {
		event := &e
		if call != nil {
			event = &call.e
		}
		h.release(event, reservation)
	}
	upstreamReq, err := http.NewRequestWithContext(
		ctx, r.Method, h.Config.UpstreamURL+h.upstreamTarget(r), bytes.NewReader(bodyBytes),
	)
	if err != nil {
		release()
		h.deny(w, e, http.StatusInternalServerError,
			"failed to build upstream request", "server_error", "upstream_error")
		return
	}
	h.copyProviderHeaders(upstreamReq.Header, r.Header, providerAuth.Mode() == modelauth.WorkloadMode)
	if err = providerAuth.Authorize(upstreamReq); err != nil {
		release()
		h.deny(w, e, http.StatusServiceUnavailable, "provider authentication is unavailable", "server_error", "provider_auth_unavailable")
		return
	}
	// Let the trusted Go transport negotiate and decode compression. Forwarding
	// a client's Accept-Encoding disables its transparent gzip decoding and can
	// expose compressed bytes to the usage verifier instead of Responses SSE.
	upstreamReq.Header.Del("Accept-Encoding")

	providerClient := *h.Config.HTTPClient
	providerClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// The provider starts work once it has the whole request. A call that
	// does not stream sends headers only when generation finishes, so an
	// interrupt before the headers is charged once the request was written.
	if call != nil {
		upstreamReq = upstreamReq.WithContext(httptrace.WithClientTrace(upstreamReq.Context(), &httptrace.ClientTrace{
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					at := call.now()
					call.written.CompareAndSwap(nil, &at)
				}
			},
		}))
	}
	resp, err := providerClient.Do(upstreamReq)
	if err != nil {
		if call != nil && call.written.Load() != nil {
			call.startedAt = *call.written.Load()
			call.observeFailure("provider call ended before its response headers")
			reason := endUpstreamError
			if call.ctx.Err() != nil {
				reason = call.cancelReason()
			}
			call.settleInterrupted(reason)
			if reason == endClientCancelled {
				// The harness left; nothing failed and no one reads a reply.
				call.finish()
				return
			}
			if reason == endGatewayRestart {
				h.deny(w, call.e, http.StatusServiceUnavailable, gatewayStoppingMessage, "server_error", gatewayStoppingCode)
				return
			}
			h.deny(w, call.e, http.StatusBadGateway,
				"upstream request failed", "server_error", "upstream_error")
			return
		}
		// The provider never received the whole request, so it did no work:
		// release the reservation so a retry can use it.
		release()
		if call != nil && call.cancelReason() == endClientCancelled && call.ctx.Err() != nil {
			// The harness left while the request was written; nothing failed.
			call.e.EndReason = endClientCancelled
			call.finish()
			return
		}
		if call != nil && call.cancelReason() == endGatewayRestart && call.ctx.Err() != nil {
			call.e.EndReason = endGatewayRestart
			h.deny(w, call.e, http.StatusServiceUnavailable, gatewayStoppingMessage, "server_error", gatewayStoppingCode)
			return
		}
		h.deny(w, e, http.StatusBadGateway,
			"upstream request failed", "server_error", "upstream_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var diagnostic *ResponseDiagnostic
	credential := upstreamReq.Header.Get("X-Api-Key")
	if credential == "" {
		credential = strings.TrimPrefix(upstreamReq.Header.Get("Authorization"), "Bearer ")
	}
	if h.Config.Diagnostics != nil {
		described := describeResponse(resp, nil, credential)
		described.RunUID = identity.Execution.UID
		diagnostic = &described
		defer func() { h.Config.Diagnostics(*diagnostic) }()
		if call != nil {
			call.diagnostic, call.credential = diagnostic, credential
		}
	}
	if call != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		call.relay(resp)
		return
	}
	// Provider errors and model discovery are small and carry no model
	// output; relay them whole. Provider errors do not represent successful
	// model consumption, so release the admission for a retry.
	release()
	body, err := readBounded(resp.Body, maxModelBodyBytes)
	if diagnostic != nil {
		*diagnostic = describeResponse(resp, body, credential)
		diagnostic.RunUID = identity.Execution.UID
	}
	if err != nil {
		if diagnostic != nil {
			diagnostic.ValidationError = "response read failed or exceeded body limit"
		}
		h.deny(w, e, http.StatusBadGateway,
			"failed to read upstream response", "server_error", "upstream_error")
		return
	}
	if h.protocol == anthropicProtocol {
		h.deny(w, e, resp.StatusCode, "provider rejected request", "api_error", "provider_rejected")
		return
	}
	h.copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	h.auditModelSuccess(r.Context(), e, resp.StatusCode, 0, budget.FromContext(r.Context()), limits)
}

func (h *Handler) copyResponseHeaders(target, source http.Header) {
	for k, vv := range source {
		if h.protocol == anthropicProtocol && !strings.EqualFold(k, "Content-Type") && !strings.EqualFold(k, "Request-ID") {
			continue
		}
		for _, v := range vv {
			target.Add(k, v)
		}
	}
}

func invalidTokenUsage(
	usageResp responseUsage, usageErr error, limits sprooziv1alpha1.EndpointBudget,
) bool {
	return usageErr != nil ||
		usageResp.Usage == nil ||
		usageResp.Usage.TotalTokens == nil ||
		*usageResp.Usage.TotalTokens < 0 ||
		(limits.MaxCostMicros > 0 && (usageResp.Usage.PromptTokens == nil ||
			usageResp.Usage.CompletionTokens == nil))
}

func requestTokenEstimate(body []byte) int64 {
	var request requestTokenBounds
	if err := json.Unmarshal(body, &request); err != nil {
		return 0
	}
	var estimate int64
	for _, bound := range []*int64{
		request.MaxTokens, request.MaxCompletionTokens, request.MaxOutputTokens,
	} {
		if bound != nil && *bound > estimate {
			estimate = *bound
		}
	}
	return estimate
}

// unambiguousJSON rejects request bodies whose meaning depends on the parser.
// encoding/json keeps the last duplicate key, while a provider may keep the
// first, so no object may repeat a key exactly. encoding/json also matches
// struct fields regardless of case, while providers match exactly, so the
// objects the gateway inspects must not contain keys that differ only by case.
// Other content, such as tool schemas and message text, keeps its own keys.
func unambiguousJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	// Read numbers as text, so one too large for a float64 is named as such
	// rather than as invalid JSON.
	decoder.UseNumber()
	if err := inspectJSONValue(decoder, inspectedRequest, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errTrailingData
	}
	// Refuse whatever encoding/json cannot decode, such as nesting deeper
	// than it allows, so no check that parses the body can see it fail and
	// treat a field as absent. Every request body is one JSON object.
	if !json.Valid(body) {
		return errInvalidJSON
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return errNotObject
	}
	return nil
}

// inspectedObject identifies the request objects that the budget and pricing
// checks decode, so their keys must be unambiguous regardless of case.
type inspectedObject uint8

const (
	opaqueContent inspectedObject = iota
	inspectedRequest
	inspectedMessage
	inspectedBlock
	inspectedCacheControl
)

// inspectedChildren maps an inspected object's field to the kind of object, or
// array elements, that the checks decode from it.
var inspectedChildren = map[inspectedObject]map[string]inspectedObject{
	inspectedRequest: {"messages": inspectedMessage, "system": inspectedBlock, "tools": inspectedBlock,
		"cache_control": inspectedCacheControl},
	inspectedMessage: {contentField: inspectedBlock},
	inspectedBlock:   {"cache_control": inspectedCacheControl, "content": inspectedBlock},
}

func inspectJSONValue(decoder *json.Decoder, kind inspectedObject, depth int) error {
	// encoding/json refuses deeper nesting, so no check would accept it either.
	if depth > 10000 {
		return fmt.Errorf("model request nesting exceeds bounds")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid model request JSON")
	}
	if number, ok := token.(json.Number); ok {
		if _, err := strconv.ParseFloat(string(number), 64); err != nil {
			return errNumberTooLarge
		}
		return nil
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	keys, folded := map[string]bool{}, map[string]bool{}
	for decoder.More() {
		child := kind // array elements share the array's kind
		if delimiter == '{' {
			token, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("invalid model request JSON")
			}
			key := token.(string)
			fold := strings.ToLower(strings.ToUpper(key))
			if keys[key] || kind != opaqueContent && folded[fold] {
				return errDuplicateKey
			}
			keys[key], folded[fold] = true, true
			// Struct decoding selects the field regardless of case.
			child = inspectedChildren[kind][fold]
		}
		if err := inspectJSONValue(decoder, child, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token() // closing delimiter
	return err
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}

// deny writes an OpenAI-compatible JSON error response, sets the HTTP status, and logs
// a denied audit event.
func (h *Handler) deny(w http.ResponseWriter, e audit.Event, status int, message, errType, code string) {
	e.Allowed = false
	e.DenyReason = message
	h.Config.AuditLogger.Log(e)
	h.writeError(w, status, message, errType, code)
}

// writeError writes the protocol's error body.
func (h *Handler) writeError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if h.protocol == anthropicProtocol {
		_ = json.NewEncoder(w).Encode(map[string]any{typeField: errorEventType, "error": map[string]string{typeField: errType, "message": message}})
		return
	}
	_ = json.NewEncoder(w).Encode(openAIErrorBody{
		Error: openAIErrorDetail{Message: message, Type: errType, Code: code},
	})
}

func (h *Handler) prepareAnthropic(r *http.Request, body []byte, limits sprooziv1alpha1.EndpointBudget) ([]byte, error) {
	if err := validateAnthropicRequest(r, body, limits.MaxCostMicros > 0); err != nil {
		return nil, err
	}
	if limits.MaxCostMicros == 0 {
		return body, nil
	}
	var request map[string]json.RawMessage
	_ = json.Unmarshal(body, &request)
	var model string
	_ = json.Unmarshal(request["model"], &model)
	if h.Config.Pricing == nil {
		return nil, errPricingUnavailable
	}
	if _, priced := h.Config.Pricing.Models[model]; !priced {
		return nil, errPricingUnavailable
	}
	// Omitted service_tier defaults to auto upstream. Capped admission uses
	// only the administrator's standard pricing, never Priority Tier capacity.
	request["service_tier"] = json.RawMessage(`"standard_only"`)
	return json.Marshal(request)
}

// readBody reads and decodes the request body, refusing an unsupported or
// corrupt encoding and a body past the gateway limit.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, e audit.Event) ([]byte, bool) {
	body, err := decodedBody(r, maxModelBodyBytes)
	switch {
	case errors.Is(err, errUnsupportedEncoding):
		h.deny(w, e, http.StatusUnsupportedMediaType,
			"request body uses an unsupported content encoding", "invalid_request_error", "unsupported_encoding")
	case errors.Is(err, errUndecodableBody):
		h.deny(w, e, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_encoding")
	case err != nil:
		h.deny(w, e, http.StatusRequestEntityTooLarge,
			"request body exceeds gateway limit", "invalid_request_error", "request_too_large")
	default:
		return body, true
	}
	return nil, false
}

// upstreamTarget is the provider's path and query for an accepted request.
func (h *Handler) upstreamTarget(r *http.Request) string {
	if h.Config.UpstreamPath == nil {
		return r.URL.RequestURI()
	}
	target := h.Config.UpstreamPath(r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	return target
}

// copyProviderHeaders never forwards cookies or the gateway credential. It
// forwards the workload's own provider credential only in workload mode.
func (h *Handler) copyProviderHeaders(target, source http.Header, workloadCredential bool) {
	for k, vv := range source {
		name := strings.ToLower(k)
		credential := name == "authorization" || name == "x-api-key"
		// The gateway forwards the body it decoded and inspected.
		if name == "proxy-authorization" || name == "cookie" || name == "content-encoding" ||
			credential && !workloadCredential {
			continue
		}
		if h.protocol == anthropicProtocol && !credential && !h.Config.KeepClientHeaders &&
			!slices.Contains([]string{"content-type", "accept", "anthropic-version", "anthropic-beta"}, name) {
			continue
		}
		for _, v := range vv {
			target.Add(k, v)
		}
	}
	if h.protocol == anthropicProtocol {
		target.Set("Anthropic-Version", "2023-06-01")
	}
}

// requestModel names the requested model for audit, bounded because the value
// comes from the workload even though the provider has already accepted it.
func requestModel(body []byte) string {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &request)
	if len(request.Model) > 128 {
		return ""
	}
	return request.Model
}

// splitTokens returns input and output usage from whichever OpenAI-compatible
// field pair the provider reported; Anthropic usage is normalized to prompt and completion.
func splitTokens(usage *tokenUsage) (int64, int64) {
	value := func(values ...*int64) int64 {
		for _, v := range values {
			if v != nil {
				return *v
			}
		}
		return 0
	}
	return value(usage.InputTokens, usage.PromptTokens), value(usage.OutputTokens, usage.CompletionTokens)
}

func (h *Handler) providerCost(body []byte, usage responseUsage, anthropic anthropicUsage) (int64, error) {
	if h.Config.Pricing == nil {
		return 0, errPricingUnavailable
	}
	var requestModel struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &requestModel)
	if h.protocol == anthropicProtocol {
		return h.Config.Pricing.costAnthropic(requestModel.Model, anthropic)
	}
	cached := int64(0)
	if usage.Usage.PromptTokensDetails != nil && usage.Usage.PromptTokensDetails.CachedTokens != nil {
		cached = *usage.Usage.PromptTokensDetails.CachedTokens
	}
	return h.Config.Pricing.Cost(requestModel.Model, *usage.Usage.PromptTokens, cached, *usage.Usage.CompletionTokens)
}

// A stopping gateway refuses and ends calls with this retryable reply.
const (
	gatewayStoppingMessage = "the model gateway is shutting down; retry the call"
	gatewayStoppingCode    = "gateway_stopping"
)

// Request bodies the JSON check refuses, each with its own reply.
var (
	errTrailingData   = errors.New("model request has trailing data")
	errDuplicateKey   = errors.New("model request has duplicate key")
	errNumberTooLarge = errors.New("model request has a number too large to read")
	errInvalidJSON    = errors.New("model request is not valid JSON")
	errNotObject      = errors.New("model request is not a JSON object")
)

// jsonRefusal is the message and code for a body unambiguousJSON refuses.
func jsonRefusal(err error) (message, code string) {
	switch {
	case errors.Is(err, errTrailingData):
		return "request body must be exactly one JSON value; remove the data after it", "trailing_data"
	case errors.Is(err, errDuplicateKey):
		return "request body must be one JSON value without duplicate keys; the keys of the request, its " +
			"messages and their content blocks are compared without case", "ambiguous_json"
	case errors.Is(err, errNumberTooLarge):
		return "request body contains a number too large to read", "number_too_large"
	case errors.Is(err, errNotObject):
		return "request body must be a JSON object", "invalid_json"
	default:
		return "request body is not valid JSON", "invalid_json"
	}
}

// errPricingUnavailable refuses a cost-capped call whose model has no
// administrator price.
var errPricingUnavailable = errors.New(reasonUnpriced)

// refusalCode names why a request was refused before inference: missing
// pricing, or an operation or option the gateway does not support.
func refusalCode(err error) string {
	if errors.Is(err, errPricingUnavailable) {
		return "pricing_unavailable"
	}
	return "unsupported_operation"
}

func (h *Handler) logger() *slog.Logger {
	if h.Config.Logger == nil {
		return slog.Default()
	}
	return h.Config.Logger
}

// forfeit charges a whole reservation when the provider may have done work
// that no estimate can bound, records the charged units in the audit event
// and logs the charge with its reason. A reservation made without an output
// bound holds all remaining capacity, so this can exhaust the allowance.
func (h *Handler) forfeit(e *audit.Event, reservation *budget.Reservation, reason string) {
	if reservation == nil {
		return
	}
	// The charge is exactly what this reservation holds in its limited
	// dimensions, so parallel settlements cannot distort it.
	units, cost := reservation.Held()
	if err := reservation.Forfeit(); err != nil {
		h.settleFailed(e, err)
		return
	}
	e.TokensUsed = units
	h.logger().Info("Charged model reservation in full", "runID", e.RunID, "runUID", e.RunUID,
		"endReason", e.EndReason, "detail", reason, "units", units, "costMicros", cost)
}

// settleFailed records a settlement, forfeit or release the ledger did not
// apply. A reservation whose lease was already recovered was charged its
// recovery estimate instead; anything else left the charge unrecorded.
func (h *Handler) settleFailed(e *audit.Event, err error) {
	e.TokensUsed = 0
	if errors.Is(err, budget.ErrReservationRecovered) {
		e.UsageSource = usageRecovered
		h.logger().Info("Model reservation was already recovered", "runID", e.RunID, "runUID", e.RunUID,
			"endReason", e.EndReason)
		return
	}
	e.UsageSource = usageUnsettled
	h.logger().Error("Failed to settle model reservation", "runID", e.RunID, "runUID", e.RunUID,
		"endReason", e.EndReason, "error", err.Error())
}

// release frees a reservation the provider did no work for.
func (h *Handler) release(e *audit.Event, reservation *budget.Reservation) {
	if reservation == nil {
		return
	}
	if err := reservation.Release(); err != nil {
		h.settleFailed(e, err)
	}
}

func (h *Handler) auditModelSuccess(ctx context.Context, e audit.Event, upstreamStatus int, tokensUsed int64, meter *budget.Meter, limits sprooziv1alpha1.EndpointBudget) {
	state, stateErr := meter.State(ctx)
	// Emit success audit event.
	e.Allowed = true
	e.UpstreamStatus = upstreamStatus
	e.TokensUsed = tokensUsed
	switch {
	case stateErr == nil && limits.MaxUnits > 0:
		remaining := max(0, limits.MaxUnits-state.UnitsUsed)
		e.TokensRemaining = &remaining
	case stateErr == nil:
		// No unit limit leaves nothing remaining to report.
	case h.Config.Diagnostics != nil:
		// The response already succeeded and its trusted usage was settled.
		// A failed subsequent observation means unknown remaining capacity,
		// not erased usage or a new request allowance. Do not expose raw errors.
		h.Config.Diagnostics(ResponseDiagnostic{Kind: "model-budget-observation", RunUID: e.RunUID,
			ErrorCode: "budget_state_unavailable", ValidationError: "remaining model budget is unavailable"})
	}
	h.Config.AuditLogger.Log(e)
}

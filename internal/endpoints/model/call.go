package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
)

// Streaming defaults and bounds.
const (
	// DefaultOutputTokensPerSecond is R, the ceiling on output tokens per
	// second that prices the time an interrupted call ran.
	DefaultOutputTokensPerSecond = 250
	// DefaultIdleTimeout ends a provider response that sends nothing for
	// this long.
	DefaultIdleTimeout = 5 * time.Minute
	// maxEventBytes bounds the one event the parser holds. A Responses
	// response.completed event carries the whole output, so it is large.
	maxEventBytes = 8 << 20
	// inputBytesPerToken divides request bytes into estimated input tokens.
	inputBytesPerToken = 4
	readChunkBytes     = 32 << 10
)

// How a call was charged, recorded as the audit event's usageSource.
const (
	usageProvider    = "provider"
	usageEstimated   = "estimated"
	usageReservation = "reservation"
	// usageRecovered: the ledger had already settled the reservation's lease
	// at its recovery estimate, so this call's own charge was not recorded.
	usageRecovered = "recovered"
	// usageUnsettled: the ledger did not record the charge.
	usageUnsettled = "unsettled"
)

// How a call ended, recorded as the audit event's endReason.
const (
	endCompleted       = "completed"
	endClientCancelled = "client_cancelled"
	endDeadline        = "deadline"
	endUpstreamError   = "upstream_error"
	endBudgetCut       = "budget_cut"
	endParseLimit      = "parse_limit"
	endExceeded        = "exceeded"
	// endGatewayRestart ends a call its gateway stopped; the ledger records
	// the same reason when it recovers an abandoned reservation.
	endGatewayRestart = "gateway_restart"
)

// Reasons logged with "Charged model reservation in full".
const (
	reasonUnsizable   = "request references input the gateway cannot size"
	reasonParseLimit  = "provider event exceeded the parse limit"
	reasonBudgetCut   = "observed usage reached the reservation"
	reasonUnpriced    = "administrator pricing is unavailable"
	budgetCutMessage  = "model budget exhausted: the call reached its reservation"
	inputFromProvider = "provider"
	inputFromRequest  = "request_bytes"
	promptField       = "prompt"
	contentField      = "content"
	sseFormat         = "sse"
	typeField         = "type"
)

// reservationPlan is what one request reserves. A bounded plan holds the
// most the request can use; otherwise the call reserves all remaining
// capacity.
type reservationPlan struct {
	units, costMicros int64
	outputBound       int64
	bounded           bool
	// sizable is false when the request references input that its bytes do
	// not contain, so no estimate can bound an interrupted call.
	sizable bool
}

// planReservation bounds a self-contained request with an output bound. A
// token is at least one byte, so request bytes bound its input.
func (h *Handler) planReservation(body []byte, limits sprooziv1alpha1.EndpointBudget) reservationPlan {
	plan := reservationPlan{outputBound: requestTokenEstimate(body), sizable: !unsizableReference(body)}
	plan.bounded = plan.sizable && plan.outputBound > 0 && !providerTools(body)
	if !plan.bounded {
		return reservationPlan{outputBound: plan.outputBound, sizable: plan.sizable}
	}
	plan.units = int64(len(body)) + plan.outputBound
	if limits.MaxCostMicros > 0 {
		price := h.price(requestModel(body))
		input := max(price.InputMicrosPerMillionTokens, price.CacheWrite5mMicrosPerMillionTokens,
			price.CacheWrite1hMicrosPerMillionTokens)
		cost, err := priceTokens(int64(len(body)), input, plan.outputBound, price.OutputMicrosPerMillionTokens)
		if err != nil || cost <= 0 {
			return reservationPlan{outputBound: plan.outputBound, sizable: plan.sizable}
		}
		plan.costMicros = cost
	}
	return plan
}

func (h *Handler) price(model string) ModelPrice {
	if h.Config.Pricing == nil {
		return ModelPrice{}
	}
	return h.Config.Pricing.Models[model]
}

// priceTokens prices input and output tokens, rounding up to a micro-dollar.
func priceTokens(input, inputRate, output, outputRate int64) (int64, error) {
	numerator := new(big.Int).Mul(big.NewInt(input), big.NewInt(inputRate))
	numerator.Add(numerator, new(big.Int).Mul(big.NewInt(output), big.NewInt(outputRate)))
	return roundedCost(numerator)
}

// unsizableReference reports whether a request names input that its bytes do
// not contain: a stored response, conversation or prompt, an uploaded file, or
// a remote document or image. Only message content is inspected, so tool
// inputs and schemas that mention URLs are not references.
func unsizableReference(body []byte) bool {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return true // a body it cannot read is never assumed self-contained
	}
	for _, key := range []string{"previous_response_id", "conversation", promptField} {
		if raw, present := request[key]; present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			// Legacy completion prompts are text; only a prompt object is a reference.
			if key != promptField || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
				return true
			}
		}
	}
	for _, key := range []string{"messages", "input", "system"} {
		var content any
		if json.Unmarshal(request[key], &content) == nil && referencesContent(content) {
			return true
		}
	}
	return false
}

func referencesContent(value any) bool {
	switch v := value.(type) {
	case []any:
		if slices.ContainsFunc(v, referencesContent) {
			return true
		}
	case map[string]any:
		if contentReference(v) {
			return true
		}
		for _, key := range []string{contentField, "output"} {
			if referencesContent(v[key]) {
				return true
			}
		}
	}
	return false
}

// contentReference recognises the content blocks of Anthropic Messages,
// OpenAI Responses and chat completions that point outside the request.
func contentReference(block map[string]any) bool {
	present := func(value any) bool { return value != nil && value != "" }
	if itemReference(block) {
		return true
	}
	switch block["type"] {
	case "image", "document":
		source, _ := block["source"].(map[string]any)
		return source != nil && (source["type"] == "url" || source["type"] == "file")
	case "container_upload":
		return true
	case "input_image":
		return present(block["file_id"]) || remoteURL(block["image_url"])
	case "input_file":
		return present(block["file_id"]) || present(block["file_url"])
	case "image_url":
		return remoteURL(block["image_url"])
	case "file":
		file, _ := block["file"].(map[string]any)
		return file != nil && present(file["file_id"])
	}
	return false
}

// itemReference recognises a Responses input item that names a stored item
// by its id instead of carrying it: an item_reference, or any item with an
// id and nothing else but its type and status.
func itemReference(item map[string]any) bool {
	if item[typeField] == "item_reference" {
		return true
	}
	// A reasoning item replays stored reasoning by id unless it carries the
	// encrypted reasoning itself.
	if encrypted := item["encrypted_content"]; item[typeField] == "reasoning" && (encrypted == nil || encrypted == "") {
		return true
	}
	if _, named := item["id"]; !named {
		return false
	}
	for key := range item {
		if key != "id" && key != typeField && key != "status" {
			return false
		}
	}
	return true
}

func remoteURL(value any) bool {
	if object, ok := value.(map[string]any); ok {
		value = object["url"]
	}
	text, _ := value.(string)
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://")
}

// providerTools reports whether a request enables tools that the provider
// runs itself. Their results become input the request bytes do not bound.
func providerTools(body []byte) bool {
	var request struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
		MCPServers json.RawMessage `json:"mcp_servers"`
		Container  json.RawMessage `json:"container"`
		// Chat completions search the web with web_search_options.
		WebSearch json.RawMessage `json:"web_search_options"`
	}
	if json.Unmarshal(body, &request) != nil {
		return true // unreadable tools may be provider-run
	}
	for _, option := range []json.RawMessage{request.MCPServers, request.Container, request.WebSearch} {
		if len(option) != 0 && !isJSON(option, "null") {
			return true
		}
	}
	for _, tool := range request.Tools {
		for _, hosted := range []string{"web_search", "web_fetch", "file_search", "code_interpreter",
			"code_execution", "image_generation", "mcp"} {
			if strings.HasPrefix(tool.Type, hosted) {
				return true
			}
		}
	}
	return false
}

// modelCall relays one admitted model response and settles its reservation.
type modelCall struct {
	h           *Handler
	ctx         context.Context
	w           http.ResponseWriter
	r           *http.Request
	e           audit.Event
	limits      sprooziv1alpha1.EndpointBudget
	reservation *budget.Reservation
	plan        reservationPlan
	body        []byte
	stream      bool

	cancelUpstream context.CancelFunc
	diagnostic     *ResponseDiagnostic
	credential     string

	// startedAt times the estimate: when the request was fully written, as
	// the provider works from then, or when the response headers arrived if
	// the transport does not report the write. A call that does not stream
	// sends its headers only once generation has finished.
	startedAt   time.Time
	written     atomic.Pointer[time.Time]
	visible     int64
	relayed     int
	anthropic   *anthropicStream
	responses   *responsesStream
	settled     bool
	passthrough bool
	tokensUsed  int64
}

func (c *modelCall) now() time.Time {
	if c.h.Config.Now != nil {
		return c.h.Config.Now()
	}
	return time.Now()
}

// relay passes a successful provider response to the workload as it
// arrives. A streamed response is relayed one whole event at a time, so the
// gateway can end it with an error event; a JSON response is relayed as read.
func (c *modelCall) relay(resp *http.Response) {
	c.startedAt = c.now()
	if at := c.written.Load(); at != nil {
		c.startedAt = *at
	}
	c.e.UpstreamStatus = resp.StatusCode
	c.e.Model = requestModel(c.body)
	contentType := resp.Header.Get("Content-Type")
	if c.stream {
		var typeErr error
		if c.h.protocol == anthropicProtocol {
			mediaType, _, err := mime.ParseMediaType(contentType)
			if strings.TrimSpace(contentType) != "" && (err != nil || mediaType != eventStreamMediaType) {
				typeErr = errors.New("unsupported Anthropic streaming response")
			}
			c.anthropic = &anthropicStream{}
		} else {
			typeErr = checkStreamType(c.r.URL.Path, contentType)
			c.responses = &responsesStream{}
		}
		if typeErr != nil {
			c.observeFailure(typeErr.Error())
			c.settleInterrupted(endUpstreamError)
			c.h.deny(c.w, c.e, http.StatusBadGateway, "unsupported provider response", "server_error", "upstream_error")
			return
		}
	}
	c.h.copyResponseHeaders(c.w.Header(), resp.Header)
	if c.stream && c.r.URL.Path == responsesPath && strings.TrimSpace(contentType) == "" {
		c.w.Header().Set("Content-Type", eventStreamMediaType)
	}
	c.w.WriteHeader(resp.StatusCode)
	if !c.flush() {
		c.settleInterrupted(endClientCancelled)
		c.finish()
		return
	}
	c.copy(resp.Body)
	c.finish()
}

func (c *modelCall) flush() bool {
	err := http.NewResponseController(c.w).Flush()
	return err == nil || errors.Is(err, http.ErrNotSupported)
}

func (c *modelCall) write(p []byte) bool {
	c.relayed += len(p)
	if _, err := c.w.Write(p); err != nil {
		return false
	}
	return c.flush()
}

type readResult struct {
	chunk []byte
	err   error
}

// copy relays the body until it ends, the call ends or the workload leaves.
func (c *modelCall) copy(body io.Reader) {
	reads := make(chan readResult)
	stopped := make(chan struct{})
	defer close(stopped)
	defer c.cancelUpstream()
	go func() {
		for {
			buffer := make([]byte, readChunkBytes)
			n, err := body.Read(buffer)
			if n > 0 || err != nil {
				select {
				case reads <- readResult{buffer[:n], err}:
				case <-stopped:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	idleTimeout := c.h.Config.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var scanner eventScanner
	var buffered []byte
	for {
		select {
		case result := <-reads:
			idle.Reset(idleTimeout)
			if len(result.chunk) > 0 {
				var more bool
				if c.stream {
					more = c.relayEvents(&scanner, result.chunk)
				} else {
					more = c.relayJSON(&buffered, result.chunk)
				}
				if !more {
					return
				}
			}
			if result.err != nil {
				c.ended(result.err, &scanner, buffered)
				return
			}
		case <-tick.C:
			if c.stream && c.overReservation() {
				c.cut()
				return
			}
		case <-idle.C:
			c.observeFailure("provider response was idle too long")
			c.settleInterrupted(endUpstreamError)
			return
		case <-c.ctx.Done():
			c.interrupted(buffered)
			return
		}
	}
}

// interrupted settles a call that ended before its response did. A JSON
// body that had already arrived whole still settles from its usage.
func (c *modelCall) interrupted(buffered []byte) {
	if !c.stream && !c.passthrough && c.settleJSON(buffered, c.cancelReason()) {
		return
	}
	c.settleInterrupted(c.cancelReason())
}

// cancelReason distinguishes the call's own deadline from a workload that
// cancelled, disconnected or lost its connection.
func (c *modelCall) cancelReason() string {
	if errors.Is(context.Cause(c.ctx), proxytransport.ErrModelCallDeadline) {
		return endDeadline
	}
	if errors.Is(context.Cause(c.ctx), errGatewayStopping) {
		return endGatewayRestart
	}
	return endClientCancelled
}

// ended settles a response body that ended or failed to read.
func (c *modelCall) ended(err error, scanner *eventScanner, buffered []byte) {
	if c.ctx.Err() != nil {
		c.interrupted(buffered)
		return
	}
	if !errors.Is(err, io.EOF) {
		c.observeFailure("provider response failed after its headers")
		c.settleInterrupted(endUpstreamError)
		return
	}
	if c.stream {
		if rest := scanner.rest(); len(rest) > 0 {
			c.write(rest)
		}
		if !c.settled {
			c.observeFailure("provider stream ended before its terminal event")
			c.settleInterrupted(endUpstreamError)
		}
		return
	}
	if c.passthrough {
		c.settleInterrupted(endParseLimit)
		return
	}
	if !c.settleJSON(buffered, endCompleted) {
		c.observeFailure("completed response omitted required token usage")
		c.settleInterrupted(endUpstreamError)
	}
}

// settleJSON settles a whole JSON body from its usage, reporting false when
// the body is incomplete or has no valid usage.
func (c *modelCall) settleJSON(buffered []byte, reason string) bool {
	if c.diagnostic != nil {
		format := describeResponse(&http.Response{StatusCode: c.e.UpstreamStatus, Header: http.Header{}}, buffered, c.credential)
		c.diagnostic.BodyFormat, c.diagnostic.Events, c.diagnostic.TerminalEvent = format.BodyFormat, format.Events, format.TerminalEvent
		c.diagnostic.ErrorCode, c.diagnostic.ErrorParam, c.diagnostic.ErrorMessage = format.ErrorCode, format.ErrorParam, format.ErrorMessage
	}
	if c.h.protocol == anthropicProtocol {
		usage, err := decodeAnthropicUsage(buffered, false, "")
		if err != nil {
			return false
		}
		c.settleProvider(anthropicResponseUsage(usage), usage, reason)
		return true
	}
	usage, err := decodeUsage(buffered, c.r.URL.Path, false, "")
	if err != nil || invalidTokenUsage(usage, nil, c.limits) {
		return false
	}
	c.settleProvider(usage, anthropicUsage{}, reason)
	return true
}

// relayJSON relays a JSON chunk as read and keeps the body for its usage.
func (c *modelCall) relayJSON(buffered *[]byte, chunk []byte) bool {
	c.visible += int64(len(chunk))
	if !c.passthrough {
		if len(*buffered)+len(chunk) > maxEventBytes {
			c.passthrough, *buffered = true, nil
		} else {
			*buffered = append(*buffered, chunk...)
		}
	}
	if !c.write(chunk) {
		c.interrupted(*buffered)
		return false
	}
	return true
}

// relayEvents relays each whole event in chunk. An event larger than the
// parser's bound turns the rest of the response into an unparsed relay.
func (c *modelCall) relayEvents(scanner *eventScanner, chunk []byte) bool {
	if c.passthrough || c.settled {
		if !c.write(chunk) {
			if !c.settled {
				c.settleInterrupted(endClientCancelled)
			}
			return false
		}
		return true
	}
	scanner.add(chunk)
	for frame := scanner.next(); frame != nil; frame = scanner.next() {
		if len(frame) > maxEventBytes {
			return c.overflow(append(frame, scanner.rest()...))
		}
		if !c.relayEvent(frame) {
			return false
		}
		if c.settled {
			// Relay whatever follows the terminal event unparsed.
			if rest := scanner.rest(); len(rest) > 0 && !c.write(rest) {
				return false
			}
			return true
		}
	}
	if scanner.pending() > maxEventBytes {
		return c.overflow(scanner.rest())
	}
	return true
}

// overflow relays the rest of a response unparsed once an event outgrew the
// parser's bound.
func (c *modelCall) overflow(rest []byte) bool {
	c.passthrough = true
	c.observeFailure(reasonParseLimit)
	if !c.write(rest) {
		c.settleInterrupted(endClientCancelled)
		return false
	}
	return true
}

// relayEvent validates one event, then relays it unless the call must end.
func (c *modelCall) relayEvent(raw []byte) bool {
	frame := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	if c.diagnostic != nil {
		c.diagnostic.observeFrame(frame, c.credential)
	}
	var err error
	if c.anthropic != nil {
		err = c.anthropic.next(frame)
		c.visible = c.anthropic.visible
	} else {
		err = c.responses.next(frame)
		c.visible = c.responses.visible
	}
	if err != nil {
		// The provider's own error event still reaches the workload.
		c.write(raw)
		c.observeFailure(err.Error())
		c.settleInterrupted(endUpstreamError)
		return false
	}
	if c.anthropic != nil && c.anthropic.stopped() {
		if !c.write(raw) {
			c.settleCompleted()
			return false
		}
		c.settleCompleted()
		return true
	}
	if c.responses != nil && c.responses.terminal != "" {
		ok := c.write(raw)
		c.settleCompleted()
		return ok
	}
	if c.overReservation() {
		c.cut()
		return false
	}
	if !c.write(raw) {
		c.settleInterrupted(endClientCancelled)
		return false
	}
	return true
}

// settleCompleted settles a stream that reached its terminal event.
func (c *modelCall) settleCompleted() {
	if c.anthropic != nil {
		usage, err := c.anthropic.finish()
		if err != nil {
			c.observeFailure(err.Error())
			c.settleInterrupted(endUpstreamError)
			return
		}
		c.settleProvider(anthropicResponseUsage(usage), usage, endCompleted)
		return
	}
	usage, err := c.responses.finish()
	if err != nil || invalidTokenUsage(usage, nil, c.limits) {
		c.observeFailure("terminal event omitted required token usage")
		c.settleInterrupted(endUpstreamError)
		return
	}
	reason := endCompleted
	if c.responses.terminal == responseFailedEvent {
		reason = endUpstreamError
	}
	c.settleProvider(usage, anthropicUsage{}, reason)
}

func anthropicResponseUsage(usage anthropicUsage) responseUsage {
	input := usage.Total - usage.Output
	return responseUsage{Usage: &tokenUsage{TotalTokens: &usage.Total, PromptTokens: &input, CompletionTokens: &usage.Output}}
}

// cut ends a stream whose running estimate reached its reservation: the
// workload receives the protocol's error event, the provider call is
// cancelled and the reservation is charged.
func (c *modelCall) cut() {
	c.cancelUpstream()
	event := `event: error` + "\n" + `data: {"type":"error","code":"budget_exhausted","message":"` + budgetCutMessage + `","param":null}` + "\n\n"
	if c.anthropic != nil {
		event = `event: error` + "\n" + `data: {"type":"error","error":{"type":"budget_error","message":"` + budgetCutMessage + `"}}` + "\n\n"
	}
	c.write([]byte(event))
	c.observeFailure(reasonBudgetCut)
	c.chargeReservation(endBudgetCut, reasonBudgetCut)
}

func (c *modelCall) observeFailure(reason string) {
	if c.diagnostic != nil && c.diagnostic.ValidationError == "" {
		c.diagnostic.ValidationError = reason
	}
}

// overReservation reports whether an unbounded call's observed usage reached
// what it reserved, in either limited dimension. Observed usage is the
// estimate's input plus the visible output bytes ÷ 4. The time floor prices
// only an interrupt. A bounded call is never cut: its reservation covers its
// input and the output bound the provider enforces, and usage above it
// settles as exceeded.
func (c *modelCall) overReservation() bool {
	if c.reservation == nil || c.plan.bounded || c.settled || c.passthrough || !c.plan.sizable {
		return false
	}
	heldUnits, heldCost := c.reservation.Held()
	units, cost, _ := c.usage(ceilDiv(c.visible, inputBytesPerToken, 1))
	return (heldUnits > 0 && units >= heldUnits) || (heldCost > 0 && cost >= heldCost)
}

// estimate is E for a call interrupted now, before the reservation cap.
// Input is Anthropic's exact usage once message_start has arrived, and
// otherwise the request bytes divided by inputBytesPerToken. Output is the
// visible output bytes or the elapsed time at R tokens per second, whichever
// is more, or Anthropic's exact output once message_delta has reported it,
// never more than the request's output bound.
func (c *modelCall) estimate() (units, costMicros int64, inputSource string) {
	rate := c.h.Config.OutputTokensPerSecond
	if rate <= 0 {
		rate = DefaultOutputTokensPerSecond
	}
	elapsed := max(c.now().Sub(c.startedAt), 0)
	output := max(c.visible, ceilDiv(elapsed.Nanoseconds(), int64(time.Second), rate))
	if c.anthropic != nil && c.anthropic.delta {
		// message_delta reported the exact output so far.
		output = c.anthropic.usage.Output
	}
	if c.plan.outputBound > 0 {
		output = min(output, c.plan.outputBound)
	}
	return c.usage(output)
}

// usage prices the call's input and the given output tokens. Input is
// Anthropic's exact usage once message_start has arrived, and otherwise the
// request bytes divided by inputBytesPerToken.
func (c *modelCall) usage(output int64) (units, costMicros int64, inputSource string) {
	inputSource = inputFromRequest
	input := ceilDiv(int64(len(c.body)), inputBytesPerToken, 1)
	var exact anthropicUsage
	if c.anthropic != nil && c.anthropic.started() {
		exact = c.anthropic.usage
		input, inputSource = exact.Input+exact.CacheRead+exact.CacheWrite5m+exact.CacheWrite1h, inputFromProvider
	}
	units = input + output
	if c.limits.MaxCostMicros > 0 {
		price := c.h.price(requestModel(c.body))
		rateOr := func(rate int64) int64 {
			if rate > 0 {
				return rate
			}
			return price.InputMicrosPerMillionTokens
		}
		categories := [][2]int64{{input, price.InputMicrosPerMillionTokens}}
		if inputSource == inputFromProvider {
			categories = [][2]int64{
				{exact.Input, price.InputMicrosPerMillionTokens},
				{exact.CacheRead, price.CachedInputMicrosPerMillionTokens},
				{exact.CacheWrite5m, rateOr(price.CacheWrite5mMicrosPerMillionTokens)},
				{exact.CacheWrite1h, rateOr(price.CacheWrite1hMicrosPerMillionTokens)},
			}
		}
		numerator := new(big.Int).Mul(big.NewInt(output), big.NewInt(price.OutputMicrosPerMillionTokens))
		for _, category := range categories {
			numerator.Add(numerator, new(big.Int).Mul(big.NewInt(category[0]), big.NewInt(category[1])))
		}
		costMicros, _ = roundedCost(numerator)
	}
	return units, costMicros, inputSource
}

// ceilDiv returns ceil(value × multiplier / divisor) for small non-negative values.
func ceilDiv(value, divisor, multiplier int64) int64 {
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(multiplier))
	quotient, remainder := new(big.Int).QuoRem(product, big.NewInt(divisor), new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 1<<63 - 1
	}
	return quotient.Int64()
}

// settleProvider settles the provider's reported usage. Usage above the
// reservation was already relayed: it is charged up to the reservation and
// the allowance, and audited as exceeded.
func (c *modelCall) settleProvider(usage responseUsage, anthropic anthropicUsage, reason string) {
	if c.settled {
		return
	}
	c.settled = true
	tokens := *usage.Usage.TotalTokens
	c.e.InputTokens, c.e.OutputTokens = splitTokens(usage.Usage)
	var cost int64
	if c.limits.MaxCostMicros > 0 {
		var err error
		if cost, err = c.h.providerCost(c.body, usage, anthropic); err != nil {
			c.settled = false
			c.chargeReservation(reason, reasonUnpriced)
			return
		}
	}
	c.e.EndReason = reason
	if c.reservation != nil {
		if err := c.reservation.Settle(tokens, cost); errors.Is(err, budget.ErrUsageExceeded) {
			c.e.EndReason = endExceeded
		} else if err != nil {
			c.h.settleFailed(&c.e, err)
			return
		}
	}
	c.e.UsageSource, c.e.TokensUsed, c.tokensUsed = usageProvider, tokens, tokens
}

// settleInterrupted charges an interrupted call the estimate E, capped at its
// reservation, or the whole reservation when the request's input cannot be
// sized or an event outgrew the parser.
func (c *modelCall) settleInterrupted(reason string) {
	if c.settled {
		return
	}
	if c.passthrough {
		c.chargeReservation(endParseLimit, reasonParseLimit)
		return
	}
	if !c.plan.sizable {
		c.chargeReservation(reason, reasonUnsizable)
		return
	}
	c.settled = true
	units, cost, inputSource := c.estimate()
	heldUnits, heldCost := c.reservation.Held()
	if heldUnits > 0 {
		units = min(units, heldUnits)
	}
	if heldCost > 0 {
		cost = min(cost, heldCost)
	}
	c.e.EndReason = reason
	if c.reservation != nil {
		if err := c.reservation.Settle(units, cost); err != nil && !errors.Is(err, budget.ErrUsageExceeded) {
			c.h.settleFailed(&c.e, err)
			return
		}
	}
	c.h.logger().Info("Charged estimated model usage", "runID", c.e.RunID, "runUID", c.e.RunUID,
		"endReason", reason, "units", units, "costMicros", cost, "inputSource", inputSource,
		"visibleBytes", c.visible, "elapsed", max(c.now().Sub(c.startedAt), 0).String())
	c.e.UsageSource, c.e.EndReason, c.e.TokensUsed, c.tokensUsed = usageEstimated, reason, units, units
}

func (c *modelCall) chargeReservation(reason, why string) {
	if c.settled {
		return
	}
	c.settled = true
	c.e.UsageSource, c.e.EndReason = usageReservation, reason
	c.h.forfeit(&c.e, c.reservation, why)
	c.tokensUsed = c.e.TokensUsed
}

// finish audits the relayed call once it has settled.
func (c *modelCall) finish() {
	if c.diagnostic != nil {
		c.diagnostic.BodyBytes = c.relayed
		if c.stream {
			c.diagnostic.BodyFormat = sseFormat
		}
	}
	c.h.auditModelSuccess(c.r.Context(), c.e, c.e.UpstreamStatus, c.tokensUsed, budget.FromContext(c.r.Context()), c.limits)
}

// eventScanner splits an event stream into whole events, keeping only the
// incomplete one. An event ends with an empty line, LF or CRLF.
type eventScanner struct {
	buffer  []byte
	scanned int
}

func (s *eventScanner) add(chunk []byte) { s.buffer = append(s.buffer, chunk...) }

func (s *eventScanner) pending() int { return len(s.buffer) }

// next returns the next whole event with its original bytes, or nil.
func (s *eventScanner) next() []byte {
	for i := max(s.scanned, 1); i < len(s.buffer); i++ {
		if s.buffer[i] != '\n' {
			continue
		}
		if s.buffer[i-1] == '\n' || (i >= 2 && s.buffer[i-1] == '\r' && s.buffer[i-2] == '\n') {
			frame := s.buffer[: i+1 : i+1]
			s.buffer, s.scanned = s.buffer[i+1:], 0
			return frame
		}
	}
	s.scanned = len(s.buffer)
	return nil
}

// rest returns and clears the bytes not yet returned as events.
func (s *eventScanner) rest() []byte {
	rest := s.buffer
	s.buffer, s.scanned = nil, 0
	return rest
}

// detachedWork refuses request options whose provider work the response does
// not carry or bound. A background response returns at once without usage
// while the provider runs the whole job, and n above 1 generates n outputs
// under one output bound.
func detachedWork(body []byte) error {
	if len(body) == 0 {
		return nil // model discovery has no body
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return errNotObject
	}
	if raw, present := request["background"]; present && !isJSON(raw, "false", "null") {
		return errors.New("background responses are not supported by the model gateway; " +
			"background must be omitted or the boolean false")
	}
	for _, key := range []string{"n", "best_of"} {
		if raw, present := request[key]; present && !isJSON(raw, "null") && !numberIsOne(raw) {
			return fmt.Errorf("%s must be 1 (or omitted): the model gateway bounds one output per request; "+
				"send one request per output", key)
		}
	}
	return nil
}

// numberIsOne reports whether raw is a JSON number equal to 1, such as 1,
// 1.0 or 1e0. An exponent too large to evaluate cannot be 1 here.
func numberIsOne(raw json.RawMessage) bool {
	text := string(bytes.TrimSpace(raw))
	if !json.Valid(raw) || strings.HasPrefix(text, `"`) {
		return false
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(text[i+1:])
		if err != nil || exponent < -400 || exponent > 400 {
			return false
		}
	}
	value, ok := new(big.Rat).SetString(text)
	return ok && value.Cmp(big.NewRat(1, 1)) == 0
}

func isJSON(raw json.RawMessage, values ...string) bool {
	return slices.Contains(values, string(bytes.TrimSpace(raw)))
}

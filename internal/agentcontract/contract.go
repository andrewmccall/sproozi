// Package agentcontract defines the versioned data exchanged between Sproozi's
// control plane and an agent runner. It is intentionally independent of Pods
// and CRDs so all producers and consumers cross the same strict seam.
package agentcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// Version identifies this contract revision. Consumers reject other values.
	Version = "sproozi.agentcontract/v1alpha1"

	maxInputBytes        = 64 << 10
	maxEventBytes        = 4 << 10
	maxInstructionsRunes = 16 << 10
	maxTaskRunes         = 16 << 10
	maxEventContextBytes = 16 << 10
	maxCapabilities      = 32
	maxLifecycleEvents   = 1 << 20
)

var (
	errInvalidContract = errors.New("agentcontract: invalid contract")
	dnsLabel           = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	repositoryName     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	identifier         = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	uid                = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

// Input is the complete runner input. Trusted and untrusted content are
// separate fields by design; callers must never concatenate them implicitly.
type Input struct {
	Version      string         `json:"version"`
	Run          RunIdentity    `json:"run"`
	Trusted      TrustedInput   `json:"trusted"`
	Untrusted    UntrustedInput `json:"untrusted"`
	Capabilities []string       `json:"capabilities"`
	Workspace    string         `json:"workspace"`
	Gateway      GatewayBinding `json:"gateway"`
}

type RunIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type TrustedInput struct {
	Instructions string `json:"instructions"`
}

// UntrustedInput holds caller- or webhook-provided data. EventContext is an
// opaque JSON object so templates cannot mistake it for operating instructions.
type UntrustedInput struct {
	Task         string          `json:"task"`
	EventContext json.RawMessage `json:"eventContext"`
}

// GatewayBinding is the single workload identity and shared proxy endpoint.
type GatewayBinding struct {
	Endpoint  string `json:"endpoint"`
	TokenPath string `json:"tokenPath"`
}

// LifecycleEvent is an audit-safe, fixed-shape event. It deliberately has no
// prompt, response, command, output, patch, payload, or credential field.
type LifecycleEvent struct {
	Version       string      `json:"version"`
	Run           RunIdentity `json:"run"`
	Sequence      uint64      `json:"sequence"`
	Type          string      `json:"type"`
	Component     string      `json:"component"`
	Operation     string      `json:"operation"`
	Outcome       string      `json:"outcome"`
	Reason        string      `json:"reason,omitempty"`
	Repository    string      `json:"repository,omitempty"`
	PRNumber      int64       `json:"prNumber,omitempty"`
	CorrelationID string      `json:"correlationID,omitempty"`
}

func DecodeInput(data []byte) (Input, error) {
	var input Input
	if err := decodeStrict(data, maxInputBytes, &input); err != nil {
		return Input{}, err
	}
	if err := input.Validate(); err != nil {
		return Input{}, err
	}
	return input, nil
}

func DecodeLifecycleEvent(data []byte) (LifecycleEvent, error) {
	var event LifecycleEvent
	if err := decodeStrict(data, maxEventBytes, &event); err != nil {
		return LifecycleEvent{}, err
	}
	if err := event.Validate(); err != nil {
		return LifecycleEvent{}, err
	}
	return event, nil
}

func (i Input) Validate() error {
	if i.Version != Version {
		return invalid("unsupported version")
	}
	if err := i.Run.validate(); err != nil {
		return err
	}
	if !boundedText(i.Trusted.Instructions, maxInstructionsRunes) {
		return invalid("trusted instructions are required and bounded")
	}
	if !boundedText(i.Untrusted.Task, maxTaskRunes) {
		return invalid("untrusted task is required and bounded")
	}
	if len(i.Untrusted.EventContext) == 0 || len(i.Untrusted.EventContext) > maxEventContextBytes || !isJSONObject(i.Untrusted.EventContext) {
		return invalid("event context must be a bounded JSON object")
	}
	if err := rejectDuplicateKeys(i.Untrusted.EventContext); err != nil {
		return invalid("event context has invalid JSON")
	}
	if len(i.Capabilities) == 0 || len(i.Capabilities) > maxCapabilities {
		return invalid("capabilities are required and bounded")
	}
	seen := make(map[string]struct{}, len(i.Capabilities))
	for _, capability := range i.Capabilities {
		if !identifier.MatchString(capability) {
			return invalid("invalid capability")
		}
		if _, ok := seen[capability]; ok {
			return invalid("duplicate capability")
		}
		seen[capability] = struct{}{}
	}
	if !safeAbsolutePath(i.Workspace) || !safeAbsolutePath(i.Gateway.TokenPath) {
		return invalid("invalid required path")
	}
	if !safeGatewayURL(i.Gateway.Endpoint) {
		return invalid("invalid gateway endpoint")
	}

	return nil
}

func (e LifecycleEvent) Validate() error {
	if e.Version != Version {
		return invalid("unsupported version")
	}
	if err := e.Run.validate(); err != nil {
		return err
	}
	if e.Sequence == 0 || e.Sequence > maxLifecycleEvents {
		return invalid("invalid event sequence")
	}
	for _, value := range []string{e.Type, e.Component, e.Operation, e.Outcome} {
		if !identifier.MatchString(value) {
			return invalid("invalid lifecycle field")
		}
	}
	for _, value := range []string{e.Reason, e.CorrelationID} {
		if value != "" && !identifier.MatchString(value) {
			return invalid("invalid lifecycle metadata")
		}
	}
	if e.Repository != "" && !repositoryName.MatchString(e.Repository) {
		return invalid("invalid lifecycle repository")
	}
	if e.PRNumber < 0 || (e.PRNumber > 0 && e.Repository == "") {
		return invalid("invalid lifecycle pull request")
	}
	return nil
}

func (r RunIdentity) validate() error {
	if !dnsLabel.MatchString(r.Namespace) || !dnsLabel.MatchString(r.Name) || !uid.MatchString(r.UID) {
		return invalid("invalid run identity")
	}
	return nil
}

func decodeStrict(data []byte, max int, target any) error {
	if len(data) == 0 || len(data) > max {
		return invalid("document size")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalid("invalid JSON: " + err.Error())
	}
	if err := ensureEOF(decoder); err != nil {
		return invalid("multiple JSON values")
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := inspectJSONValue(decoder, 0); err != nil {
		return err
	}
	return ensureEOF(decoder)
}

func inspectJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("nesting too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate key %q", name)
			}
			seen[name] = struct{}{}
			if err := inspectJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func ensureEOF(decoder *json.Decoder) error {
	_, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra JSON value")
	}
	return err
}
func isJSONObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) >= 2 && raw[0] == '{' && raw[len(raw)-1] == '}'
}
func safeAbsolutePath(value string) bool {
	return value != "" && len(value) <= 256 && path.IsAbs(value) && path.Clean(value) == value && !strings.Contains(value, "\\")
}
func safeGatewayURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "" && len(value) <= 512
}
func boundedText(value string, max int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= max && !strings.ContainsRune(value, '\x00')
}
func invalid(reason string) error { return fmt.Errorf("%w: %s", errInvalidContract, reason) }

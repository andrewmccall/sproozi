// Package profile transports one fixed trusted placement over authenticated TLS.
// It never exposes Kubernetes credentials, resource templates or an exec API.
package profile

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

const protocolVersion = 1
const jsonMedia = "application/json"
const headerContentType = "Content-Type"
const headerAuthorization = "Authorization"
const bearerPrefix = "Bearer "
const metadataLimit = int64(96 << 10)
const maxArtifactLimit = int64(256 << 20)
const (
	actionPrepare  = "prepare"
	actionActivate = "activate"
	actionStop     = "stop"
	actionInspect  = "inspect"
	actionAbort    = "abort"
	actionAttach   = "attach"
	actionDescribe = "describe"
)

// Code describes a stable transport outcome. Unavailable never means absence.
type Code string

const (
	CodeInvalid     Code = "invalid_request"
	CodeDenied      Code = "denied"
	CodeConflict    Code = "conflict"
	CodeExpired     Code = "expired"
	CodeUnsupported Code = "unsupported"
	CodeUnavailable Code = "unavailable"
)

// Rejection marks a backend's known semantic refusal. Ordinary errors remain
// unavailable/unknown; callers cannot mistake a timeout for rejected activation.
type Rejection struct{ Code Code }

func (r *Rejection) Error() string { return "session profile: " + string(r.Code) }

// RemoteError preserves a server's explicit status and stable outcome.
// Transport, when set, is the local client's own failure to reach the server.
// It stays out of Error, which callers may relay, so only a trusted local
// caller such as the CLI shows it.
type RemoteError struct {
	Code      Code
	Status    int
	Transport error
}

func (r *RemoteError) Error() string {
	if r.Status == 0 {
		return fmt.Sprintf("session profile: %s (no HTTP response)", r.Code)
	}
	return fmt.Sprintf("session profile: %s (HTTP %d)", r.Code, r.Status)
}

type request struct {
	Version     int                  `json:"version"`
	Launch      session.Launch       `json:"launch"`
	Incarnation *session.Incarnation `json:"incarnation,omitempty"`
	Checkpoint  []byte               `json:"checkpoint,omitempty"`
	// Profile is the archive a prepared launch names by ProfileDigest.
	Profile []byte `json:"profile,omitempty"`
}
type observation struct {
	Launch      session.Launch         `json:"launch"`
	Incarnation session.Incarnation    `json:"incarnation"`
	State       session.PlacementState `json:"state"`
	Stopped     *session.Stopped       `json:"stopped,omitempty"`
}

// Description is what a remote executor states about itself: the placement
// name launches must carry, its harness and its checkpoint byte limit.
type Description struct {
	Placement          string             `json:"placement"`
	Harness            harness.NativeKind `json:"harness"`
	MaxCheckpointBytes int64              `json:"maxCheckpointBytes"`
}

type response struct {
	Version     int                  `json:"version"`
	Executor    *Description         `json:"executor,omitempty"`
	Incarnation *session.Incarnation `json:"incarnation,omitempty"`
	Observation *observation         `json:"observation,omitempty"`
	Stopped     *session.Stopped     `json:"stopped,omitempty"`
	Checkpoint  []byte               `json:"checkpoint,omitempty"`
	Error       Code                 `json:"error,omitempty"`
}

func bodyLimit(max int64) (int64, error) {
	if max < 1 || max > maxArtifactLimit {
		return 0, errors.New("profile: invalid checkpoint byte limit")
	}
	return ((max+2)/3)*4 + agentprofile.EncodedArchiveBytes + metadataLimit, nil
}
func strictDecode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("profile: trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(target)
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 12 {
		return errors.New("profile: excessive JSON nesting")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			tok, err = d.Token()
			if err != nil {
				return err
			}
			key, ok := tok.(string)
			key = strings.ToLower(key)
			if !ok || keys[key] {
				return errors.New("profile: duplicate JSON key")
			}
			keys[key] = true
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err = uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("profile: invalid JSON")
	}
	_, err = d.Token()
	return err
}
func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}
func validName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
func validDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && value == strings.ToLower(value)
}
func validateLaunch(l session.Launch) error {
	if err := workspace.ValidateIgnoredPaths(l.IncludeIgnored); err != nil {
		return err
	}
	if l.Harness.Validate() != nil || !validID(l.ExecutionID) || !validID(l.OperationID) || !validID(l.AllocationID) || !validID(l.ThreadID) || l.Epoch == 0 || !validName(l.Placement) || !validDigest(l.InstructionDigest) || !validDigest(l.CheckpointDigest) || l.Deadline.IsZero() || len(l.Allowances) == 0 || len(l.Allowances) > 32 ||
		l.ProfileDigest != "" && !agentprofile.ValidDigest(l.ProfileDigest) {
		return errors.New("profile: invalid launch")
	}
	if len(l.HarnessArgs) != 0 {
		// Harness arguments apply to local Docker visits only.
		return errors.New("profile: harness arguments are not supported on remote placements")
	}
	for capability, a := range l.Allowances {
		if !validName(capability) || !validLimit(a.Units) || !validLimit(a.CostMicros) {
			return errors.New("profile: invalid allowance")
		}
	}
	return nil
}
func validLimit(l session.Limit) bool { return l.Value >= 0 && (l.Bounded || l.Value == 0) }
func incarnationValid(i session.Incarnation) bool {
	return validName(i.ExecutionID) && validName(i.ResourceUID)
}
func sameLaunch(a, b session.Launch) bool {
	a.Deadline = a.Deadline.UTC()
	b.Deadline = b.Deadline.UTC()
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}
func limitFits(want, ceiling session.Limit) bool {
	return !ceiling.Bounded || want.Bounded && want.Value <= ceiling.Value
}
func ensureAlive(deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return &Rejection{Code: CodeExpired}
	}
	return nil
}

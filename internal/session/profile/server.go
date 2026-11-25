package profile

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
)

// Config fixes one authenticated principal, placement and immutable profile.
// Backend must itself be configured for this principal and retain owner binding,
// exact operation intent, actual writer stop, accounting seal and policy checks.
// MaxDuration, when set, caps the original-deadline request horizon and denies
// a launch with no deadline; zero admits any deadline, none included (ADR
// 0056). The backend also enforces its stricter admission/runtime deadline
// without rewriting immutable Launch.
type Config struct {
	Harness            harness.NativeKind
	Principal          string
	Placement          string
	InstructionDigest  string
	Allowances         map[string]session.Allowance
	MaxDuration        time.Duration
	MaxCheckpointBytes int64
	// Authorizer decides who the home principal is.
	Authorizer Authorizer
	Backend    session.Placement
	// Profiles retains profile archives that launches name. Without it, a
	// launch with a profile is unsupported.
	Profiles ProfileStore
	// Stops tracks the stops the handler carries on after their requests
	// end, so the executor can drain them at shutdown. Without it the handler
	// keeps its own.
	Stops *Stops
}

// ProfileStore retains a verified profile archive under its digest.
type ProfileStore interface {
	StoreProfile(ctx context.Context, digest string, archive []byte) error
}
type handler struct {
	config  Config
	home    Authorizer
	maxBody int64
	// stops are the stops this executor carries on after their requests
	// end, each for at most stopBound.
	stops     *Stops
	stopBound time.Duration
}

// ErrUnauthenticated means a bearer token identifies no caller at all.
var ErrUnauthenticated = errors.New("profile: unauthenticated")

// Authorizer decides whether a bearer token belongs to the home principal. It
// returns nil for the principal, ErrUnauthenticated for an unknown token, a
// *Rejection with CodeDenied for a known caller that is not the principal, and
// any other error when it cannot decide, which the caller sees as unavailable.
type Authorizer interface {
	Authorize(ctx context.Context, token string) error
}

// AuthorizationStatus maps an Authorizer result to the HTTP status and code a
// handler answers with; status 0 means the caller is the home principal.
func AuthorizationStatus(err error) (int, Code) {
	var refusal *Rejection
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized, CodeDenied
	case errors.As(err, &refusal) && refusal.Code == CodeDenied:
		return http.StatusForbidden, CodeDenied
	}
	return http.StatusServiceUnavailable, CodeUnavailable
}

// NewHandler requires direct TLS 1.3 requests and authenticates before decoding.
// It serves fixed placement actions only. No cookies, origins or CORS are accepted.
func NewHandler(config Config) (http.Handler, error) {
	maxBody, err := bodyLimit(config.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	if config.Harness.Validate() != nil || !validName(config.Principal) || !validName(config.Placement) || !validDigest(config.InstructionDigest) || config.Backend == nil || config.MaxDuration < 0 || len(config.Allowances) == 0 || len(config.Allowances) > 32 {
		return nil, errors.New("profile: invalid fixed configuration")
	}
	allowed := make(map[string]session.Allowance, len(config.Allowances))
	for name, a := range config.Allowances {
		// Bounded-zero exception (session.Limit.Valid): a fixed allowance
		// may be exhausted.
		if !validName(name) || !a.Units.Valid() && !a.Units.Exhausted() || !a.CostMicros.Valid() && !a.CostMicros.Exhausted() {
			return nil, errors.New("profile: invalid fixed allowance")
		}
		allowed[name] = a
	}
	config.Allowances = allowed
	home := config.Authorizer
	if home == nil {
		return nil, errors.New("profile: home principal authorizer required")
	}
	config.Authorizer = nil
	stops := config.Stops
	if stops == nil {
		stops = &Stops{}
	}
	return &handler{config: config, home: home, maxBody: maxBody, stops: stops, stopBound: StopBound}, nil
}

// validToken trims a bearer token and checks the length and characters every
// home principal and work token needs.
func validToken(raw string) (string, error) {
	token := strings.TrimSpace(raw)
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \r\n\t") {
		return "", errors.New("profile: token needs at least 32 non-whitespace characters")
	}
	return token, nil
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(headerContentType, jsonMedia)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	action, status, code := h.requestAction(r)
	if status != 0 {
		writeError(w, status, code)
		return
	}
	if action == actionAttach {
		h.attach(w, r)
		return
	}
	if action == actionDescribe {
		h.describe(w, r)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get(headerContentType))
	if err != nil || media != jsonMedia {
		writeError(w, http.StatusUnsupportedMediaType, CodeInvalid)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, CodeInvalid)
		return
	}
	var input request
	if err = strictDecode(data, &input); err != nil || input.Version != protocolVersion || validateLaunch(input.Launch) != nil || input.Launch.Placement != h.config.Placement {
		writeError(w, http.StatusBadRequest, CodeInvalid)
		return
	}
	if err = h.validateAction(action, input); err != nil {
		h.fail(w, action, err)
		return
	}
	output, err := h.perform(r, action, input)
	if err != nil {
		h.fail(w, action, err)
		return
	}
	if err = validateResponse(action, input, output, h.config.MaxCheckpointBytes); err != nil {
		h.fail(w, action, err)
		return
	}
	encoded, err := json.Marshal(output)
	if err != nil || int64(len(encoded)) > h.maxBody {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
func (h *handler) requestAction(r *http.Request) (string, int, Code) {
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS13 {
		return "", http.StatusUpgradeRequired, CodeDenied
	}
	authorization := r.Header.Values(headerAuthorization)
	if len(authorization) != 1 || !strings.HasPrefix(authorization[0], bearerPrefix) {
		return "", http.StatusUnauthorized, CodeDenied
	}
	if status, code := AuthorizationStatus(h.home.Authorize(r.Context(), strings.TrimPrefix(authorization[0], bearerPrefix))); status != 0 {
		return "", status, code
	}
	if len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Cookie")) > 0 {
		return "", http.StatusForbidden, CodeDenied
	}
	action := strings.TrimPrefix(r.URL.Path, "/v1/")
	if r.URL.Path != "/v1/"+action || r.URL.RawQuery != "" || r.URL.ForceQuery || r.Method != http.MethodPost {
		return "", http.StatusNotFound, CodeInvalid
	}
	switch action {
	case actionPrepare, actionActivate, actionStop, actionInspect, actionAbort, actionAbortPreparation, actionExited, actionAttach, actionDescribe:
	default:
		return "", http.StatusNotFound, CodeInvalid
	}
	return action, 0, ""
}

// describe states the executor's fixed placement name, harness and checkpoint
// limit, so a home that reached it through a Kubernetes context can name it and
// check the limit without hand-copied configuration. The request carries only
// the protocol version.
func (h *handler) describe(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get(headerContentType))
	if err != nil || media != jsonMedia {
		writeError(w, http.StatusUnsupportedMediaType, CodeInvalid)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, metadataLimit))
	var input struct {
		Version int `json:"version"`
	}
	if err != nil || strictDecode(data, &input) != nil || input.Version != protocolVersion {
		writeError(w, http.StatusBadRequest, CodeInvalid)
		return
	}
	encoded, err := json.Marshal(response{Version: protocolVersion, Executor: &Description{
		Placement: h.config.Placement, Harness: h.config.Harness, MaxCheckpointBytes: h.config.MaxCheckpointBytes,
	}})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (h *handler) validateAction(action string, input request) error {
	if err := h.validatePayload(action, input); err != nil {
		return err
	}
	return h.validateAdmission(action, input.Launch)
}
func (h *handler) validatePayload(action string, input request) error {
	if action == actionPrepare {
		if input.Incarnation != nil || len(input.Checkpoint) == 0 {
			return &Rejection{Code: CodeInvalid}
		}
		named := input.Launch.ProfileDigest != ""
		if named != (len(input.Profile) > 0) || named && agentprofile.Verify(input.Launch.ProfileDigest, input.Profile) != nil {
			return &Rejection{Code: CodeInvalid}
		}
		meta, err := checkpoint.Inspect(input.Checkpoint, h.config.MaxCheckpointBytes)
		if err != nil || meta.Harness != input.Launch.Harness || meta.ThreadID != input.Launch.ThreadID || meta.SHA256 != input.Launch.CheckpointDigest {
			return &Rejection{Code: CodeInvalid}
		}
	} else if len(input.Checkpoint) > 0 || len(input.Profile) > 0 {
		return &Rejection{Code: CodeInvalid}
	}
	if action == actionInspect || action == actionAbortPreparation {
		if input.Incarnation != nil {
			return &Rejection{Code: CodeInvalid}
		}
	} else if action != actionPrepare && (input.Incarnation == nil || !incarnationValid(*input.Incarnation)) {
		return &Rejection{Code: CodeInvalid}
	}
	return nil
}
func (h *handler) validateAdmission(action string, l session.Launch) error {
	if l.Harness != h.config.Harness {
		return &Rejection{Code: CodeDenied}
	}
	if action == actionPrepare || action == actionActivate {
		if err := ensureAlive(l.Deadline); err != nil {
			return err
		}
		if l.InstructionDigest != h.config.InstructionDigest || !deadline.At(l.Deadline).Fits(time.Now(), h.config.MaxDuration) {
			return &Rejection{Code: CodeDenied}
		}
		for name, want := range l.Allowances {
			ceiling, ok := h.config.Allowances[name]
			if !ok || !want.Units.Fits(ceiling.Units) || !want.CostMicros.Fits(ceiling.CostMicros) || want.Units.Exhausted() || want.CostMicros.Exhausted() {
				return &Rejection{Code: CodeDenied}
			}
		}
	}
	return nil
}
func (h *handler) perform(r *http.Request, action string, input request) (response, error) {
	output := response{Version: protocolVersion}
	p := h.config.Backend
	switch action {
	case actionPrepare:
		if digest := input.Launch.ProfileDigest; digest != "" {
			if h.config.Profiles == nil {
				return output, &Rejection{Code: CodeUnsupported}
			}
			if err := h.config.Profiles.StoreProfile(r.Context(), digest, input.Profile); err != nil {
				return output, err
			}
		}
		i, err := p.Prepare(r.Context(), input.Launch, input.Checkpoint)
		if err != nil {
			return output, err
		}
		if !incarnationValid(i) {
			return output, errors.New("invalid backend incarnation")
		}
		output.Incarnation = &i
	case actionActivate:
		if err := p.Activate(r.Context(), input.Launch, *input.Incarnation); err != nil {
			return output, err
		}
	case actionAbortPreparation:
		// Unlike abort, it interrupts no stop: a stop names an incarnation
		// the preparation never reported, and a visit with a stop is
		// refused as not a preparation once the stop lets go of its lock.
		aborter, ok := p.(session.PreparationAborter)
		if !ok {
			return output, &Rejection{Code: CodeUnsupported}
		}
		if err := aborter.AbortPreparation(r.Context(), input.Launch); err != nil {
			return output, err
		}
	case actionExited:
		observer, ok := p.(session.ExitObserver)
		if !ok {
			return output, &Rejection{Code: CodeUnsupported}
		}
		exited, err := observer.Exited(r.Context(), input.Launch, *input.Incarnation)
		if err != nil {
			return output, err
		}
		output.Exited = exited
	case actionStop:
		// The stop goes on to its outcome if home goes away or gives up, so
		// a quiesce in flight is never abandoned; its retry joins it, and an
		// abort of the same visit interrupts it.
		return h.stops.run(r.Context(), input, h.stopBound, func(ctx context.Context) (response, error) {
			output := response{Version: protocolVersion}
			s, err := p.Stop(ctx, input.Launch, *input.Incarnation)
			// A fenced writer with unsealed usage is a stop result, not a
			// failure: its nil receipt tells the client to keep the allocation
			// encumbered.
			if errors.Is(err, session.ErrUsageUnsealed) && s.ActualStopped {
				s.Receipt, s.Checkpoint, s.CheckpointDigest = nil, nil, ""
				err = nil
			}
			if err != nil {
				return output, err
			}
			output.Stopped = &s
			output.Checkpoint = s.Checkpoint
			return output, nil
		})
	case actionInspect, actionAbort:
		var o session.Observation
		var err error
		if action == actionInspect {
			o, err = p.Inspect(r.Context(), input.Launch)
		} else {
			o, err = h.abort(r.Context(), input)
		}
		if err != nil {
			return output, err
		}
		output.Observation = &observation{Launch: o.Launch, Incarnation: o.Incarnation, State: o.State, Stopped: o.Stopped}
		if o.Stopped != nil {
			output.Checkpoint = o.Stopped.Checkpoint
		}
	}
	return output, nil
}

// abort interrupts the visit's stops, and keeps new ones from waiting for a
// turn, while the placement aborts it.
func (h *handler) abort(ctx context.Context, input request) (session.Observation, error) {
	release := h.stops.abort(input)
	defer release()
	return h.config.Backend.Abort(ctx, input.Launch, *input.Incarnation)
}

// fail answers action's failure err with its outcome.
func (h *handler) fail(w http.ResponseWriter, action string, err error) {
	if errors.Is(err, harness.ErrNativeTurnPending) {
		// Still waiting is not unavailable: the client keeps the stop pending.
		var pending *harness.NativeTurnPending
		waiting := ""
		if errors.As(err, &pending) {
			waiting = pending.Waiting
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(response{Version: protocolVersion, Error: CodeTurnPending, Waiting: waitingText(waiting)})
		return
	}
	var refused *session.StartRefused
	if action == actionPrepare && errors.As(err, &refused) {
		// The preparation can never be made: the home ends the start. A
		// client accepts refused for a prepare only.
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(response{Version: protocolVersion, Error: CodeRefused,
			Reason: session.RefusalReason(refused.Reason)})
		return
	}
	var refusal *Rejection
	if errors.As(err, &refusal) {
		switch refusal.Code {
		case CodeInvalid:
			writeError(w, http.StatusBadRequest, refusal.Code)
		case CodeDenied:
			writeError(w, http.StatusForbidden, refusal.Code)
		case CodeConflict:
			writeError(w, http.StatusConflict, refusal.Code)
		case CodeExpired:
			writeError(w, http.StatusGone, refusal.Code)
		case CodeUnsupported:
			writeError(w, http.StatusUnprocessableEntity, refusal.Code)
		default:
			writeError(w, http.StatusServiceUnavailable, CodeUnavailable)
		}
	} else {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable)
	}
}
func writeError(w http.ResponseWriter, status int, code Code) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response{Version: protocolVersion, Error: code})
}

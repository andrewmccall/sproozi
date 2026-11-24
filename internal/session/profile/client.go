package profile

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/session"
)

// fixedOrigin accepts an HTTPS scheme, host and optional port, and nothing else.
func fixedOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" &&
		!u.ForceQuery && u.Fragment == "" && u.Opaque == "" && (u.Path == "" || u.Path == "/") && u.RawPath == ""
}

// Client implements Placement without holding Kubernetes credentials.
type Client struct {
	base  string
	token string
	// http is bounded by the connection's timeout, for short requests and an
	// attachment's admission. long has no bound of its own: each request it
	// carries waits as long as its action may take (wait).
	http          *http.Client
	long          *http.Client
	maxCheckpoint int64
	maxBody       int64
	profiles      func(digest string) ([]byte, error)
	// noExits records an executor that answered that it cannot report
	// exits, so this connection stops asking.
	noExits atomic.Bool
}

// SetProfileSource supplies retained profile archives for launches that name
// one. Without it such launches fail before any request.
func (c *Client) SetProfileSource(source func(digest string) ([]byte, error)) { c.profiles = source }

// Connection is a resolved route to one remote executor. Origin is the HTTPS
// origin its certificate covers; Roots, when set, replace the system roots.
// Dial, when set, replaces the network path to Origin with a byte stream the
// caller opens, such as a Kubernetes port-forward; TLS still runs over it.
// Timeout, 45 seconds when zero, bounds short requests; prepare, stop and
// the aborts wait as long as the executor may take over them, and never less.
type Connection struct {
	Origin             string
	Token              string
	Roots              *x509.CertPool
	Dial               func(ctx context.Context) (net.Conn, error)
	MaxCheckpointBytes int64
	Timeout            time.Duration
}

// Connect builds a client for a resolved connection without contacting it.
func Connect(config Connection) (*Client, error) {
	maxBody, err := bodyLimit(config.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	if !fixedOrigin(config.Origin) {
		return nil, errors.New("profile: fixed HTTPS origin required")
	}
	token, err := validToken(config.Token)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(config.Origin)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: config.Roots}
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	if config.Timeout < 0 || config.Timeout > time.Hour {
		return nil, errors.New("profile: invalid timeout")
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: config.Timeout, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 2}
	if dial := config.Dial; dial != nil {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }
	}
	// A long action answers only when it ends, so its headers wait as long.
	longTransport := transport.Clone()
	longTransport.ResponseHeaderTimeout = 0
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	client := &http.Client{Transport: transport, Timeout: config.Timeout, CheckRedirect: noRedirect}
	long := &http.Client{Transport: longTransport, CheckRedirect: noRedirect}
	return &Client{base: strings.TrimSuffix(u.String(), "/"), token: token, http: client, long: long, maxCheckpoint: config.MaxCheckpointBytes, maxBody: maxBody}, nil
}

// Close releases idle transport connections; it does not revoke an allocation.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
	c.long.CloseIdleConnections()
}

// wait is how long a placement action may take on the executor, plus a
// margin for its answer. It is zero for a short action, which the
// connection's timeout bounds.
func (c *Client) wait(action string) time.Duration {
	switch action {
	case actionPrepare:
		return prepareWait
	case actionStop:
		return stopWait
	case actionAbort, actionAbortPreparation:
		return abortWait
	}
	return 0
}

// do sends req, bounded by wait, but never less than the connection's
// timeout, when wait is set, and by the connection's timeout otherwise. The
// caller reads the body before calling the returned release.
func (c *Client) do(req *http.Request, wait time.Duration) (*http.Response, func(), error) {
	if wait == 0 {
		r, err := c.http.Do(req)
		return r, func() {}, err
	}
	ctx, cancel := context.WithTimeout(req.Context(), max(wait, c.http.Timeout))
	r, err := c.long.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return r, cancel, nil
}

func (c *Client) Prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	if err := validateCheckpoint(data, l, l.CheckpointDigest, c.maxCheckpoint); err != nil {
		return session.Incarnation{}, err
	}
	input := request{Launch: l, Checkpoint: data}
	if l.ProfileDigest != "" {
		if c.profiles == nil {
			return session.Incarnation{}, errors.New("profile: no retained profile archives")
		}
		archive, err := c.profiles(l.ProfileDigest)
		if err != nil {
			return session.Incarnation{}, err
		}
		if err = agentprofile.Verify(l.ProfileDigest, archive); err != nil {
			return session.Incarnation{}, err
		}
		input.Profile = archive
	}
	r, err := c.call(ctx, actionPrepare, input)
	if err != nil {
		return session.Incarnation{}, err
	}
	return *r.Incarnation, nil
}
func (c *Client) Activate(ctx context.Context, l session.Launch, i session.Incarnation) error {
	_, err := c.call(ctx, actionActivate, request{Launch: l, Incarnation: &i})
	return err
}
func (c *Client) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	r, err := c.call(ctx, actionStop, request{Launch: l, Incarnation: &i})
	if err != nil {
		return session.Stopped{}, err
	}
	r.Stopped.Checkpoint = r.Checkpoint
	if r.Stopped.Receipt == nil {
		if !r.Stopped.ActualStopped {
			return session.Stopped{}, errors.New("profile: remote stop remains unknown")
		}
		return *r.Stopped, session.ErrUsageUnsealed
	}
	return *r.Stopped, nil
}
func (c *Client) Inspect(ctx context.Context, l session.Launch) (session.Observation, error) {
	r, err := c.call(ctx, actionInspect, request{Launch: l})
	if err != nil {
		return session.Observation{}, err
	}
	return decodedObservation(r), nil
}
func (c *Client) Abort(ctx context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	r, err := c.call(ctx, actionAbort, request{Launch: l, Incarnation: &i})
	if err != nil {
		return session.Observation{}, err
	}
	return decodedObservation(r), nil
}

// AbortPreparation asks the executor to fence a launch whose preparation never
// reported an incarnation. An executor older than this action, or one whose
// placement cannot, gets what a coordinator did before the action existed:
// an absent launch is fenced, one with an incarnation is aborted, and one
// still preparing stays unproven. The error then says why.
func (c *Client) AbortPreparation(ctx context.Context, l session.Launch) error {
	_, err := c.call(ctx, actionAbortPreparation, request{Launch: l})
	if !preparationAbortUnsupported(err) {
		return err
	}
	return c.abortPreparationByInspection(ctx, l)
}

// Exited reports whether an active visit's harness exited on its own or at
// its deadline, as the executor's placement observes it. An executor older
// than the action, or one whose placement cannot observe exits, reports none,
// as home saw before the action existed: the connection then stops asking,
// and a new connection, after a failure or before its token expires, asks
// again, so an updated executor is noticed.
func (c *Client) Exited(ctx context.Context, l session.Launch, i session.Incarnation) (bool, error) {
	if c.noExits.Load() {
		return false, nil
	}
	r, err := c.call(ctx, actionExited, request{Launch: l, Incarnation: &i})
	if actionUnsupported(err) {
		c.noExits.Store(true)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.Exited, nil
}

// preparationAbortUnsupported reports an executor that does not know the
// action (404 invalid_request, as for any unknown action) or whose placement
// cannot abort a preparation (unsupported).
func preparationAbortUnsupported(err error) bool { return actionUnsupported(err) }

// actionUnsupported reports an executor that does not know an action (404
// invalid_request, as for any unknown action) or whose placement cannot
// perform it (unsupported).
func actionUnsupported(err error) bool {
	var answer *RemoteError
	if !errors.As(err, &answer) {
		return false
	}
	return answer.Code == CodeUnsupported || answer.Code == CodeInvalid && answer.Status == http.StatusNotFound
}

// errPreparationAbortUnsupported explains the fallback's unproven outcomes.
var errPreparationAbortUnsupported = errors.New("the remote executor cannot abort a preparation that has not reported its Pod; " +
	"it is older than this Sproozi. Update the executor, then run recover on the session")

func (c *Client) abortPreparationByInspection(ctx context.Context, l session.Launch) error {
	o, err := c.Inspect(ctx, l)
	if err != nil {
		return errors.Join(errPreparationAbortUnsupported, err)
	}
	if o.State == session.PlacementAbsent {
		return nil
	}
	if !incarnationValid(o.Incarnation) {
		return fmt.Errorf("%w; the cancelled preparation is still %s", errPreparationAbortUnsupported, o.State)
	}
	// Only a visit still preparing, or one already stopped (whose receipt is
	// checked below), is aborted here. An active visit, or one whose state is
	// unknown, as a stopping Kubernetes visit inspects, may have spent: it is
	// not a preparation, and success would report no usage.
	if o.State != session.PlacementPreparing && o.State != session.PlacementPrepared && o.State != session.PlacementStopped {
		return fmt.Errorf("%w; the visit is %s, not a preparation, so only a stop or an abort with its incarnation can end it",
			errPreparationAbortUnsupported, o.State)
	}
	a, err := c.Abort(ctx, l, o.Incarnation)
	if err != nil {
		return errors.Join(errPreparationAbortUnsupported, err)
	}
	if a.State != session.PlacementStopped || a.Incarnation != o.Incarnation {
		return fmt.Errorf("%w; aborting the prepared Pod did not prove it stopped", errPreparationAbortUnsupported)
	}
	// As for any abort, a stopped writer without a receipt has unknown usage,
	// which a successful preparation abort could not report.
	if a.Stopped == nil || a.Stopped.Receipt == nil {
		return fmt.Errorf("%w; the prepared Pod stopped, but its usage is not finalized yet", errPreparationAbortUnsupported)
	}
	// Success reports no usage, so only a receipt that sealed none is
	// dropped. A visit that inspects as prepared or stopped may still have
	// run; its usage is left for an abort that carries the receipt.
	for _, usage := range a.Stopped.Receipt.Usage {
		if usage != (budget.TrustedUsage{}) {
			return fmt.Errorf("%w; the visit stopped but recorded usage, so it was not a preparation", errPreparationAbortUnsupported)
		}
	}
	return nil
}

func decodedObservation(r response) session.Observation {
	o := r.Observation
	if o.Stopped != nil {
		o.Stopped.Checkpoint = r.Checkpoint
	}
	return session.Observation{Launch: o.Launch, Incarnation: o.Incarnation, State: o.State, Stopped: o.Stopped}
}

func (c *Client) call(ctx context.Context, action string, input request) (response, error) {
	if err := validateLaunch(input.Launch); err != nil {
		return response{}, err
	}
	if input.Incarnation != nil && !incarnationValid(*input.Incarnation) {
		return response{}, errors.New("profile: invalid incarnation")
	}
	input.Version = protocolVersion
	data, err := json.Marshal(input)
	if err != nil || int64(len(data)) > c.maxBody {
		return response{}, errors.New("profile: request exceeds bound")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/"+action, bytes.NewReader(data))
	if err != nil {
		return response{}, err
	}
	req.Header.Set(headerAuthorization, bearerPrefix+c.token)
	req.Header.Set(headerContentType, jsonMedia)
	r, release, err := c.do(req, c.wait(action))
	if err != nil {
		return response{}, err
	}
	defer release()
	defer func() { _ = r.Body.Close() }()
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get(headerContentType))
	if mediaErr != nil || media != jsonMedia {
		return response{}, errors.New("profile: invalid response content type")
	}
	data, err = io.ReadAll(io.LimitReader(r.Body, c.maxBody+1))
	if err != nil || int64(len(data)) > c.maxBody {
		return response{}, errors.New("profile: response unavailable or exceeds bound")
	}
	var output response
	if err = strictDecode(data, &output); err != nil || output.Version != protocolVersion {
		if answer := unrecognized(data, r.StatusCode); r.StatusCode != http.StatusOK && answer != nil {
			return response{}, answer
		}
		return response{}, errors.New("profile: invalid response")
	}
	if r.StatusCode != http.StatusOK {
		if answer := unrecognized(data, r.StatusCode); answer != nil {
			return response{}, answer
		}
		if !errorResponse(output, r.StatusCode) || output.Error == CodeTurnPending && action != actionStop ||
			output.Error == CodeRefused && action != actionPrepare {
			return response{}, errors.New("profile: invalid error response")
		}
		return response{}, &RemoteError{Code: output.Error, Status: r.StatusCode, Waiting: output.Waiting, Reason: output.Reason}
	}
	if err = validateResponse(action, input, output, c.maxCheckpoint); err != nil {
		return response{}, err
	}
	return output, nil
}

// Describe asks the executor for its placement name, harness and checkpoint
// limit. It needs only the origin and token, so a client built with any valid
// checkpoint limit can ask before the real limit is known.
func (c *Client) Describe(ctx context.Context) (Description, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/"+actionDescribe,
		strings.NewReader(fmt.Sprintf(`{"version":%d}`, protocolVersion)))
	if err != nil {
		return Description{}, err
	}
	req.Header.Set(headerAuthorization, bearerPrefix+c.token)
	req.Header.Set(headerContentType, jsonMedia)
	r, err := c.http.Do(req)
	if err != nil {
		return Description{}, &RemoteError{Code: CodeUnavailable, Transport: err}
	}
	defer func() { _ = r.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(r.Body, metadataLimit+1))
	var output response
	if err != nil || int64(len(data)) > metadataLimit || strictDecode(data, &output) != nil || output.Version != protocolVersion {
		return Description{}, &RemoteError{Code: CodeUnavailable, Status: r.StatusCode}
	}
	if r.StatusCode != http.StatusOK {
		if answer := unrecognized(data, r.StatusCode); answer != nil {
			return Description{}, answer
		}
		if !errorResponse(output, r.StatusCode) || output.Error == CodeTurnPending {
			return Description{}, errors.New("profile: invalid error response")
		}
		return Description{}, &RemoteError{Code: output.Error, Status: r.StatusCode}
	}
	d := output.Executor
	if output.Error != "" || d == nil || output.Incarnation != nil || output.Observation != nil || output.Stopped != nil || len(output.Checkpoint) > 0 || output.Exited ||
		!validName(d.Placement) || d.Harness.Validate() != nil {
		return Description{}, errors.New("profile: invalid description")
	}
	if _, err = bodyLimit(d.MaxCheckpointBytes); err != nil {
		return Description{}, errors.New("profile: invalid description")
	}
	return *d, nil
}

func errorResponse(r response, status int) bool {
	if r.Executor != nil || r.Incarnation != nil || r.Observation != nil || r.Stopped != nil || len(r.Checkpoint) > 0 || r.Exited {
		return false
	}
	if (r.Error == CodeTurnPending) != (r.Waiting != "") || r.Waiting != "" && waitingText(r.Waiting) != r.Waiting {
		return false
	}
	if r.Reason != "" && (r.Error != CodeRefused || session.RefusalReason(r.Reason) != r.Reason) {
		return false
	}
	switch r.Error {
	case CodeInvalid:
		return status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge || status == http.StatusUnsupportedMediaType
	case CodeDenied:
		return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusUpgradeRequired
	case CodeConflict, CodeRefused:
		return status == http.StatusConflict
	case CodeExpired:
		return status == http.StatusGone
	case CodeUnsupported:
		return status == http.StatusUnprocessableEntity
	case CodeUnavailable, CodeTurnPending:
		return status == http.StatusServiceUnavailable
	}
	return false
}

func validateCheckpoint(data []byte, l session.Launch, digest string, max int64) error {
	meta, err := checkpoint.Inspect(data, max)
	if err != nil || meta.Harness != l.Harness || meta.ThreadID != l.ThreadID || meta.SHA256 != digest {
		return errors.New("profile: checkpoint identity/digest mismatch")
	}
	return nil
}

var (
	_ session.Placement          = (*Client)(nil)
	_ session.PreparationAborter = (*Client)(nil)
	_ session.ExitObserver       = (*Client)(nil)
)

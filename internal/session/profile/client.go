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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/session"
)

// ClientConfig selects one administrator-configured HTTPS profile. CAFile may
// select a private CA; an empty CAFile uses system roots. Redirects, proxy
// environment variables and cookies are never used. Timeout bounds each request.
// The JSON names are the home configuration's keys; decoding ignores case, so
// older capitalised keys such as "URL" still load.
type ClientConfig struct {
	URL                string        `json:"url"`
	TokenFile          string        `json:"tokenFile"`
	CAFile             string        `json:"caFile,omitempty"`
	MaxCheckpointBytes int64         `json:"maxCheckpointBytes"`
	Timeout            time.Duration `json:"timeout,omitempty"`
}

// Validate checks the settings NewClient needs without reading any file. Each
// error names the JSON key it concerns.
func (c ClientConfig) Validate() error {
	var problems []error
	if !fixedOrigin(c.URL) {
		problems = append(problems, errors.New("url: fixed HTTPS origin such as https://profile.example:8443 required"))
	}
	if !filepath.IsAbs(c.TokenFile) {
		problems = append(problems, errors.New("tokenFile: absolute path to the bearer token file required"))
	}
	if c.CAFile != "" && !filepath.IsAbs(c.CAFile) {
		problems = append(problems, errors.New("caFile: absolute path required, or omit it to use system roots"))
	}
	if _, err := bodyLimit(c.MaxCheckpointBytes); err != nil {
		problems = append(problems, fmt.Errorf("maxCheckpointBytes: 1 byte to %d MiB required", maxArtifactLimit>>20))
	}
	if c.Timeout < 0 || c.Timeout > time.Hour {
		problems = append(problems, errors.New("timeout: at most one hour required"))
	}
	return errors.Join(problems...)
}

// fixedOrigin accepts an HTTPS scheme, host and optional port, and nothing else.
func fixedOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" &&
		!u.ForceQuery && u.Fragment == "" && u.Opaque == "" && (u.Path == "" || u.Path == "/") && u.RawPath == ""
}

// Client implements Placement without holding Kubernetes credentials.
type Client struct {
	base          string
	token         string
	http          *http.Client
	maxCheckpoint int64
	maxBody       int64
	profiles      func(digest string) ([]byte, error)
}

// SetProfileSource supplies retained profile archives for launches that name
// one. Without it such launches fail before any request.
func (c *Client) SetProfileSource(source func(digest string) ([]byte, error)) { c.profiles = source }

// NewClient validates the fixed origin and constructs a CA-verified TLS 1.3
// transport. Credentials are read once and sent only to that origin.
func NewClient(config ClientConfig) (*Client, error) {
	if !fixedOrigin(config.URL) {
		return nil, errors.New("profile: fixed HTTPS origin required")
	}
	token, err := readToken(config.TokenFile)
	if err != nil {
		return nil, err
	}
	connection := Connection{Origin: config.URL, Token: token, MaxCheckpointBytes: config.MaxCheckpointBytes, Timeout: config.Timeout}
	if config.CAFile != "" {
		if connection.Roots, err = readRoots(config.CAFile); err != nil {
			return nil, err
		}
	}
	return Connect(connection)
}

// Connection is a resolved route to one remote executor. Origin is the HTTPS
// origin its certificate covers; Roots, when set, replace the system roots.
// Dial, when set, replaces the network path to Origin with a byte stream the
// caller opens, such as a Kubernetes port-forward; TLS still runs over it.
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
		config.Timeout = 45 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > time.Hour {
		return nil, errors.New("profile: invalid timeout")
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: config.Timeout, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 2}
	if dial := config.Dial; dial != nil {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }
	}
	client := &http.Client{Transport: transport, Timeout: config.Timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{base: strings.TrimSuffix(u.String(), "/"), token: token, http: client, maxCheckpoint: config.MaxCheckpointBytes, maxBody: maxBody}, nil
}

func readRoots(name string) (*x509.CertPool, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, errors.New("profile: CA file unavailable")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, metadataLimit+1))
	roots := x509.NewCertPool()
	if err != nil || int64(len(data)) > metadataLimit || !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("profile: invalid CA file")
	}
	return roots, nil
}

// Close releases idle transport connections; it does not revoke an allocation.
func (c *Client) Close() { c.http.CloseIdleConnections() }

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
	r, err := c.http.Do(req)
	if err != nil {
		return response{}, err
	}
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
		return response{}, errors.New("profile: invalid response")
	}
	if r.StatusCode != http.StatusOK {
		if !errorResponse(output, r.StatusCode) {
			return response{}, errors.New("profile: invalid error response")
		}
		return response{}, &RemoteError{Code: output.Error, Status: r.StatusCode}
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
		if !errorResponse(output, r.StatusCode) {
			return Description{}, errors.New("profile: invalid error response")
		}
		return Description{}, &RemoteError{Code: output.Error, Status: r.StatusCode}
	}
	d := output.Executor
	if output.Error != "" || d == nil || output.Incarnation != nil || output.Observation != nil || output.Stopped != nil || len(output.Checkpoint) > 0 ||
		!validName(d.Placement) || d.Harness.Validate() != nil {
		return Description{}, errors.New("profile: invalid description")
	}
	if _, err = bodyLimit(d.MaxCheckpointBytes); err != nil {
		return Description{}, errors.New("profile: invalid description")
	}
	return *d, nil
}

func errorResponse(r response, status int) bool {
	if r.Executor != nil || r.Incarnation != nil || r.Observation != nil || r.Stopped != nil || len(r.Checkpoint) > 0 {
		return false
	}
	switch r.Error {
	case CodeInvalid:
		return status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusRequestEntityTooLarge || status == http.StatusUnsupportedMediaType
	case CodeDenied:
		return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusUpgradeRequired
	case CodeConflict:
		return status == http.StatusConflict
	case CodeExpired:
		return status == http.StatusGone
	case CodeUnsupported:
		return status == http.StatusUnprocessableEntity
	case CodeUnavailable:
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

var _ session.Placement = (*Client)(nil)

// Package modelauth owns provider credentials on the trusted gateway. It is
// deliberately separate from projected-token authentication of AgentRuns.
package modelauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	APIKeyMode               = "api_key"
	chatGPTAccountHeader     = "ChatGPT-Account-ID"
	openAIOrganizationHeader = "OpenAI-Organization"
	openAIProjectHeader      = "OpenAI-Project"
	ChatGPTMode              = "chatgpt"
	Issuer                   = "https://auth.openai.com"
	Resource                 = "https://api.openai.com/v1"
	TokenEndpoint            = Issuer + "/api/accounts/oauth/token"
	PlanScope                = "chatgpt.tokens.use.direct"
	clientIDField            = "client_id"
	refreshTokenField        = "refresh_token"
	nonceField               = "nonce"
	dynamicClientID          = "dynamic_agent_client"
	resourceField            = "resource"
	stateField               = "state"
	codeField                = "code"
	errorField               = "error"
)

// Authenticator authenticates an already-authorized request to the provider.
// Its mode is administrator-owned, never selected by a workload header.
type Authenticator interface {
	Mode() string
	Authorize(*http.Request) error
}

// WorkloadMode forwards the credential the harness signed in with (ADR 0047).
const WorkloadMode = "workload"

// Workload keeps the harness's own provider credential and account headers:
// the harness, not the gateway, chose the account it signed in to.
type Workload struct{}

func (Workload) Mode() string { return WorkloadMode }
func (Workload) Authorize(r *http.Request) error {
	if !HasWorkloadCredential(r.Header) {
		return errors.New("the harness sent no provider credential")
	}
	return nil
}

// HasWorkloadCredential reports whether the headers carry one well-formed
// provider credential: an X-Api-Key or a bearer Authorization.
func HasWorkloadCredential(header http.Header) bool {
	for _, value := range []string{header.Get("X-Api-Key"), strings.TrimPrefix(header.Get("Authorization"), "Bearer ")} {
		if strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\r\n") {
			return true
		}
	}
	return false
}

// TokenMode installs a gateway-held sign-in token (ADR 0047).
const TokenMode = "token"

// Token installs a gateway-held sign-in token, such as one from claude
// setup-token, as a bearer credential, with any headers its provider requires
// for one. Workload credentials and account headers never select the account.
type Token struct {
	Value   string
	Headers map[string]string
}

func (Token) Mode() string { return TokenMode }
func (t Token) Authorize(r *http.Request) error {
	if strings.TrimSpace(t.Value) == "" || strings.ContainsAny(t.Value, "\r\n") {
		return errors.New("provider sign-in token is unavailable")
	}
	removeOpenAIAccountHeaders(r.Header)
	r.Header.Del("X-Api-Key")
	r.Header.Set("Authorization", "Bearer "+t.Value)
	for name, value := range t.Headers {
		// Keep the client's other comma-separated values, such as betas.
		values := strings.Split(r.Header.Get(name), ",")
		if !slices.ContainsFunc(values, func(v string) bool { return strings.TrimSpace(v) == value }) {
			values = append(slices.DeleteFunc(values, func(v string) bool { return strings.TrimSpace(v) == "" }), value)
		}
		r.Header.Set(name, strings.Join(values, ","))
	}
	return nil
}

type APIKey struct{ Key string }

// AnthropicAPIKey installs a trusted Messages API credential. A workload's
// placeholder and account headers never select the provider account.
type AnthropicAPIKey struct{ Key string }

func (AnthropicAPIKey) Mode() string { return APIKeyMode }
func (a AnthropicAPIKey) Authorize(r *http.Request) error {
	if strings.TrimSpace(a.Key) == "" || strings.ContainsAny(a.Key, "\r\n") {
		return errors.New("provider API key is unavailable")
	}
	r.Header.Del("Authorization")
	r.Header.Set("X-Api-Key", a.Key)
	return nil
}

func (APIKey) Mode() string { return APIKeyMode }
func (a APIKey) Authorize(r *http.Request) error {
	if strings.TrimSpace(a.Key) == "" || strings.ContainsAny(a.Key, "\r\n") {
		return errors.New("provider API key is unavailable")
	}
	removeOpenAIAccountHeaders(r.Header)
	r.Header.Set("Authorization", "Bearer "+a.Key)
	return nil
}

func removeOpenAIAccountHeaders(headers http.Header) {
	for _, header := range []string{openAIOrganizationHeader, openAIProjectHeader, chatGPTAccountHeader} {
		headers.Del(header)
	}
}

// Session is a protected SIWC credential record, not a Codex auth.json file.
// Identity is validated by the login helper before this record is saved.
type Session struct {
	Email        string    `json:"email,omitempty"`
	Issuer       string    `json:"issuer"`
	Subject      string    `json:"subject"`
	ClientID     string    `json:"client_id"`
	HostID       string    `json:"ext_agent_host_id"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int64     `json:"expires_in"`
	Scopes       []string  `json:"scopes"`
	SavedAt      time.Time `json:"saved_at"`
}

func (s Session) Validate() error {
	if s.Issuer != Issuer || s.Subject == "" || !strings.HasPrefix(s.ClientID, "oaiapp_") || s.HostID == "" {
		return errors.New("ChatGPT registration is incomplete; sign in again")
	}
	if s.AccessToken == "" || s.RefreshToken == "" || s.IDToken == "" ||
		strings.ContainsAny(s.AccessToken+s.RefreshToken, "\r\n") ||
		!strings.EqualFold(s.TokenType, "Bearer") || s.ExpiresIn <= 0 || s.ExpiresIn > 86400 || s.SavedAt.IsZero() {
		return errors.New("ChatGPT credential is incomplete; sign in again")
	}
	if !slices.Contains(s.Scopes, PlanScope) {
		return errors.New("ChatGPT plan permission was not granted; sign in again")
	}
	return nil
}

// SessionStore atomically replaces a credential set only if the current tokens
// still match before. Refresh tokens rotate and must not be overwritten by a
// stale writer. One active gateway process owns each renewable session.
type SessionStore interface {
	Load(context.Context) (Session, error)
	Replace(context.Context, Session, Session) error
}

// SessionReloadInterval bounds how long Authorize serves an in-memory access
// token before reading the Secret again, measured from the start of the last
// read. It is how quickly an administrator's replacement or removal of the
// stored session reaches model requests.
const SessionReloadInterval = 10 * time.Second

// refreshWindow is how close to expiry the access token is refreshed.
const refreshWindow = time.Minute

// refreshTimeout bounds each step of a reload or refresh once started: the
// store read, the save of an unsaved refresh, the refresh and its save. It
// matches the refresh client's timeout (NewRefreshClient).
const refreshTimeout = 30 * time.Second

// step returns the context for one step of a reload or refresh. It derives
// from no model request: the provider spends the refresh token when it
// answers, so neither the refresh nor the save of what it returned may end
// because the request that started it ended.
func step() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), refreshTimeout)
}

type ChatGPT struct {
	store  SessionStore
	client *http.Client
	// current is the last session read from or written to the store, with the
	// time it was read and the refresh record as the read left it. Requests
	// use it without the lock while it is younger than reloadInterval, and
	// its access token is outside the refresh window or a remembered failure
	// answers for it.
	current        atomic.Pointer[loadedSession]
	reloadInterval time.Duration
	// mu guards inflight, the one reload or refresh running for this
	// credential: this process is the session's one refresh owner (ADR
	// 0026). It is never held across a store call or the refresh.
	mu       sync.Mutex
	inflight *attempt
	// The fields below belong to the running attempt; attempts run one at a
	// time, each started under mu after the last has ended.
	//
	// Keep an unsaved replacement so persistence retries never rotate the same
	// refresh token twice. Do not expose it to inference until durably saved.
	pending       *Session
	pendingBefore Session
	// refresh remembers a failed refresh of the stored session, so a provider
	// outage or a refused sign-in does not send a refresh with every request.
	refresh       refreshRecord
	tokenEndpoint string
	// now is the clock every expiry, reload and hold is measured against.
	now func() time.Time
}

// attempt is one reload, and refresh when due, run by the authenticator in
// its own goroutine. Every caller waits for done or its own context.
type attempt struct {
	done    chan struct{}
	session Session
	err     error
}

type loadedSession struct {
	session  Session
	loadedAt time.Time
	refresh  refreshRecord
}

// NewChatGPT returns the authenticator for a stored sign-in. It never follows
// a redirect from the token endpoint, whatever client it is given.
func NewChatGPT(store SessionStore, client *http.Client) *ChatGPT {
	refreshing := *client
	refreshing.CheckRedirect = refuseRedirect
	return &ChatGPT{store: store, client: &refreshing, tokenEndpoint: TokenEndpoint, reloadInterval: SessionReloadInterval,
		now: time.Now}
}

func (*ChatGPT) Mode() string { return ChatGPTMode }

func (a *ChatGPT) Authorize(r *http.Request) error {
	// The administrator's OAuth session selects the account, not a workload.
	r.Header.Del("Authorization")
	removeOpenAIAccountHeaders(r.Header)
	s, ok, err := a.usable()
	if !ok {
		s, err = a.reloadOrRefresh(r.Context())
	}
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+s.AccessToken)
	return nil
}

// usable answers from memory when no Secret read or refresh is due: with the
// session, or with a remembered failure while it holds the refresh back.
func (a *ChatGPT) usable() (Session, bool, error) {
	current := a.current.Load()
	now := a.now()
	if current == nil || now.Sub(current.loadedAt) >= a.reloadInterval {
		return Session{}, false, nil
	}
	return current.answer(now)
}

// answer is what a request sees for a session read at loadedAt: the session
// while its access token is outside the refresh window, the remembered
// failure while it holds a due refresh back, and nothing (a refresh is due
// and may be sent) otherwise.
func (l *loadedSession) answer(now time.Time) (Session, bool, error) {
	if l.session.expiry().Sub(now) > refreshWindow {
		return l.session, true, nil
	}
	if held := l.refresh.admit(now); held != nil {
		return Session{}, true, held
	}
	return Session{}, false, nil
}

// reloadOrRefresh joins the reload or refresh in flight, or starts one, and
// waits for its result or for ctx, whichever comes first. A caller that
// leaves does not stop it: it finishes on its own bounds and saves what the
// provider issued.
func (a *ChatGPT) reloadOrRefresh(ctx context.Context) (Session, error) {
	a.mu.Lock()
	// Another request may have reloaded or refreshed while this one waited.
	if s, ok, err := a.usable(); ok {
		a.mu.Unlock()
		return s, err
	}
	at := a.inflight
	if at == nil {
		at = &attempt{done: make(chan struct{})}
		a.inflight = at
		go a.run(at)
	}
	a.mu.Unlock()
	select {
	case <-at.done:
		return at.session, at.err
	case <-ctx.Done():
		return Session{}, ctx.Err()
	}
}

// run carries out one attempt and publishes its result.
func (a *ChatGPT) run(at *attempt) {
	s, err := a.reload()
	a.mu.Lock()
	at.session, at.err = s, err
	a.inflight = nil
	a.mu.Unlock()
	close(at.done)
}

// reload reads the stored session and refreshes it when due. It always
// refreshes from the stored tokens, never the in-memory copy, so the store's
// compare-before-write check sees what it read.
func (a *ChatGPT) reload() (Session, error) {
	a.current.Store(nil)
	// The reload interval runs from the start of the read, so a slow Secret
	// read does not extend how long an older stored session is served.
	loadedAt := a.now()
	ctx, cancel := step()
	s, err := a.store.Load(ctx)
	cancel()
	if err != nil {
		return Session{}, errors.New("ChatGPT credentials are unavailable")
	}
	if err = s.Validate(); err != nil {
		return Session{}, err
	}
	if a.pending != nil {
		if s.SameCredentials(a.pendingBefore) {
			ctx, cancel := step()
			err = a.store.Replace(ctx, s, *a.pending)
			cancel()
			if err != nil {
				return Session{}, errors.New("could not persist renewed ChatGPT credentials")
			}
			s = *a.pending
		}
		a.pending = nil
	}
	now := a.now()
	if a.refresh.current() != refreshReady && !s.SameCredentials(a.refresh.of) {
		if err = a.refresh.apply(eventSignedIn, s, nil, now); err != nil {
			return Session{}, err
		}
	}
	if s.expiry().Sub(now) <= refreshWindow {
		if held := a.refresh.admit(now); held != nil {
			a.publish(s, loadedAt)
			return Session{}, held
		}
		ctx, cancel := step()
		next, refreshErr := Exchange(ctx, a.client, a.tokenEndpoint, url.Values{
			"grant_type": {refreshTokenField}, clientIDField: {s.ClientID},
			refreshTokenField: {s.RefreshToken}, resourceField: {Resource},
		})
		cancel()
		if refreshErr != nil {
			return Session{}, a.failed(s, loadedAt, classify(refreshErr))
		}
		renewed := s
		renewed.AccessToken, renewed.RefreshToken = next.AccessToken, next.RefreshToken
		renewed.TokenType, renewed.ExpiresIn, renewed.SavedAt = next.TokenType, next.ExpiresIn, a.now().UTC()
		if next.IDToken != "" {
			renewed.IDToken = next.IDToken
		}
		if next.Scope != "" {
			renewed.Scopes = strings.Fields(next.Scope)
		}
		if err = renewed.Validate(); err != nil {
			return Session{}, a.failed(s, loadedAt, classify(err))
		}
		if err = a.refresh.apply(eventRefreshed, s, nil, a.now()); err != nil {
			return Session{}, err
		}
		a.pending, a.pendingBefore = &renewed, s
		ctx, cancel = step()
		err = a.store.Replace(ctx, s, renewed)
		cancel()
		if err != nil {
			return Session{}, errors.New("could not persist renewed ChatGPT credentials")
		}
		a.pending = nil
		s = renewed
	}
	a.publish(s, loadedAt)
	return s, nil
}

// publish makes s, read at loadedAt, and the refresh record what requests
// answer from until the next read.
func (a *ChatGPT) publish(s Session, loadedAt time.Time) {
	a.current.Store(&loadedSession{session: s, loadedAt: loadedAt, refresh: a.refresh})
}

// failed remembers a refresh of s, read at loadedAt, that failed and returns
// what the requests waiting for it see.
func (a *ChatGPT) failed(s Session, loadedAt time.Time, failure *RefreshError) error {
	event := eventFailed
	if failure.Refused {
		event = eventRefused
	}
	if err := a.refresh.apply(event, s, failure, a.now()); err != nil {
		return err
	}
	a.publish(s, loadedAt)
	return failure
}

// expiry is when the session's access token expires.
func (s Session) expiry() time.Time {
	return s.SavedAt.Add(time.Duration(s.ExpiresIn) * time.Second)
}

// SameCredentials compares the rotating credential identity used by store CAS.
// Expiry/metadata changes do not turn an unchanged token pair into a new writer.
func (s Session) SameCredentials(other Session) bool {
	return s.ClientID == other.ClientID && s.Subject == other.Subject &&
		s.AccessToken == other.AccessToken && s.RefreshToken == other.RefreshToken
}

// TokenResponse is decoded only from the fixed trusted OAuth endpoint. Never
// include the provider body, tokens, or redirect URLs in errors or logs.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func Exchange(ctx context.Context, client *http.Client, endpoint string, form url.Values) (TokenResponse, error) {
	var result TokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return result, errors.New("could not construct ChatGPT token exchange")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return result, &tokenEndpointError{message: "ChatGPT token exchange was unavailable"}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return result, &tokenEndpointError{status: resp.StatusCode, grantError: grantError(resp),
			message: fmt.Sprintf("ChatGPT token exchange failed (HTTP %d); sign in again if access was revoked", resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &result) != nil {
		return TokenResponse{}, &tokenEndpointError{status: http.StatusOK,
			message: "ChatGPT token exchange returned invalid credentials"}
	}
	return result, nil
}

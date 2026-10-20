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

type ChatGPT struct {
	store  SessionStore
	client *http.Client
	// current is the last session read from or written to the store, with the
	// time it was read. Requests use it without the lock while it is younger
	// than reloadInterval and its access token is outside the refresh window.
	current        atomic.Pointer[loadedSession]
	reloadInterval time.Duration
	// mu serializes reloads and refreshes: this process is the session's one
	// refresh owner (ADR 0026).
	mu sync.Mutex
	// Keep an unsaved replacement so persistence retries never rotate the same
	// refresh token twice. Do not expose it to inference until durably saved.
	pending       *Session
	pendingBefore Session
	tokenEndpoint string
}

type loadedSession struct {
	session  Session
	loadedAt time.Time
}

func NewChatGPT(store SessionStore, client *http.Client) *ChatGPT {
	return &ChatGPT{store: store, client: client, tokenEndpoint: TokenEndpoint, reloadInterval: SessionReloadInterval}
}

func (*ChatGPT) Mode() string { return ChatGPTMode }

func (a *ChatGPT) Authorize(r *http.Request) error {
	// The administrator's OAuth session selects the account, not a workload.
	r.Header.Del("Authorization")
	removeOpenAIAccountHeaders(r.Header)
	s, ok := a.usable()
	if !ok {
		var err error
		if s, err = a.reloadOrRefresh(r.Context()); err != nil {
			return err
		}
	}
	r.Header.Set("Authorization", "Bearer "+s.AccessToken)
	return nil
}

// usable returns the in-memory session when no Secret read or refresh is due.
func (a *ChatGPT) usable() (Session, bool) {
	current := a.current.Load()
	if current == nil || time.Since(current.loadedAt) >= a.reloadInterval ||
		time.Until(current.session.SavedAt.Add(time.Duration(current.session.ExpiresIn)*time.Second)) <= refreshWindow {
		return Session{}, false
	}
	return current.session, true
}

// reloadOrRefresh reads the stored session and refreshes it when due. It holds
// the lock throughout, and always refreshes from the stored tokens, never the
// in-memory copy, so the store's compare-before-write check sees what it read.
func (a *ChatGPT) reloadOrRefresh(ctx context.Context) (Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Another request may have reloaded or refreshed while this one waited.
	if s, ok := a.usable(); ok {
		return s, nil
	}
	a.current.Store(nil)
	// The reload interval runs from the start of the read, so a slow Secret
	// read does not extend how long an older stored session is served.
	loadedAt := time.Now()
	s, err := a.store.Load(ctx)
	if err != nil {
		return Session{}, errors.New("ChatGPT credentials are unavailable")
	}
	if err = s.Validate(); err != nil {
		return Session{}, err
	}
	if a.pending != nil {
		if s.SameCredentials(a.pendingBefore) {
			if err = a.store.Replace(ctx, s, *a.pending); err != nil {
				return Session{}, errors.New("could not persist renewed ChatGPT credentials")
			}
			s = *a.pending
		}
		a.pending = nil
	}
	if time.Until(s.SavedAt.Add(time.Duration(s.ExpiresIn)*time.Second)) <= refreshWindow {
		next, refreshErr := Exchange(ctx, a.client, a.tokenEndpoint, url.Values{
			"grant_type": {refreshTokenField}, clientIDField: {s.ClientID},
			refreshTokenField: {s.RefreshToken}, resourceField: {Resource},
		})
		if refreshErr != nil {
			return Session{}, refreshErr
		}
		renewed := s
		renewed.AccessToken, renewed.RefreshToken = next.AccessToken, next.RefreshToken
		renewed.TokenType, renewed.ExpiresIn, renewed.SavedAt = next.TokenType, next.ExpiresIn, time.Now().UTC()
		if next.IDToken != "" {
			renewed.IDToken = next.IDToken
		}
		if next.Scope != "" {
			renewed.Scopes = strings.Fields(next.Scope)
		}
		if err = renewed.Validate(); err != nil {
			return Session{}, err
		}
		a.pending, a.pendingBefore = &renewed, s
		if err = a.store.Replace(ctx, s, renewed); err != nil {
			return Session{}, errors.New("could not persist renewed ChatGPT credentials")
		}
		a.pending = nil
		s = renewed
	}
	a.current.Store(&loadedSession{session: s, loadedAt: loadedAt})
	return s, nil
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
		return result, errors.New("ChatGPT token exchange was unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("ChatGPT token exchange failed (HTTP %d); sign in again if access was revoked", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &result) != nil {
		return TokenResponse{}, errors.New("ChatGPT token exchange returned invalid credentials")
	}
	return result, nil
}

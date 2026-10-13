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
	"time"
)

const (
	APIKeyMode        = "api_key"
	ChatGPTMode       = "chatgpt"
	Issuer            = "https://auth.openai.com"
	Resource          = "https://api.openai.com/v1"
	TokenEndpoint     = Issuer + "/api/accounts/oauth/token"
	PlanScope         = "chatgpt.tokens.use.direct"
	clientIDField     = "client_id"
	refreshTokenField = "refresh_token"
	nonceField        = "nonce"
	dynamicClientID   = "dynamic_agent_client"
	resourceField     = "resource"
	stateField        = "state"
	codeField         = "code"
	errorField        = "error"
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
	r.Header.Set("Authorization", "Bearer "+a.Key)
	return nil
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
		return errors.New("ChatGPT credentials are incomplete; sign in again")
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

type ChatGPT struct {
	store  SessionStore
	client *http.Client
	mu     sync.Mutex
	// Keep an unsaved replacement so persistence retries never rotate the same
	// refresh token twice. Do not expose it to inference until durably saved.
	pending       *Session
	pendingBefore Session
	tokenEndpoint string
}

func NewChatGPT(store SessionStore, client *http.Client) *ChatGPT {
	return &ChatGPT{store: store, client: client, tokenEndpoint: TokenEndpoint}
}

func (*ChatGPT) Mode() string { return ChatGPTMode }

func (a *ChatGPT) Authorize(r *http.Request) error {
	// The administrator's OAuth session selects the account, not a workload.
	for _, header := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project", "ChatGPT-Account-ID"} {
		r.Header.Del(header)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.store.Load(r.Context())
	if err != nil {
		return errors.New("ChatGPT credentials are unavailable")
	}
	if err = s.Validate(); err != nil {
		return err
	}
	if a.pending != nil {
		if sameCredentials(s, a.pendingBefore) {
			if err = a.store.Replace(r.Context(), s, *a.pending); err != nil {
				return errors.New("could not persist renewed ChatGPT credentials")
			}
			s = *a.pending
		}
		a.pending = nil
	}
	if time.Until(s.SavedAt.Add(time.Duration(s.ExpiresIn)*time.Second)) <= time.Minute {
		next, refreshErr := Exchange(r.Context(), a.client, a.tokenEndpoint, url.Values{
			"grant_type": {refreshTokenField}, clientIDField: {s.ClientID},
			refreshTokenField: {s.RefreshToken}, resourceField: {Resource},
		})
		if refreshErr != nil {
			return refreshErr
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
			return err
		}
		a.pending, a.pendingBefore = &renewed, s
		if err = a.store.Replace(r.Context(), s, renewed); err != nil {
			return errors.New("could not persist renewed ChatGPT credentials")
		}
		a.pending = nil
		s = renewed
	}
	r.Header.Set("Authorization", "Bearer "+s.AccessToken)
	return nil
}

func sameCredentials(a, b Session) bool {
	return a.ClientID == b.ClientID && a.Subject == b.Subject &&
		a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
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

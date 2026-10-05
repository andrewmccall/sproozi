package modelauth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

// LoadFile reads only owner-private, regular credential files. Never accept a
// symlink or use the user's Codex login cache as an SIWC credential record.
func LoadFile(path string) (Session, error) {
	var s Session
	info, err := os.Lstat(path)
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return s, errors.New("ChatGPT credential file must be a regular owner-private file (0600)")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 1<<20 {
		return s, errors.New("could not read ChatGPT credential file")
	}
	if json.Unmarshal(data, &s) != nil {
		return s, errors.New("invalid ChatGPT credential file")
	}
	return s, s.Validate()
}

// WritePrivateJSON replaces the entire record atomically, with no token output.
func WritePrivateJSON(path string, value any) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("refusing to replace a non-regular credential file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("could not encode private credentials")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".chatgpt-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func hostID(path string) (string, error) {
	var record struct {
		ID string `json:"ext_agent_host_id"`
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &record) != nil || !strings.HasPrefix(record.ID, "urn:uuid:") {
			return "", errors.New("invalid saved ChatGPT host registration")
		}
		return record.ID, nil
	}
	if !os.IsNotExist(err) {
		return "", errors.New("could not read ChatGPT host registration")
	}
	record.ID = "urn:uuid:" + uuid.NewString()
	return record.ID, WritePrivateJSON(path, record)
}

// callbackClient validates one pending attempt before exposing its code. Never
// accept a different issued client ID during a returning-account sign-in.
func callbackClient(q url.Values, state, existingClient string) (string, error) {
	for _, key := range []string{stateField, codeField, clientIDField, errorField} {
		if len(q[key]) > 1 {
			return "", errors.New("ambiguous OAuth callback")
		}
	}
	if subtle.ConstantTimeCompare([]byte(q.Get(stateField)), []byte(state)) != 1 {
		return "", errors.New("OAuth state mismatch")
	}
	if q.Get(errorField) != "" {
		return "", errors.New("ChatGPT authorization was not granted")
	}
	clientID := q.Get(clientIDField)
	if existingClient != "" {
		if clientID != "" && clientID != existingClient {
			return "", errors.New("ChatGPT registration changed during authorization")
		}
		clientID = existingClient
	}
	if !strings.HasPrefix(clientID, "oaiapp_") || q.Get(codeField) == "" {
		return "", errors.New("ChatGPT registration did not return an issued client ID and code")
	}
	return clientID, nil
}

func verifyIdentity(ctx context.Context, verifier *oidc.IDTokenVerifier, raw, nonce string, previous Session) (*oidc.IDToken, error) {
	identity, err := verifier.Verify(ctx, raw)
	if err != nil || identity == nil || identity.Subject == "" {
		return nil, errors.New("ChatGPT ID-token verification failed")
	}
	if subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(nonce)) != 1 {
		return nil, errors.New("ChatGPT ID-token nonce mismatch")
	}
	if previous.Subject != "" && (identity.Subject != previous.Subject || identity.Issuer != previous.Issuer) {
		return nil, errors.New("ChatGPT account changed; use a separate credential file for a different account")
	}
	return identity, nil
}

// Login performs public-client registration with PKCE and validated OIDC
// identity. openBrowser is supplied by the CLI; only the human grants consent.
// It does not log the authorization URL (a returning flow contains an ID token).
func Login(ctx context.Context, path, hostPath string, openBrowser func(string) error) (Session, error) {
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return login(ctx, path, hostPath, openBrowser, client)
}

func login(ctx context.Context, path, hostPath string, openBrowser func(string) error, client *http.Client) (Session, error) {
	previous, err := existingSession(path)
	if err != nil {
		return Session{}, err
	}
	host, err := hostID(hostPath)
	if err != nil {
		return Session{}, err
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, Issuer)
	if err != nil {
		return Session{}, errors.New("OpenAI identity discovery is unavailable")
	}
	attempt, err := browserAuthorization(ctx, previous, host, openBrowser)
	if err != nil {
		return Session{}, err
	}
	tokens, err := Exchange(ctx, client, TokenEndpoint, url.Values{
		"grant_type": {"authorization_code"}, clientIDField: {attempt.clientID}, codeField: {attempt.code},
		"code_verifier": {attempt.verifier}, "redirect_uri": {attempt.redirect}, resourceField: {Resource},
	})
	if err != nil {
		return Session{}, err
	}
	identity, err := verifyIdentity(ctx, provider.Verifier(&oidc.Config{ClientID: attempt.clientID}), tokens.IDToken, attempt.nonce, previous)
	if err != nil {
		return Session{}, err
	}
	var claims struct {
		Email string `json:"email"`
	}
	if identity.Claims(&claims) != nil {
		return Session{}, errors.New("ChatGPT identity claims are invalid")
	}
	session := Session{Email: claims.Email, Issuer: identity.Issuer, Subject: identity.Subject, ClientID: attempt.clientID, HostID: host,
		IDToken: tokens.IDToken, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		TokenType: tokens.TokenType, ExpiresIn: tokens.ExpiresIn, Scopes: strings.Fields(tokens.Scope), SavedAt: time.Now().UTC()}
	if err = session.Validate(); err != nil {
		return Session{}, err
	}
	return session, WritePrivateJSON(path, session)
}

func existingSession(path string) (Session, error) {
	if _, err := os.Lstat(path); err == nil {
		return LoadFile(path)
	} else if !os.IsNotExist(err) {
		return Session{}, err
	}
	return Session{}, nil
}

type authorizationAttempt struct{ clientID, code, verifier, redirect, nonce string }

func browserAuthorization(ctx context.Context, previous Session, host string, openBrowser func(string) error) (authorizationAttempt, error) {
	var attempt authorizationAttempt
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return attempt, errors.New("could not start loopback OAuth callback")
	}
	defer func() { _ = listener.Close() }()
	redirect := "http://" + listener.Addr().String() + "/auth/callback"
	state, nonce, verifier := rand.Text(), rand.Text(), oauth2.GenerateVerifier()
	query := url.Values{
		clientIDField: {dynamicClientID}, "agent_name_hint": {"Sproozi"}, "ext_agent_host_id": {host},
		"response_type": {codeField}, "redirect_uri": {redirect}, "scope": {"openid profile email offline_access resource.invoke " + PlanScope},
		resourceField: {Resource}, stateField: {state}, nonceField: {nonce},
		"code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)},
	}
	if previous.ClientID != "" {
		query.Set(clientIDField, previous.ClientID)
		query.Del("agent_name_hint")
		query.Set("id_token_hint", previous.IDToken)
	}
	callbacks := make(chan url.Values, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		q := r.URL.Query()
		if r.Method != http.MethodGet || q.Get(stateField) != state {
			http.Error(w, "Invalid authorization callback", http.StatusBadRequest)
			return
		}
		select {
		case callbacks <- q:
			_, _ = fmt.Fprint(w, "Authorization received. Return to your terminal for verification.")
		default:
			http.Error(w, "Authorization already received", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() { _ = server.Serve(listener) }()
	if err = openBrowser(Issuer + "/api/accounts/authorize?" + query.Encode()); err != nil {
		return attempt, errors.New("could not open the browser; run this login helper on your local desktop")
	}
	var callback url.Values
	select {
	case callback = <-callbacks:
	case <-ctx.Done():
		return attempt, errors.New("ChatGPT login timed out or was cancelled")
	}
	clientID, err := callbackClient(callback, state, previous.ClientID)
	if err != nil {
		return attempt, err
	}
	return authorizationAttempt{clientID: clientID, code: callback.Get(codeField), verifier: verifier, redirect: redirect, nonce: nonce}, nil
}

// CheckModel checks the catalog first. Missing entries require one small plan-
// token inference: catalog visibility can lag actual access. Never substitute
// a different model or treat an HTTP 200 without completed inference as success.
func CheckModel(ctx context.Context, s Session, model string) error {
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return checkModel(ctx, client, s, model)
}

func checkModel(ctx context.Context, client *http.Client, s Session, model string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Resource+"/models", nil)
	if err != nil {
		return errors.New("could not construct model catalog request")
	}
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("ChatGPT model catalog is unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&catalog) != nil {
		return errors.New("ChatGPT model catalog was not available to this account")
	}
	for _, entry := range catalog.Models {
		if entry.Slug == model && entry.Visibility == "list" {
			return nil
		}
	}
	return probeModel(ctx, client, s, model)
}

func probeModel(ctx context.Context, client *http.Client, s Session, model string) error {
	body, err := json.Marshal(map[string]any{
		"model": model, "store": false, "stream": true,
		"input":     []map[string]string{{"role": "user", "content": "Reply with exactly OK."}},
		"reasoning": map[string]string{"effort": "low"},
	})
	if err != nil {
		return errors.New("could not construct model verification request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, Resource+"/responses", strings.NewReader(string(body)))
	if err != nil {
		return errors.New("could not construct model verification request")
	}
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("ChatGPT model verification was unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	verificationErr := fmt.Errorf("requested model %s did not complete ChatGPT verification; no replacement was selected", model)
	if resp.StatusCode != http.StatusOK {
		return verificationErr
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 2<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
				Model  string `json:"model"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			return verificationErr
		}
		switch event.Type {
		case "response.completed":
			if event.Response.Status == "completed" && event.Response.Model == model {
				return nil
			}
			return verificationErr
		case "response.failed", "response.incomplete", errorField:
			return verificationErr
		}
	}
	return verificationErr
}

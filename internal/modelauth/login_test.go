package modelauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestCallbackBindsStateAndIssuedClient(t *testing.T) {
	const newClientID = "oaiapp_new"
	const pendingState = "pending"
	for _, tc := range []struct {
		name, state, code, clientID, previous, oauthError string
		valid                                             bool
	}{
		{"registration", pendingState, codeField, newClientID, "", "", true},
		{"returning", pendingState, codeField, "", "oaiapp_existing", "", true},
		{"wrong state", fixtureOther, codeField, newClientID, "", "", false},
		{"missing issued id", pendingState, codeField, "", "", "", false},
		{"bootstrap id", pendingState, codeField, dynamicClientID, "", "", false},
		{"switched registration", pendingState, codeField, "oaiapp_other", "oaiapp_existing", "", false},
		{"denied consent", pendingState, "", newClientID, "", "access_denied", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callbackClient(url.Values{stateField: {tc.state}, codeField: {tc.code}, clientIDField: {tc.clientID}, errorField: {tc.oauthError}}, pendingState, tc.previous)
			if (err == nil) != tc.valid {
				t.Fatalf("callback validation = %v", err)
			}
		})
	}
}

type issuerTransport struct{ target *url.URL }

func (rt issuerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	if copy.URL.Host == "auth.openai.com" {
		copy.URL.Scheme, copy.URL.Host = rt.target.Scheme, rt.target.Host
	}
	return http.DefaultTransport.RoundTrip(copy)
}

func TestLoginFlowRegistersReauthorizesAndPreservesAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var authorization url.Values
	subject := fixtureSubject
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": Issuer, "authorization_endpoint": Issuer + "/api/accounts/authorize",
			"token_endpoint": TokenEndpoint, "jwks_uri": Issuer + "/jwks", "id_token_signing_alg_values_supported": []string{string(jose.RS256)}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Algorithm: string(jose.RS256), Use: "sig"}}})
	})
	mux.HandleFunc("/api/accounts/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get(clientIDField) != fixtureClientID ||
			r.Form.Get("redirect_uri") != authorization.Get("redirect_uri") || r.Form.Get(resourceField) != Resource ||
			oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != authorization.Get("code_challenge") {
			t.Error("authorization code was not bound to PKCE, client, redirect and resource")
		}
		payload, err := json.Marshal(map[string]any{"iss": Issuer, "sub": subject, "aud": fixtureClientID,
			nonceField: authorization.Get(nonceField), "exp": time.Now().Add(time.Hour).Unix()})
		if err != nil {
			t.Error(err)
			return
		}
		signed, err := signer.Sign(payload)
		if err != nil {
			t.Error(err)
			return
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: fixtureAccess, RefreshToken: fixtureRefresh, IDToken: raw,
			TokenType: "Bearer", ExpiresIn: 3600, Scope: "openid profile email offline_access resource.invoke " + PlanScope})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: issuerTransport{target: target}, Timeout: 5 * time.Second}
	dir := t.TempDir()
	path, host := filepath.Join(dir, "session.json"), filepath.Join(dir, "host.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openBrowser := mockBrowser(t, &authorization)
	first, err := login(ctx, path, host, openBrowser, client)
	if err != nil {
		t.Fatal(err)
	}
	second, err := login(ctx, path, host, openBrowser, client)
	if err != nil {
		t.Fatal(err)
	}
	if first.HostID != second.HostID || first.ClientID != second.ClientID {
		t.Fatal("host/client registration changed on reauthorization")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	subject = "different-account"
	if _, err := login(ctx, path, host, openBrowser, client); err == nil {
		t.Fatal("changed account silently overwrote saved credentials")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed account validation replaced private credentials")
	}
}

func TestOIDCIdentityChecksSignatureIssuerAudienceExpiryNonceAndAccount(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public := jose.JSONWebKey{Key: &key.PublicKey, KeyID: "fixture", Algorithm: string(jose.RS256), Use: "sig"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{public}})
	}))
	defer server.Close()
	ctx := context.Background()
	verifier := oidc.NewVerifier(Issuer, oidc.NewRemoteKeySet(ctx, server.URL), &oidc.Config{ClientID: fixtureClientID})
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, issuer, audience, nonce, subject string
		expired, valid                         bool
	}{
		{"valid", Issuer, fixtureClientID, nonceField, fixtureSubject, false, true},
		{"issuer", "https://wrong.example", fixtureClientID, nonceField, fixtureSubject, false, false},
		{"audience", Issuer, fixtureOther, nonceField, fixtureSubject, false, false},
		{nonceField, Issuer, fixtureClientID, fixtureOther, fixtureSubject, false, false},
		{fixtureSubject, Issuer, fixtureClientID, nonceField, fixtureOther, false, false},
		{"expired", Issuer, fixtureClientID, nonceField, fixtureSubject, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiry := time.Now().Add(time.Hour)
			if tc.expired {
				expiry = time.Now().Add(-time.Hour)
			}
			payload, err := json.Marshal(map[string]any{"iss": tc.issuer, "aud": tc.audience, nonceField: tc.nonce, "sub": tc.subject, "exp": expiry.Unix()})
			if err != nil {
				t.Fatal(err)
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			_, err = verifyIdentity(ctx, verifier, raw, nonceField, fixtureSession())
			if (err == nil) != tc.valid {
				t.Fatalf("ID validation = %v", err)
			}
			if tc.valid {
				_, err = verifyIdentity(ctx, verifier, raw[:len(raw)-5]+"AAAAA", nonceField, fixtureSession())
				if err == nil {
					t.Fatal("invalid signature accepted")
				}
			}
		})
	}
}

func mockBrowser(t *testing.T, authorization *url.Values) func(string) error {
	t.Helper()
	return func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		*authorization = u.Query()
		if (*authorization).Get(clientIDField) == dynamicClientID {
			if (*authorization).Get("agent_name_hint") != "Sproozi" {
				t.Error("registration name missing")
			}
		} else if (*authorization).Get(clientIDField) != fixtureClientID || (*authorization).Has("agent_name_hint") || (*authorization).Get("id_token_hint") == "" {
			t.Error("returning registration was not retained")
		}
		callback, err := url.Parse((*authorization).Get("redirect_uri"))
		if err != nil {
			return err
		}
		if callback.Hostname() != "127.0.0.1" || callback.Path != "/auth/callback" {
			t.Error("callback is not loopback-only")
		}
		q := url.Values{stateField: {"wrong"}, codeField: {"fixture-code"}, clientIDField: {fixtureClientID}}
		callback.RawQuery = q.Encode()
		resp, err := http.Get(callback.String())
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Error("wrong callback state accepted")
		}
		q.Set(stateField, (*authorization).Get(stateField))
		callback.RawQuery = q.Encode()
		resp, err = http.Get(callback.String())
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
}

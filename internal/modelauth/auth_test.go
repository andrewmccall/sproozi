package modelauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	fixtureClientID = "oaiapp_fixture"
	fixtureSubject  = "account"
	fixtureOther    = "other"
	fixtureRefresh  = "fixture-refresh"
	fixtureAccess   = "fixture-access"
	renewedBearer   = "Bearer new-access"
)

func fixtureSession() Session {
	return Session{Issuer: Issuer, Subject: fixtureSubject, ClientID: fixtureClientID, HostID: "urn:uuid:fixture",
		AccessToken: fixtureAccess, RefreshToken: fixtureRefresh, IDToken: "fixture-id",
		TokenType: "Bearer", ExpiresIn: 3600, SavedAt: time.Now().UTC(), Scopes: []string{PlanScope}}
}

type memoryStore struct {
	session Session
	fail    bool
	loadErr error
	loads   atomic.Int32
}

func (s *memoryStore) Load(context.Context) (Session, error) {
	s.loads.Add(1)
	return s.session, s.loadErr
}
func (s *memoryStore) Replace(_ context.Context, before, after Session) error {
	if s.fail {
		s.fail = false
		return errors.New("temporary storage failure")
	}
	if !s.session.SameCredentials(before) {
		return errors.New("conflict")
	}
	s.session = after
	return nil
}

func TestWorkloadKeepsTheHarnessCredentialAndAccount(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
	r.Header.Set("Authorization", "Bearer harness-own-key")
	r.Header.Set(openAIProjectHeader, "harness-project")
	if err := (Workload{}).Authorize(r); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer harness-own-key" || r.Header.Get(openAIProjectHeader) != "harness-project" {
		t.Fatal("the harness's credential or account was changed")
	}
	for _, header := range []struct{ name, value string }{
		{"Authorization", ""}, {"Authorization", "Bearer "}, {"X-Api-Key", "bad\r\nvalue"},
	} {
		r := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
		r.Header[header.name] = []string{header.value}
		if err := (Workload{}).Authorize(r); err == nil {
			t.Fatalf("missing or invalid %s accepted", header.name)
		}
	}
}

func TestAPIKeyReplacesWorkloadAuthorization(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
	r.Header.Set("Authorization", "Bearer workload-placeholder")
	for _, header := range []string{openAIOrganizationHeader, openAIProjectHeader, chatGPTAccountHeader} {
		r.Header.Set(header, "workload-selected-account")
	}
	if err := (APIKey{Key: "fixture-api-key"}).Authorize(r); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer fixture-api-key" {
		t.Fatal("API key was not injected")
	}
	for _, header := range []string{openAIOrganizationHeader, openAIProjectHeader, chatGPTAccountHeader} {
		if r.Header.Get(header) != "" {
			t.Fatalf("workload selected provider identity through %s", header)
		}
	}
	for _, key := range []string{"", "\r\ninvalid"} {
		if err := (APIKey{Key: key}).Authorize(r); err == nil {
			t.Fatal("invalid API key accepted")
		}
	}
}

func TestChatGPTSerializesRefreshAndPersistsRotatedSession(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.Form.Get(clientIDField) != fixtureClientID ||
			r.Form.Get("grant_type") != refreshTokenField || r.Form.Get(refreshTokenField) != fixtureRefresh ||
			r.Form.Get(resourceField) != Resource || r.Form.Has("scope") {
			t.Error("incorrect OAuth refresh form")
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	store := &memoryStore{session: fixtureSession()}
	store.session.SavedAt = time.Now().Add(-2 * time.Hour)
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
			if err := auth.Authorize(req); err != nil {
				t.Error(err)
				return
			}
			if req.Header.Get("Authorization") != renewedBearer {
				t.Error("stale credentials were injected")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || store.session.RefreshToken != "new-refresh" || !store.session.SavedAt.After(time.Now().Add(-time.Minute)) {
		t.Fatal("refresh did not rotate and persist exactly once")
	}
}

func TestChatGPTDoesNotUseUnsavedCredentialsOrRefreshTwice(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	store := &memoryStore{session: fixtureSession(), fail: true}
	store.session.SavedAt = time.Now().Add(-2 * time.Hour)
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	r := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
	if err := auth.Authorize(r); err == nil || r.Header.Get("Authorization") != "" {
		t.Fatal("unsaved credentials reached inference")
	}
	if err := auth.Authorize(r); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || r.Header.Get("Authorization") != renewedBearer {
		t.Fatal("persistence retry rotated twice")
	}
}

func TestChatGPTParallelRequestsDoNotSerializeWhenNoRefreshIsDue(t *testing.T) {
	store := &memoryStore{session: fixtureSession()}
	auth := NewChatGPT(store, http.DefaultClient)
	auth.tokenEndpoint = "http://127.0.0.1:0" // a refresh would fail
	if err := auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)); err != nil {
		t.Fatal(err)
	}
	// Hold the refresh lock: requests that needed it, or a Secret read, would block.
	auth.mu.Lock()
	defer auth.mu.Unlock()
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 50 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
			if err := auth.Authorize(req); err != nil || req.Header.Get("Authorization") != "Bearer "+fixtureAccess {
				t.Errorf("parallel authorize = %v, %q", err, req.Header.Get("Authorization"))
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parallel requests waited for the refresh lock")
	}
	if store.loads.Load() != 1 {
		t.Fatalf("Secret reads = %d, want 1", store.loads.Load())
	}
}

func TestChatGPTRereadsTheStoredSessionAfterTheReloadInterval(t *testing.T) {
	store := &memoryStore{session: fixtureSession()}
	auth := NewChatGPT(store, http.DefaultClient)
	authorize := func() (string, error) {
		req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
		err := auth.Authorize(req)
		return req.Header.Get("Authorization"), err
	}
	if header, err := authorize(); err != nil || header != "Bearer "+fixtureAccess {
		t.Fatalf("first authorize = %q, %v", header, err)
	}
	// An administrator loads a new sign-in into the Secret.
	store.session.AccessToken = "replaced-access"
	if header, _ := authorize(); header != "Bearer "+fixtureAccess {
		t.Fatalf("authorize within the reload interval = %q", header)
	}
	auth.current.Load().loadedAt = time.Now().Add(-SessionReloadInterval)
	if header, err := authorize(); err != nil || header != "Bearer replaced-access" {
		t.Fatalf("authorize after the reload interval = %q, %v", header, err)
	}
	// A Secret that can no longer be read stops model requests at the next reload.
	store.loadErr = errors.New("forbidden")
	auth.current.Load().loadedAt = time.Now().Add(-SessionReloadInterval)
	if header, err := authorize(); err == nil || header != "" {
		t.Fatalf("authorize with an unreadable Secret = %q, %v", header, err)
	}
	if header, err := authorize(); err == nil || header != "" {
		t.Fatalf("authorize after a failed reload served a cached token: %q, %v", header, err)
	}
}

func TestChatGPTRefreshesFromTheStoredSessionNotTheInMemoryCopy(t *testing.T) {
	var refreshTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		refreshTokens = append(refreshTokens, r.Form.Get(refreshTokenField))
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	store := &memoryStore{session: fixtureSession()}
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	if err := auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)); err != nil {
		t.Fatal(err)
	}
	// The administrator replaces the session; then the in-memory copy expires.
	store.session.RefreshToken = "administrator-refresh"
	store.session.SavedAt = time.Now().Add(-2 * time.Hour)
	auth.current.Load().session.SavedAt = time.Now().Add(-2 * time.Hour)
	req := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
	if err := auth.Authorize(req); err != nil || req.Header.Get("Authorization") != renewedBearer {
		t.Fatalf("refresh = %v", err)
	}
	if len(refreshTokens) != 1 || refreshTokens[0] != "administrator-refresh" || store.session.RefreshToken != "new-refresh" {
		t.Fatalf("refresh used %v, stored %q", refreshTokens, store.session.RefreshToken)
	}
}

func TestChatGPTRefreshErrorsDoNotLeakProviderBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"fixture-refresh and fixture-access"}`))
	}))
	defer server.Close()
	store := &memoryStore{session: fixtureSession()}
	store.session.SavedAt = time.Now().Add(-2 * time.Hour)
	auth := NewChatGPT(store, server.Client())
	auth.tokenEndpoint = server.URL
	err := auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil))
	if err == nil || strings.Contains(err.Error(), "fixture-") {
		t.Fatal("provider error leaked credentials or was ignored")
	}
}

func TestPrivateCredentialFilesAndRequiredPlanScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	session := fixtureSession()
	if err := WritePrivateJSON(path, session); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("public credential file accepted")
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := WritePrivateJSON(link, session); err == nil {
		t.Fatal("symlink overwritten")
	}
	session.Scopes = []string{"openid", "profile"}
	if session.Validate() == nil {
		t.Fatal("identity-only login authorized inference")
	}
	session = fixtureSession()
	session.ClientID = dynamicClientID
	if session.Validate() == nil {
		t.Fatal("bootstrap client accepted as issued client")
	}
}

type slowStore struct {
	*memoryStore
	delay time.Duration
}

func (s slowStore) Load(ctx context.Context) (Session, error) {
	time.Sleep(s.delay)
	return s.memoryStore.Load(ctx)
}

func TestChatGPTReloadIntervalStartsBeforeTheSecretRead(t *testing.T) {
	store := &memoryStore{session: fixtureSession()}
	auth := NewChatGPT(slowStore{memoryStore: store, delay: 30 * time.Millisecond}, http.DefaultClient)
	auth.reloadInterval = 20 * time.Millisecond // shorter than one read
	for range 2 {
		if err := auth.Authorize(httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if loads := store.loads.Load(); loads != 2 {
		t.Fatalf("Secret reads = %d, want 2: a read slower than the reload interval extended the cached session", loads)
	}
}

func TestTokenInstallsTheGatewaySignInWithProviderHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	r.Header.Set("X-Api-Key", "workload-placeholder")
	r.Header.Set("Authorization", "Bearer workload-placeholder")
	r.Header.Set("Anthropic-Beta", "context-management-2025-06-27")
	token := Token{Value: "sk-ant-oat01-gateway-held", Headers: map[string]string{"Anthropic-Beta": "oauth-2025-04-20"}}
	if err := token.Authorize(r); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer sk-ant-oat01-gateway-held" || r.Header.Get("X-Api-Key") != "" ||
		r.Header.Get("Anthropic-Beta") != "context-management-2025-06-27,oauth-2025-04-20" {
		t.Fatalf("headers: %v", r.Header)
	}
	if err := (Token{}).Authorize(r); err == nil {
		t.Fatal("an empty token was accepted")
	}
}

func TestFileSessionStoreReplacesOnlyTheSessionItRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chatgpt-session.json")
	before := fixtureSession()
	if err := WritePrivateJSON(path, before); err != nil {
		t.Fatal(err)
	}
	store := NewFileSessionStore(path)
	after := before
	after.AccessToken, after.RefreshToken = "rotated-access", "rotated-refresh"
	if err := store.Replace(t.Context(), before, after); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(t.Context()); err != nil || loaded.RefreshToken != "rotated-refresh" {
		t.Fatalf("loaded %+v %v", loaded, err)
	}
	if err := store.Replace(t.Context(), before, after); err == nil {
		t.Fatal("a stale refresh replaced the newer session")
	}
}

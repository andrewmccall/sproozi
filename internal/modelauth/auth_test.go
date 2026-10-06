package modelauth

import (
	"context"
	"encoding/json"
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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	fixtureClientID = "oaiapp_fixture"
	fixtureSubject  = "account"
	fixtureOther    = "other"
	fixtureRefresh  = "fixture-refresh"
	fixtureAccess   = "fixture-access"
)

func fixtureSession() Session {
	return Session{Issuer: Issuer, Subject: fixtureSubject, ClientID: fixtureClientID, HostID: "urn:uuid:fixture",
		AccessToken: fixtureAccess, RefreshToken: fixtureRefresh, IDToken: "fixture-id",
		TokenType: "Bearer", ExpiresIn: 3600, SavedAt: time.Now().UTC(), Scopes: []string{PlanScope}}
}

type memoryStore struct {
	session Session
	fail    bool
}

func (s *memoryStore) Load(context.Context) (Session, error) { return s.session, nil }
func (s *memoryStore) Replace(_ context.Context, before, after Session) error {
	if s.fail {
		s.fail = false
		return errors.New("temporary storage failure")
	}
	if !sameCredentials(s.session, before) {
		return errors.New("conflict")
	}
	s.session = after
	return nil
}

func TestAPIKeyReplacesWorkloadAuthorization(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, Resource+"/responses", nil)
	r.Header.Set("Authorization", "Bearer workload-placeholder")
	if err := (APIKey{Key: "fixture-api-key"}).Authorize(r); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer fixture-api-key" {
		t.Fatal("API key was not injected")
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
			if req.Header.Get("Authorization") != "Bearer new-access" {
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
	if calls.Load() != 1 || r.Header.Get("Authorization") != "Bearer new-access" {
		t.Fatal("persistence retry rotated twice")
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

func TestKubernetesSessionStoreRejectsStaleCredentialWriter(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	original := fixtureSession()
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: SessionSecretName}, Data: map[string][]byte{SessionSecretKey: data}}
	store := KubernetesSessionStore{Namespace: "system", Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()}
	updated := original
	updated.RefreshToken = "rotated"
	if err := store.Replace(context.Background(), original, updated); err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(context.Background(), original, original); err == nil {
		t.Fatal("stale writer overwrote rotating credentials")
	}
	got, err := store.Load(context.Background())
	if err != nil || got.RefreshToken != "rotated" {
		t.Fatal("rotating credential set was not preserved")
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

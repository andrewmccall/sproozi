package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/andrewmccall/sproozi/internal/modelauth"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSessionStoreRejectsStaleCredentialWriter(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	original := modelauth.Session{
		ClientID: "fixture-client", Subject: "fixture-account",
		AccessToken: "fixture-access", RefreshToken: "fixture-refresh",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: SessionSecretName},
		Data:       map[string][]byte{SessionSecretKey: data},
	}
	store := SessionStore{
		Namespace: "system", Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
	}
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

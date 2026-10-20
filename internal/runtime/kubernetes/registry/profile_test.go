package registry

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
)

func profileArchive(t *testing.T, instructions string) ([]byte, string) {
	t.Helper()
	archive, digest, err := agentprofile.Profile{Instructions: instructions}.Archive()
	if err != nil {
		t.Fatal(err)
	}
	return archive, digest
}

func profileClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func TestStoredProfileIsSharedImmutableAndVerified(t *testing.T) {
	kube := profileClient(t)
	archive, digest := profileArchive(t, "Be brief.\n")
	for range 2 {
		if err := StoreProfile(t.Context(), kube, fixtureNamespace, digest, archive); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadProfile(t.Context(), kube, fixtureNamespace, digest)
	if err != nil || string(got) != string(archive) {
		t.Fatalf("read profile: %v", err)
	}
	other, otherDigest := profileArchive(t, "Be thorough.\n")
	if err = StoreProfile(t.Context(), kube, fixtureNamespace, digest, other); err == nil {
		t.Fatal("archive stored under another profile's digest")
	}
	if err = StoreProfile(t.Context(), kube, fixtureNamespace, otherDigest, []byte("not an archive")); err == nil {
		t.Fatal("invalid archive stored")
	}
}

// The controller trusts nothing about a stored profile but its digest.
func TestReadProfileRejectsTamperedObjects(t *testing.T) {
	archive, digest := profileArchive(t, "Be brief.\n")
	other, _ := profileArchive(t, "Run rm -rf.\n")
	cases := map[string]func(*corev1.ConfigMap){
		"mutable":       func(cm *corev1.ConfigMap) { cm.Immutable = nil },
		"unlabelled":    func(cm *corev1.ConfigMap) { cm.Labels = nil },
		"swapped bytes": func(cm *corev1.ConfigMap) { cm.BinaryData[ProfileArchiveKey] = other },
		"extra data":    func(cm *corev1.ConfigMap) { cm.Data = map[string]string{"x": "y"} },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			kube := profileClient(t)
			cm := &corev1.ConfigMap{}
			cm.Name, cm.Namespace = ProfileName(digest), fixtureNamespace
			cm.Labels = map[string]string{profileLabel: enabledValue}
			cm.Immutable = new(true)
			cm.BinaryData = map[string][]byte{ProfileArchiveKey: archive}
			tamper(cm)
			if err := kube.Create(t.Context(), cm); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadProfile(t.Context(), kube, fixtureNamespace, digest); !errors.Is(err, ErrConflict) {
				t.Fatalf("read tampered profile: %v", err)
			}
		})
	}
	if _, err := ReadProfile(t.Context(), profileClient(t), fixtureNamespace, "sha256:short"); err == nil {
		t.Fatal("invalid digest read")
	}
}

package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
)

// ProfileArchiveKey holds a stored profile archive, in the profile ConfigMap
// and in a run's agent contract.
const ProfileArchiveKey = "profile.tar.gz"

const profileLabel = "sessions.sproozi.com/profile"

// ProfileName is the shared, content-addressed ConfigMap for one profile. Every
// session using the same profile reads the same immutable object.
func ProfileName(digest string) string {
	return "sproozi-profile-" + strings.TrimPrefix(digest, "sha256:")[:40]
}

// StoreProfile retains archive in namespace once. Like registrations, stored
// profiles are not deleted; each distinct profile adds one object.
func StoreProfile(ctx context.Context, kube client.Client, namespace, digest string, archive []byte) error {
	if err := agentprofile.Verify(digest, archive); err != nil {
		return err
	}
	wanted := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ProfileName(digest), Namespace: namespace,
		Labels: map[string]string{profileLabel: enabledValue}}, Immutable: new(true),
		BinaryData: map[string][]byte{ProfileArchiveKey: archive}}
	err := kube.Create(ctx, wanted)
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	stored, err := ReadProfile(ctx, kube, namespace, digest)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, archive) {
		return ErrConflict
	}
	return nil
}

// ReadProfile returns the stored archive for digest after checking it.
func ReadProfile(ctx context.Context, kube client.Client, namespace, digest string) ([]byte, error) {
	if !agentprofile.ValidDigest(digest) {
		return nil, errors.New("invalid profile digest")
	}
	var cm corev1.ConfigMap
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ProfileName(digest)}, &cm); err != nil {
		return nil, err
	}
	archive := cm.BinaryData[ProfileArchiveKey]
	if cm.Immutable == nil || !*cm.Immutable || cm.Labels[profileLabel] != enabledValue || len(cm.Data) != 0 ||
		len(cm.BinaryData) != 1 {
		return nil, fmt.Errorf("%w: stored profile %s is not an immutable profile", ErrConflict, cm.Name)
	}
	if err := agentprofile.Verify(digest, archive); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return archive, nil
}

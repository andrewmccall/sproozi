// Package kubernetes persists trusted model credentials in a namespace-local Secret.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/andrewmccall/sproozi/internal/modelauth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const SessionSecretName = "model-gateway-chatgpt"
const SessionSecretKey = "session.json"

var _ modelauth.SessionStore = SessionStore{}

// SessionStore persists rotating tokens in a namespace-local Secret.
// Gateway RBAC is restricted to get/update on this exact Secret, not all Secrets.
type SessionStore struct {
	Client    client.Client
	Namespace string
}

func (s SessionStore) secret(ctx context.Context) (*corev1.Secret, modelauth.Session, error) {
	secret := &corev1.Secret{}
	var session modelauth.Session
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: SessionSecretName}, secret); err != nil {
		return nil, session, errors.New("could not read ChatGPT credential Secret")
	}
	if json.Unmarshal(secret.Data[SessionSecretKey], &session) != nil {
		return nil, session, errors.New("invalid ChatGPT credential Secret")
	}
	return secret, session, nil
}

func (s SessionStore) Load(ctx context.Context) (modelauth.Session, error) {
	_, session, err := s.secret(ctx)
	return session, err
}

func (s SessionStore) Replace(ctx context.Context, before, after modelauth.Session) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		secret, current, err := s.secret(ctx)
		if err != nil {
			return err
		}
		if !before.SameCredentials(current) {
			return errors.New("ChatGPT credential set changed during refresh")
		}
		data, err := json.Marshal(after)
		if err != nil {
			return errors.New("could not encode ChatGPT credentials")
		}
		secret.Data[SessionSecretKey] = data
		// Remove kubectl's duplicate credential copy when rotating the Secret.
		delete(secret.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		return s.Client.Update(ctx, secret)
	})
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	testManagedByLabel = "sproozi.com/managed-by"
)

const (
	testRunUIDLabel = "sproozi.com/agentrun-uid"
)

const (
	testRunNamespaceLabel = "sproozi.com/agentrun-namespace"
)

const (
	testRunLabel = "sproozi.com/agentrun"
)

const (
	testManagedByValue = "sproozi-controller"
)

// Sentinel errors returned by TokenValidator and RunFinder implementations.
var (
	// ErrTokenInvalid is returned when a bearer token is invalid or expired.
	ErrTokenInvalid = errors.New("gateway: token invalid or expired")

	// ErrRunNotFound is returned when no active AgentRun is found for the identity.
	ErrRunNotFound = errors.New("gateway: no active AgentRun found for this identity")

	// ErrRunNotRunning is returned when the matched AgentRun is not in Running phase.
	ErrRunNotRunning = errors.New("gateway: AgentRun is not in Running phase")

	// ErrNotServiceAccount is returned when the token username is not a ServiceAccount.
	ErrNotServiceAccount      = errors.New("gateway: token is not a ServiceAccount token")
	ErrWrongAgentNamespace    = errors.New("gateway: ServiceAccount is outside the agent namespace")
	ErrServiceAccountMismatch = errors.New("gateway: ServiceAccount does not match the AgentRun")
	ErrDuplicateRunIdentity   = errors.New("gateway: multiple AgentRuns match the ServiceAccount")
)

const (
	runLabel          = testRunLabel
	runNamespaceLabel = testRunNamespaceLabel
	runUIDLabel       = testRunUIDLabel
	managedByLabel    = testManagedByLabel
	managedByValue    = testManagedByValue
)

// ServiceAccountIdentity is the complete token subject. UID comes from the
// TokenReview and prevents a deleted ServiceAccount name from being reused.
type ServiceAccountIdentity struct {
	Namespace string
	Name      string
	UID       string
}

// RunIdentity holds the verified AgentRun and its resolved Policy and Template.
type RunIdentity struct {
	// Run is the matched AgentRun.
	Run *sprooziv1alpha1.AgentRun

	// Policy is the AgentPolicy governing this run.
	Policy *sprooziv1alpha1.AgentPolicy

	// Template is the AgentTemplate that defines this run.
	Template *sprooziv1alpha1.AgentTemplate

	// SAName is the service account name bound to the run.
	SAName      string
	SANamespace string
	SAUID       string
}

// Authenticator composes token validation and run lookup at the shared
// gateway identity seam.
type Authenticator struct {
	Tokens TokenValidator
	Runs   RunFinder
}

func (a Authenticator) Authenticate(ctx context.Context, token string) (*RunIdentity, error) {
	if a.Tokens == nil || a.Runs == nil || token == "" {
		return nil, ErrTokenInvalid
	}
	sa, err := a.Tokens.Validate(ctx, token)
	if err != nil {
		return nil, err
	}
	identity, err := a.Runs.Find(ctx, sa)
	if err != nil {
		return nil, err
	}
	if identity == nil || identity.Run == nil || identity.Policy == nil {
		return nil, ErrRunNotFound
	}
	return identity, nil
}

// TokenValidator validates a bearer token and returns the complete ServiceAccount identity.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (ServiceAccountIdentity, error)
}

// RunFinder resolves a ServiceAccount identity to the owning active AgentRun with its policy.
type RunFinder interface {
	Find(ctx context.Context, serviceAccount ServiceAccountIdentity) (*RunIdentity, error)
}

// K8sTokenValidator validates bearer tokens via the Kubernetes TokenReview API.
type K8sTokenValidator struct {
	// clientset is used to call the TokenReview API.
	clientset kubernetes.Interface

	// audience is the expected token audience.
	audience string
}

// NewK8sTokenValidator returns a K8sTokenValidator backed by the given clientset
// that validates tokens for the specified audience.
func NewK8sTokenValidator(clientset kubernetes.Interface, audience string) *K8sTokenValidator {
	return &K8sTokenValidator{clientset: clientset, audience: audience}
}

// Validate submits a TokenReview to the Kubernetes API and returns the
// ServiceAccount namespace and name on success.
func (v *K8sTokenValidator) Validate(ctx context.Context, token string) (ServiceAccountIdentity, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{v.audience},
		},
	}
	result, err := v.clientset.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return ServiceAccountIdentity{}, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}
	if !result.Status.Authenticated {
		return ServiceAccountIdentity{}, ErrTokenInvalid
	}
	namespace, name, err := ParseServiceAccountUsername(result.Status.User.Username)
	if err != nil {
		return ServiceAccountIdentity{}, err
	}
	if result.Status.User.UID == "" {
		return ServiceAccountIdentity{}, ErrTokenInvalid
	}
	return ServiceAccountIdentity{Namespace: namespace, Name: name, UID: result.Status.User.UID}, nil
}

// K8sRunFinder resolves AgentRuns through the immutable ServiceAccount owner
// labels. This keeps authentication bounded to the named ServiceAccount, run,
// template, and policy rather than scanning every run in the cluster.
type K8sRunFinder struct {
	// client is the controller-runtime client used to list and get resources.
	client         client.Client
	agentNamespace string
}

// NewK8sRunFinder returns a K8sRunFinder backed by the given controller-runtime client.
func NewK8sRunFinder(c client.Client, agentNamespace string) *K8sRunFinder {
	return &K8sRunFinder{client: c, agentNamespace: agentNamespace}
}

// Find returns the active AgentRun owned by the complete ServiceAccount
// identity. ServiceAccount name reuse and stale owner labels fail closed.
func (f *K8sRunFinder) Find(ctx context.Context, serviceAccount ServiceAccountIdentity) (*RunIdentity, error) {
	if serviceAccount.Namespace != f.agentNamespace {
		return nil, ErrWrongAgentNamespace
	}
	if serviceAccount.Name == "" || serviceAccount.UID == "" {
		return nil, ErrTokenInvalid
	}
	var serviceAccountObject corev1.ServiceAccount
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: serviceAccount.Namespace, Name: serviceAccount.Name}, &serviceAccountObject); err != nil {
		return nil, ErrRunNotFound
	}
	if string(serviceAccountObject.UID) != serviceAccount.UID || serviceAccountObject.Labels[managedByLabel] != managedByValue {
		return nil, ErrServiceAccountMismatch
	}
	labels := serviceAccountObject.Labels
	runName, runNamespace, runUID := labels[runLabel], labels[runNamespaceLabel], labels[runUIDLabel]
	if runName == "" || runNamespace == "" || runUID == "" {
		return nil, ErrServiceAccountMismatch
	}
	var run sprooziv1alpha1.AgentRun
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: runNamespace, Name: runName}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if string(run.UID) != runUID || run.Status.Identity.ServiceAccountName != serviceAccount.Name || run.Namespace != runNamespace {
		return nil, ErrServiceAccountMismatch
	}
	if run.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning || run.Spec.Cancel || !run.DeletionTimestamp.IsZero() {
		return nil, ErrRunNotRunning
	}
	if runName == "" {
		return nil, ErrRunNotFound
	}

	var tmpl sprooziv1alpha1.AgentTemplate
	if err := f.client.Get(ctx, client.ObjectKey{
		Namespace: run.Namespace,
		Name:      run.Spec.TemplateRef.Name,
	}, &tmpl); err != nil {
		return nil, err
	}

	var pol sprooziv1alpha1.AgentPolicy
	if err := f.client.Get(ctx, client.ObjectKey{
		Namespace: run.Namespace,
		Name:      tmpl.Spec.PolicyRef.Name,
	}, &pol); err != nil {
		return nil, err
	}

	return &RunIdentity{Run: &run, Policy: &pol, Template: &tmpl, SAName: serviceAccount.Name, SANamespace: serviceAccount.Namespace, SAUID: serviceAccount.UID}, nil
}

// FakeTokenValidator is an exported test double for TokenValidator.
type FakeTokenValidator struct {
	// SANamespace is the namespace returned by Validate.
	SANamespace string

	// SAName is the service account name returned by Validate.
	SAName string

	// Err is the error returned by Validate.
	Err error
}

// Validate returns the pre-configured SANamespace, SAName, and Err.
func (f *FakeTokenValidator) Validate(_ context.Context, _ string) (ServiceAccountIdentity, error) {
	return ServiceAccountIdentity{Namespace: f.SANamespace, Name: f.SAName, UID: "test-service-account-uid"}, f.Err
}

// FakeRunFinder is an exported test double for RunFinder.
type FakeRunFinder struct {
	// Identity is the RunIdentity returned by Find.
	Identity *RunIdentity

	// Err is the error returned by Find.
	Err error
}

// Find returns the pre-configured Identity and Err.
func (f *FakeRunFinder) Find(_ context.Context, _ ServiceAccountIdentity) (*RunIdentity, error) {
	return f.Identity, f.Err
}

// ParseServiceAccountUsername parses a Kubernetes ServiceAccount username of
// the form "system:serviceaccount:<namespace>:<name>" and returns the namespace
// and name. Returns ErrNotServiceAccount if the string does not match the
// expected format.
func ParseServiceAccountUsername(username string) (namespace, name string, err error) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return "", "", ErrNotServiceAccount
	}
	rest := strings.TrimPrefix(username, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", ErrNotServiceAccount
	}
	return parts[0], parts[1], nil
}

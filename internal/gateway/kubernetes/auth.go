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

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	runLabel          = "sproozi.com/agentrun"
	runNamespaceLabel = "sproozi.com/agentrun-namespace"
	runUIDLabel       = "sproozi.com/agentrun-uid"
	managedByLabel    = "sproozi.com/managed-by"
	managedByValue    = "sproozi-controller"
)

// Sentinel errors returned by Authenticator.
var (
	// ErrTokenInvalid is returned when a bearer token is invalid or expired.
	ErrTokenInvalid = gateway.ErrTokenInvalid

	// ErrRunNotFound is returned when no active AgentRun is found for the identity.
	ErrRunNotFound = errors.New("gateway: no active AgentRun found for this identity")

	// ErrRunNotRunning is returned when the matched AgentRun is not in Running phase.
	ErrRunNotRunning = errors.New("gateway: AgentRun is not in Running phase")

	// ErrPolicyChanged is returned when the run's template now references a
	// different AgentPolicy from the one the run was admitted under.
	ErrPolicyChanged = errors.New("gateway: AgentTemplate references a different AgentPolicy from the run's admission")

	// ErrNotServiceAccount is returned when the token username is not a ServiceAccount.
	ErrNotServiceAccount      = errors.New("gateway: token is not a ServiceAccount token")
	ErrWrongAgentNamespace    = errors.New("gateway: ServiceAccount is outside the agent namespace")
	ErrServiceAccountMismatch = errors.New("gateway: ServiceAccount does not match the AgentRun")

	// errAuthorityChanged explains a revocation because what the run may do
	// changed: its policy, template, capabilities or allocation.
	errAuthorityChanged = errors.New("gateway: the AgentRun's policy, template, capabilities or allocation changed")
)

// ServiceAccountIdentity is the complete token subject. UID comes from the
// TokenReview and prevents a deleted ServiceAccount name from being reused.
type ServiceAccountIdentity struct {
	Namespace string
	Name      string
	UID       string
}

// LiveRecheckInterval bounds how long a session of one execution relies on the
// watch cache alone. If a watch stalls, the first revalidation after this
// interval claims a live read, and every session of the execution reads live
// from that claim until a live read completes. A revocation therefore reaches
// every open session within LiveRecheckInterval plus one revocation tick plus
// one authority timeout: under 10s + 1s + 5s after the revocation with the
// gateway defaults, and about 11s when the API server answers promptly.
const LiveRecheckInterval = 10 * time.Second

// Authenticator is the Kubernetes execution authority at the shared gateway
// identity seam. It resolves a projected ServiceAccount token to the owning
// active AgentRun, and resolves the same owner again on every revalidation.
//
// Authentication reads through to the API server: a new session is always
// admitted against live state. Revalidation of open sessions, every request
// and every revocation tick, reads AgentRun, AgentTemplate, AgentPolicy and
// ServiceAccount from the watch cache when one is configured, so API load does
// not grow with sessions times ticks. A watch delay is acceptable there because
// the session was admitted live, and any revocation is confirmed live before a
// session is closed. Managed-session registrations and sandbox Pods stay live
// reads (see cmd/gateway): a session move revokes the source through them, and
// it must not overlap with the destination.
type Authenticator struct {
	clientset      kubernetes.Interface
	audience       string
	live           client.Client
	cached         client.Client
	agentNamespace string

	mu       sync.Mutex
	lastLive map[gateway.Reference]liveCheck
	now      func() time.Time
}

// NewAuthenticator validates tokens for audience through the TokenReview API
// and resolves their owners in agentNamespace with the uncached client c.
func NewAuthenticator(clientset kubernetes.Interface, audience string, c client.Client, agentNamespace string) *Authenticator {
	return &Authenticator{clientset: clientset, audience: audience, live: c, agentNamespace: agentNamespace,
		lastLive: map[gateway.Reference]liveCheck{}, now: time.Now}
}

// WithWatchCache serves revalidation from cached, a client whose AgentRun,
// AgentTemplate, AgentPolicy and ServiceAccount reads come from a watch cache.
func (a *Authenticator) WithWatchCache(cached client.Client) *Authenticator {
	a.cached = cached
	return a
}

func (a *Authenticator) Authenticate(ctx context.Context, token string) (*gateway.ExecutionGrant, error) {
	if token == "" {
		return nil, ErrTokenInvalid
	}
	// TokenReview is never cached: it is the only check of the token itself.
	serviceAccount, err := a.reviewToken(ctx, token)
	if err != nil {
		return nil, err
	}
	started := a.now()
	grant, err := a.Find(ctx, serviceAccount)
	if err == nil {
		a.recordLive(grant.Authority, started, true)
	}
	return grant, err
}

// Revalidate resolves the owning resources again, binding an inspected session
// to the same execution, policy, template and placement allocation.
func (a *Authenticator) Revalidate(ctx context.Context, grant *gateway.ExecutionGrant) error {
	if grant == nil || grant.Execution.Backend != gateway.BackendKubernetes || grant.Authority.Backend != gateway.BackendKubernetes {
		return gateway.ErrGrantRevoked
	}
	identity := ServiceAccountIdentity{Namespace: grant.Authority.Namespace, Name: grant.Authority.Name, UID: grant.Authority.UID}
	if a.cached == nil || a.claimLiveCheck(grant.Authority) {
		return a.revalidateLive(ctx, grant, identity)
	}
	// A cached pass counts only if no live read became due or was claimed while
	// it ran: a slow cached read (managed sessions read their registration and
	// Pod live) must not carry stale state past the recheck. The live read then
	// shares this revalidation's authority deadline.
	if fresh, err := a.find(ctx, a.cached, identity); err == nil && grant.SameAuthority(fresh) &&
		!a.claimLiveCheck(grant.Authority) {
		return nil
	}
	// The cache can lag the live read that admitted this session, for example
	// just after the run starts. Confirm against the API server before revoking.
	return a.revalidateLive(ctx, grant, identity)
}

func (a *Authenticator) revalidateLive(ctx context.Context, grant *gateway.ExecutionGrant, identity ServiceAccountIdentity) error {
	started := a.now()
	fresh, err := a.Find(ctx, identity)
	if err == nil && !grant.SameAuthority(fresh) {
		err = errAuthorityChanged
	}
	if err != nil {
		a.recordLive(grant.Authority, started, false)
		return fmt.Errorf("%w: %w", gateway.ErrGrantRevoked, err)
	}
	a.recordLive(grant.Authority, started, true)
	return nil
}

// liveCheck is the newest live read of one execution's authority.
type liveCheck struct {
	// at is when that read, or the pending claim, started.
	at time.Time
	// pending marks a claimed live read that has not completed. Until it does,
	// every session of the execution reads live rather than trust a cache the
	// claim found too old, so a stalled read cannot extend the stale window.
	pending bool
	// failed makes every session of the execution read live until a live read
	// succeeds, so one failed check closes all of them on their next tick.
	failed bool
}

// claimLiveCheck reports whether a session must read live: the execution's
// last live read failed, is still pending, or is older than
// LiveRecheckInterval. A due recheck is claimed, so that when live reads are
// prompt, concurrent sessions of one execution do not all read live.
func (a *Authenticator) claimLiveCheck(authority gateway.Reference) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	last, ok := a.lastLive[authority]
	if ok && !last.failed && !last.pending && now.Sub(last.at) < LiveRecheckInterval {
		return false
	}
	if !ok || (!last.failed && !last.pending) {
		a.lastLive[authority] = liveCheck{at: now, pending: true}
	}
	return true
}

// recordLive records a completed live read that started at started. A failure
// always applies. A success applies only if it started no earlier than the
// newest recorded read or claim, so a slow read that began before a claim
// cannot clear it with older state.
func (a *Authenticator) recordLive(authority gateway.Reference, started time.Time, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for key, last := range a.lastLive {
		if now.Sub(last.at) >= 2*LiveRecheckInterval {
			delete(a.lastLive, key) // the execution has no recent session
		}
	}
	if !ok {
		a.lastLive[authority] = liveCheck{at: now, failed: true}
		return
	}
	if last, found := a.lastLive[authority]; found && started.Before(last.at) {
		return
	}
	a.lastLive[authority] = liveCheck{at: started}
}

// reviewToken submits a TokenReview and returns the complete ServiceAccount subject.
func (a *Authenticator) reviewToken(ctx context.Context, token string) (ServiceAccountIdentity, error) {
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{a.audience},
		},
	}
	result, err := a.clientset.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
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

// Find returns the active AgentRun owned by the complete ServiceAccount
// identity, read through to the API server. ServiceAccount name reuse and
// stale owner labels fail closed.
func (a *Authenticator) Find(ctx context.Context, serviceAccount ServiceAccountIdentity) (*gateway.ExecutionGrant, error) {
	return a.find(ctx, a.live, serviceAccount)
}

func (a *Authenticator) find(ctx context.Context, reader client.Client, serviceAccount ServiceAccountIdentity) (*gateway.ExecutionGrant, error) {
	if serviceAccount.Namespace != a.agentNamespace {
		return nil, ErrWrongAgentNamespace
	}
	if serviceAccount.Name == "" || serviceAccount.UID == "" {
		return nil, ErrTokenInvalid
	}
	var serviceAccountObject corev1.ServiceAccount
	if err := reader.Get(ctx, client.ObjectKey{Namespace: serviceAccount.Namespace, Name: serviceAccount.Name}, &serviceAccountObject); err != nil {
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
	if err := reader.Get(ctx, client.ObjectKey{Namespace: runNamespace, Name: runName}, &run); err != nil {
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
	var tmpl sprooziv1alpha1.AgentTemplate
	if err := reader.Get(ctx, client.ObjectKey{
		Namespace: run.Namespace,
		Name:      run.Spec.TemplateRef.Name,
	}, &tmpl); err != nil {
		return nil, err
	}
	// The run counts against the policy it was admitted under; never grant it
	// another policy's permissions. The controller fails it with PolicyChanged.
	if registry.AdmittedPolicyChanged(&run, &tmpl) {
		return nil, ErrPolicyChanged
	}

	var pol sprooziv1alpha1.AgentPolicy
	if err := reader.Get(ctx, client.ObjectKey{
		Namespace: run.Namespace,
		Name:      tmpl.Spec.PolicyRef.Name,
	}, &pol); err != nil {
		return nil, err
	}
	record, err := registry.ResolveRun(ctx, reader, &run, &tmpl)
	if err != nil {
		return nil, err
	}
	if err := registry.RequireActive(ctx, reader, &run, record, a.agentNamespace); err != nil {
		return nil, err
	}
	if deadline := registry.EffectiveDeadline(&run, &pol, record); !deadline.IsZero() && !deadline.After(time.Now()) {
		return nil, ErrRunNotRunning
	}

	grant := &gateway.ExecutionGrant{
		Execution:    gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: run.Namespace, Name: run.Name, UID: string(run.UID)},
		Authority:    gateway.Reference{Backend: gateway.BackendKubernetes, Namespace: serviceAccount.Namespace, Name: serviceAccount.Name, UID: serviceAccount.UID},
		Capabilities: append([]sprooziv1alpha1.CapabilityKind(nil), run.Spec.Capabilities...),
		Policy:       gateway.PolicySnapshot{Name: pol.Name, UID: string(pol.UID), Generation: pol.Generation, Spec: *pol.Spec.DeepCopy()},
		Template:     gateway.TemplateSnapshot{Name: tmpl.Name, UID: string(tmpl.UID), Generation: tmpl.Generation, Spec: *tmpl.Spec.DeepCopy()},
	}
	grant.ExecutionID = string(run.UID)
	if record != nil {
		grant.ExecutionID = record.SessionID
		grant.Managed = true
		grant.Allocations = make(map[sprooziv1alpha1.CapabilityKind]gateway.Allocation, len(record.Ceilings))
		for capability, ceiling := range record.Ceilings {
			grant.Allocations[capability] = gateway.Allocation{AccountKey: record.RunUID + "/" + string(capability), Ceiling: ceiling}
		}
	}
	return grant, nil
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

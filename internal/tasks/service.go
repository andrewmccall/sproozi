// Package tasks exposes fixed administrator workflows to persistent MCP clients.
// Its identity is a durable caller, independent of disposable worker identity.
package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	patchValue = "value"
	patchPath  = "path"
)

const (
	ownerAnnotation    = "tasks.sproozi.com/owner"
	workflowAnnotation = "tasks.sproozi.com/workflow"
	expiryAnnotation   = "tasks.sproozi.com/expires-at"
	requestWindow      = time.Hour
	retentionMargin    = 5 * time.Minute
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

var (
	ErrInvalid     = errors.New("invalid task request")
	ErrDenied      = errors.New("task access denied")
	ErrConflict    = errors.New("request ID conflicts with an existing task")
	ErrExpired     = errors.New("submission expired; use status for an existing task")
	ErrGone        = errors.New("task incarnation is unavailable")
	ErrUnavailable = errors.New("task service unavailable")
)

// Workflow contains only trusted authority. Tool arguments cannot override it.
type Workflow struct {
	Template     string               `json:"template"`
	Capabilities []api.CapabilityKind `json:"capabilities"`
}

// Config is administrator-owned, certificate- and credential-free configuration.
type Config struct {
	Namespace string              `json:"namespace"`
	Principal string              `json:"principal"`
	Workflows map[string]Workflow `json:"workflows"`
}

// Submit carries untrusted intent, with a replay deadline retained on the Run.
type Submit struct {
	Workflow  string `json:"workflow" jsonschema:"Approved workflow name"`
	RequestID string `json:"requestId" jsonschema:"Stable operation key; reuse this exact key and all input fields after an uncertain response"`
	ExpiresAt string `json:"expiresAt" jsonschema:"UTC RFC3339 replay deadline, at most one hour ahead; store and reuse the exact deadline on retries"`
	Task      string `json:"task" jsonschema:"Untrusted task for the approved worker, at most 16384 characters"`
}

// Reference binds status and cancellation to one incarnation, never just a name.
type Reference struct {
	ID  string `json:"id"`
	UID string `json:"uid"`
}

// View exposes lifecycle and explicitly untrusted answer data, never credentials.
type View struct {
	Reference
	Phase                 api.AgentRunPhase   `json:"phase"`
	CancellationRequested bool                `json:"cancellationRequested"`
	StartedAt             string              `json:"startedAt,omitempty"`
	CompletedAt           string              `json:"completedAt,omitempty"`
	Result                *api.AgentRunResult `json:"result,omitempty"`
}

// Service hides Kubernetes write authority, workflow selection and retry rules.
type Service struct {
	kube   client.Client
	config Config
	now    func() time.Time
}

func NewService(kube client.Client, config Config) (*Service, error) {
	if config.Namespace == "" || !identifier.MatchString(config.Principal) || len(config.Workflows) == 0 {
		return nil, fmt.Errorf("invalid task service configuration")
	}
	workflows := make(map[string]Workflow, len(config.Workflows))
	for name, workflow := range config.Workflows {
		if !identifier.MatchString(name) || workflow.Template == "" || len(workflow.Capabilities) == 0 {
			return nil, fmt.Errorf("invalid workflow %q", name)
		}
		workflow.Capabilities = slices.Clone(workflow.Capabilities)
		slices.Sort(workflow.Capabilities)
		workflow.Capabilities = slices.Compact(workflow.Capabilities)
		for _, capability := range workflow.Capabilities {
			if !validCapability(capability) {
				return nil, fmt.Errorf("invalid workflow capability")
			}
		}
		workflows[name] = workflow
	}
	config.Workflows = workflows
	return &Service{kube: kube, config: config, now: time.Now}, nil
}

func validCapability(capability api.CapabilityKind) bool {
	if _, ok := capability.MCPServerName(); ok {
		return true
	}
	return slices.Contains([]api.CapabilityKind{api.CapabilityKubernetesRead, api.CapabilityGitHubPullRequest,
		api.CapabilityModelInference, api.CapabilityNetworkEgress, api.CapabilityPackagesInstall}, capability)
}

// Submit atomically converges on one Run during the explicit replay lifetime.
// Administrative deletion or shortening retention is an explicit guarantee reset.
func (s *Service) Submit(ctx context.Context, request Submit) (View, error) {
	workflow, ok := s.config.Workflows[request.Workflow]
	if !ok {
		return View{}, ErrDenied
	}
	expires, err := time.Parse(time.RFC3339, request.ExpiresAt)
	if err != nil || expires.Format(time.RFC3339) != request.ExpiresAt || expires.Location() != time.UTC ||
		!identifier.MatchString(request.RequestID) || !utf8.ValidString(request.Task) ||
		strings.TrimSpace(request.Task) == "" || strings.ContainsRune(request.Task, '\x00') || utf8.RuneCountInString(request.Task) > 16384 {
		return View{}, ErrInvalid
	}
	name := "task-" + digest(s.config.Principal + "\x00" + request.RequestID)[:40]
	wanted := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.config.Namespace,
		Annotations: map[string]string{ownerAnnotation: s.config.Principal, workflowAnnotation: request.Workflow, expiryAnnotation: request.ExpiresAt}},
		Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: workflow.Template}, Task: request.Task, Capabilities: slices.Clone(workflow.Capabilities)}}
	var existing api.AgentRun
	err = s.kube.Get(ctx, client.ObjectKeyFromObject(wanted), &existing)
	if err == nil {
		return s.replay(&existing, wanted)
	}
	if !apierrors.IsNotFound(err) {
		return View{}, ErrUnavailable
	}
	if !expires.After(s.now()) {
		return View{}, ErrExpired
	}
	if expires.After(s.now().Add(requestWindow)) {
		return View{}, ErrInvalid
	}
	if err := s.validateRetention(ctx, workflow); err != nil {
		return View{}, err
	}
	if !expires.After(s.now()) {
		return View{}, ErrExpired
	}
	if err = s.kube.Create(ctx, wanted); err == nil {
		return view(wanted), nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return View{}, ErrUnavailable
	}
	if err = s.kube.Get(ctx, client.ObjectKeyFromObject(wanted), &existing); err != nil {
		return View{}, ErrUnavailable
	}
	return s.replay(&existing, wanted)
}

func (s *Service) replay(existing, wanted *api.AgentRun) (View, error) {
	actual := existing.Spec
	actual.Cancel = false // Cancellation does not change submission identity.
	if !existing.DeletionTimestamp.IsZero() || existing.Annotations[ownerAnnotation] != s.config.Principal ||
		existing.Annotations[workflowAnnotation] != wanted.Annotations[workflowAnnotation] ||
		existing.Annotations[expiryAnnotation] != wanted.Annotations[expiryAnnotation] ||
		!reflect.DeepEqual(actual, wanted.Spec) {
		return View{}, ErrConflict
	}
	return view(existing), nil
}

func (s *Service) validateRetention(ctx context.Context, workflow Workflow) error {
	var template api.AgentTemplate
	if err := s.kube.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: workflow.Template}, &template); err != nil {
		return ErrUnavailable
	}
	var policy api.AgentPolicy
	if err := s.kube.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: template.Spec.PolicyRef.Name}, &policy); err != nil {
		return ErrUnavailable
	}
	if policy.Spec.RetentionTTL.Duration <= requestWindow+retentionMargin {
		return ErrDenied
	}
	return nil
}

func (s *Service) owned(ctx context.Context, reference Reference) (*api.AgentRun, error) {
	if !strings.HasPrefix(reference.ID, "task-") || !identifier.MatchString(reference.ID) || reference.UID == "" || len(reference.UID) > 128 {
		return nil, ErrInvalid
	}
	var run api.AgentRun
	if err := s.kube.Get(ctx, client.ObjectKey{Namespace: s.config.Namespace, Name: reference.ID}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrGone
		}
		return nil, ErrUnavailable
	}
	if run.Annotations[ownerAnnotation] != s.config.Principal {
		return nil, ErrDenied
	}
	if string(run.UID) != reference.UID || !run.DeletionTimestamp.IsZero() {
		return nil, ErrGone
	}
	return &run, nil
}

func (s *Service) Status(ctx context.Context, reference Reference) (View, error) {
	run, err := s.owned(ctx, reference)
	if err != nil {
		return View{}, err
	}
	return view(run), nil
}

// Cancel uses UID/resource-version tests so a replacement Run cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, reference Reference) (View, error) {
	run, err := s.owned(ctx, reference)
	if err != nil {
		return View{}, err
	}
	if run.Spec.Cancel || terminal(run.Status.Phase) {
		return view(run), nil
	}
	patch, err := cancelPatch(run)
	if err != nil {
		return View{}, ErrUnavailable
	}
	if err = s.kube.Patch(ctx, run, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		return View{}, ErrUnavailable
	}
	return view(run), nil
}

// Wait returns the latest observation within a bounded interval. It never cancels
// a worker merely because the caller stops waiting.
func (s *Service) Wait(ctx context.Context, reference Reference, timeout time.Duration) (View, error) {
	if timeout <= 0 || timeout > 30*time.Second {
		return View{}, ErrInvalid
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var latest View
	for {
		observation, err := s.Status(waitContext, reference)
		if err != nil {
			if ctx.Err() != nil {
				return View{}, ctx.Err()
			}
			if waitContext.Err() != nil && latest.ID != "" {
				return latest, nil
			}
			return View{}, err
		}
		latest = observation
		if terminal(observation.Phase) {
			return observation, nil
		}
		select {
		case <-waitContext.Done():
			if ctx.Err() != nil {
				return View{}, ctx.Err()
			}
			return observation, nil
		case <-ticker.C:
		}
	}
}

func view(run *api.AgentRun) View {
	phase := run.Status.Phase
	if phase == "" {
		phase = api.AgentRunPhaseQueued
	}
	return View{Reference: Reference{ID: run.Name, UID: string(run.UID)}, Phase: phase, CancellationRequested: run.Spec.Cancel,
		StartedAt: timestamp(run.Status.StartedAt), CompletedAt: timestamp(run.Status.CompletedAt), Result: run.Status.Result}
}

func timestamp(value *metav1.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func terminal(phase api.AgentRunPhase) bool {
	return slices.Contains([]api.AgentRunPhase{api.AgentRunPhaseSucceeded, api.AgentRunPhaseFailed, api.AgentRunPhaseTimedOut, api.AgentRunPhaseCancelled}, phase)
}

func digest(text string) string {
	value := sha256.Sum256([]byte(text))
	return hex.EncodeToString(value[:])
}

func cancelPatch(run *api.AgentRun) ([]byte, error) {
	return json.Marshal([]map[string]any{
		{"op": "test", patchPath: "/metadata/uid", patchValue: string(run.UID)},
		{"op": "test", patchPath: "/metadata/resourceVersion", patchValue: run.ResourceVersion},
		{"op": "add", patchPath: "/spec/cancel", patchValue: true},
	})
}

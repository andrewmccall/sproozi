// Package session retains trusted remote execution registration and fencing.
// Only the remote executor may write this store. Native workers, checkpoints
// and Run annotations do not supply authority. Callers confirm restoration,
// native-container termination and ledger seals before recording those effects.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/agentcontract"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const enabledValue = "true"

const (
	dataKey        = "registration.json"
	maxRecordBytes = 64 << 10
	registryLabel  = "sessions.sproozi.com/registry"
)

var (
	ErrConflict = errors.New("mobility registration conflict")
	ErrGone     = errors.New("mobility registration incarnation unavailable")
	ErrExpired  = errors.New("mobility execution deadline expired")
	// ErrSessionStopped reports a run whose session stop completed: its
	// checkpoint and finalized usage are recorded. ResolveRun returns the
	// record with it. The gateway denies the run; the controller waits for
	// the sandbox to finish and ends the run as stopped, not failed.
	ErrSessionStopped = errors.New("mobility session stopped this execution")
	// ErrSessionCancelled reports a run whose own registration was cancelled:
	// an abort cancels the registration before it patches spec.cancel on the
	// run. It wraps ErrConflict, so every caller that refuses a conflict still
	// refuses it; the controller ends the run Cancelled rather than Failed.
	ErrSessionCancelled = fmt.Errorf("%w: session cancelled this execution", ErrConflict)
	identifier          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	digestPattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	uuidPattern         = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
)

type Phase string

const (
	Creating  Phase = "creating"
	Bound     Phase = "bound"
	Prepared  Phase = "prepared"
	Active    Phase = "active"
	Stopping  Phase = "stopping"
	Stopped   Phase = "stopped"
	Cancelled Phase = "cancelled"
)

// Reference identifies the registry incarnation as well as the session epoch.
// A replacement ConfigMap with the same name is a different execution record.
type Reference struct {
	Name  string `json:"name"`
	UID   string `json:"uid"`
	Epoch uint64 `json:"epoch"`
}

// Registration is server-selected immutable intent. Zero cost means unbounded
// monetary accounting; units must have a positive finite ceiling. Exhausted
// units must be denied, never represented by the accounting API's unbounded zero.
type Registration struct {
	Harness           harness.NativeKind                        `json:"harness"`
	Principal         string                                    `json:"principal"`
	SessionID         string                                    `json:"sessionId"`
	Epoch             uint64                                    `json:"epoch"`
	OperationID       string                                    `json:"operationId"`
	NativeUUID        string                                    `json:"nativeUuid"`
	InstructionDigest string                                    `json:"instructionDigest"`
	OriginalDeadline  time.Time                                 `json:"originalDeadline"`
	TemplateName      string                                    `json:"templateName"`
	Ceilings          map[api.CapabilityKind]api.EndpointBudget `json:"ceilings"`
	WorkspaceDigest   string                                    `json:"workspaceDigest,omitempty"`
	Task              string                                    `json:"task,omitempty"`
	CheckpointDigest  string                                    `json:"checkpointDigest"`
	IncludeIgnored    []string                                  `json:"includeIgnored,omitempty"`
	IntendedRunName   string                                    `json:"intendedRunName"`
	// ProfileDigest names the stored profile archive copied into the run's
	// agent contract.
	ProfileDigest string `json:"profileDigest,omitempty"`
	// Login is the named login a harness that signs in itself uses
	// (ADR 0049); empty is harness.DefaultLogin.
	Login string `json:"login,omitempty"`
}

// StopReceipt contains only trusted ledger seals and checkpoint identity.
// It is retained once; later retries cannot amend or free additional allowance.
type StopReceipt struct {
	CheckpointDigest string                                     `json:"checkpointDigest"`
	Seals            map[api.CapabilityKind]budget.TrustedUsage `json:"seals"`
}

type Record struct {
	Reference Reference `json:"reference"`
	Registration
	RunUID  string       `json:"runUid"`
	PodUID  string       `json:"podUid"`
	Phase   Phase        `json:"phase"`
	Receipt *StopReceipt `json:"receipt,omitempty"`
}

// stored is the storage boundary; fields are checked together before conversion
// into a Record. Object UID comes from Kubernetes metadata, not stored JSON.
type stored struct {
	Version      int                `json:"version"`
	Registration storedRegistration `json:"registration"`
	RunUID       string             `json:"runUid"`
	PodUID       string             `json:"podUid"`
	Phase        Phase              `json:"phase"`
	Receipt      *StopReceipt       `json:"receipt,omitempty"`
}

// storedRegistration accepts the retired nativePin field only when reading an
// existing journal. It grants no authority and is omitted from all new writes.
type storedRegistration struct {
	Registration
	RetiredNativePin json.RawMessage `json:"nativePin,omitempty"`
}

type Store struct {
	kube      client.Client
	namespace string
	now       func() time.Time
}

func NewStore(kube client.Client, namespace string) (*Store, error) {
	if kube == nil || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, fmt.Errorf("invalid mobility registry configuration")
	}
	return &Store{kube: kube, namespace: namespace, now: time.Now}, nil
}

// NameForRun gives controller and gateway one bounded direct lookup. Namespace
// is fixed by Store; matching a name alone never establishes ownership.
func NameForRun(runName string) string {
	sum := sha256.Sum256([]byte(runName))
	return "sproozi-session-" + hex.EncodeToString(sum[:])[:40]
}

func (s *Store) Create(ctx context.Context, registration Registration) (Record, error) {
	registration.OriginalDeadline = registration.OriginalDeadline.UTC()
	registration.IncludeIgnored = append([]string(nil), registration.IncludeIgnored...)
	if err := validateRegistration(registration); err != nil {
		return Record{}, err
	}
	name := NameForRun(registration.IntendedRunName)
	var existing corev1.ConfigMap
	err := s.kube.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, &existing)
	if err == nil {
		return replay(&existing, registration)
	}
	if !apierrors.IsNotFound(err) {
		return Record{}, err
	}
	if !registration.OriginalDeadline.After(s.now()) {
		return Record{}, ErrExpired
	}
	document, err := encode(stored{Version: 1, Registration: storedRegistration{Registration: registration}, Phase: Creating})
	if err != nil {
		return Record{}, err
	}
	wanted := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace,
		Labels: map[string]string{registryLabel: enabledValue}}, Data: map[string]string{dataKey: document}}
	// No Run owner reference: receipts and replay identity survive Pod/Run cleanup.
	if err = s.kube.Create(ctx, wanted); err == nil {
		return read(wanted)
	}
	if !apierrors.IsAlreadyExists(err) {
		return Record{}, err
	}
	if err = s.kube.Get(ctx, client.ObjectKeyFromObject(wanted), &existing); err != nil {
		return Record{}, err
	}
	return replay(&existing, registration)
}

func replay(cm *corev1.ConfigMap, registration Registration) (Record, error) {
	record, err := read(cm)
	if err != nil {
		return Record{}, err
	}
	actual := record.Registration
	// Initial launch replay retains the original empty native UUID intent. The
	// genuine UUID is bound once after native observation, never caller-selected.
	if actual.WorkspaceDigest != "" && registration.NativeUUID == "" {
		actual.NativeUUID = ""
	}
	if !reflect.DeepEqual(actual, registration) {
		return Record{}, ErrConflict
	}
	return record, nil
}

func (s *Store) Get(ctx context.Context, ref Reference) (Record, error) {
	_, record, err := s.load(ctx, ref)
	return record, err
}

func (s *Store) BindRun(ctx context.Context, ref Reference, runUID string) (Record, error) {
	if !validUID(runUID) {
		return Record{}, fmt.Errorf("invalid run UID")
	}
	return s.change(ctx, ref, func(r *stored) error {
		if r.RunUID == runUID {
			return nil
		}
		if r.Phase != Creating || r.RunUID != "" {
			return ErrConflict
		}
		r.RunUID, r.Phase = runUID, Bound
		return nil
	})
}

// BindPod records an observed exact Pod before restore. Bound remains inert:
// observation does not imply a successful checkpoint or enable gateway access.
func (s *Store) BindPod(ctx context.Context, ref Reference, podUID string) (Record, error) {
	if !validUID(podUID) {
		return Record{}, fmt.Errorf("invalid pod UID")
	}
	return s.change(ctx, ref, func(r *stored) error {
		if r.PodUID == podUID {
			return nil
		}
		if r.Phase != Bound || r.PodUID != "" {
			return ErrConflict
		}
		r.PodUID = podUID
		return nil
	})
}

// Prepared is called only after the trusted service observed successful restore
// and verified the registered checkpoint in this exact Pod incarnation.
func (s *Store) Prepared(ctx context.Context, ref Reference, podUID string) (Record, error) {
	if !validUID(podUID) {
		return Record{}, fmt.Errorf("invalid pod UID")
	}
	return s.change(ctx, ref, func(r *stored) error {
		if r.PodUID == podUID && r.Phase != Bound {
			return nil
		}
		if r.Phase != Bound || r.PodUID != "" && r.PodUID != podUID {
			return ErrConflict
		}
		r.PodUID, r.Phase = podUID, Prepared
		return nil
	})
}

func (s *Store) Activate(ctx context.Context, ref Reference) (Record, error) {
	return s.change(ctx, ref, func(r *stored) error {
		if !r.Registration.OriginalDeadline.After(s.now()) {
			return ErrExpired
		}
		if r.Phase == Active {
			return nil
		}
		if r.Phase != Prepared {
			return ErrConflict
		}
		r.Phase = Active
		return nil
	})
}

// BindNative binds the actual first native UUID once for an initial workspace.
// It neither enables authority nor creates an execution; the trusted placement
// must observe it on the exact admitted native container before calling this.
func (s *Store) BindNative(ctx context.Context, ref Reference, nativeUUID string) (Record, error) {
	if !uuidPattern.MatchString(nativeUUID) {
		return Record{}, ErrConflict
	}
	return s.change(ctx, ref, func(r *stored) error {
		if r.Registration.WorkspaceDigest == "" {
			return ErrConflict
		}
		if r.Registration.NativeUUID == nativeUUID {
			return nil
		}
		if r.Registration.NativeUUID != "" || r.Phase != Active {
			return ErrConflict
		}
		r.Registration.NativeUUID = nativeUUID
		return nil
	})
}

func (s *Store) Stop(ctx context.Context, ref Reference) (Record, error) {
	return s.change(ctx, ref, func(r *stored) error {
		if r.Phase == Stopping || r.Phase == Stopped {
			return nil
		}
		if r.Phase != Active {
			return ErrConflict
		}
		r.Phase = Stopping
		return nil
	})
}

func (s *Store) Cancel(ctx context.Context, ref Reference) (Record, error) {
	return s.change(ctx, ref, func(r *stored) error {
		if r.Phase == Cancelled || r.Phase == Stopped {
			return nil
		}
		r.Phase = Cancelled
		return nil
	})
}

// CompleteStop may be called only by the trusted service after confirming native
// container termination and obtaining each seal from the trusted ledger.
func (s *Store) CompleteStop(ctx context.Context, ref Reference, checkpointDigest string, seals map[api.CapabilityKind]budget.TrustedUsage) (Record, error) {
	return s.change(ctx, ref, func(r *stored) error {
		receipt := &StopReceipt{CheckpointDigest: checkpointDigest, Seals: seals}
		if err := validateReceipt(receipt, r.Registration.Ceilings); err != nil {
			return err
		}
		if r.Phase == Stopped {
			if !reflect.DeepEqual(r.Receipt, receipt) {
				return ErrConflict
			}
			return nil
		}
		if r.Phase != Stopping {
			return ErrConflict
		}
		r.Receipt, r.Phase = receipt, Stopped
		return nil
	})
}

// CompleteCancel retains trusted seals after the placement revoked access and
// physically fenced the exact bound Pod. Cancellation requires no new checkpoint.
func (s *Store) CompleteCancel(ctx context.Context, ref Reference, seals map[api.CapabilityKind]budget.TrustedUsage) (Record, error) {
	return s.change(ctx, ref, func(r *stored) error {
		if r.Phase != Cancelled || r.RunUID == "" || r.PodUID == "" {
			return ErrConflict
		}
		receipt := &StopReceipt{Seals: seals}
		if err := validateCancellationReceipt(receipt, r.Registration.Ceilings); err != nil {
			return err
		}
		if r.Receipt != nil {
			if !reflect.DeepEqual(r.Receipt, receipt) {
				return ErrConflict
			}
			return nil
		}
		r.Receipt = receipt
		return nil
	})
}

func (s *Store) load(ctx context.Context, ref Reference) (*corev1.ConfigMap, Record, error) {
	if len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || !validUID(ref.UID) || ref.Epoch == 0 {
		return nil, Record{}, fmt.Errorf("invalid mobility reference")
	}
	var cm corev1.ConfigMap
	if err := s.kube.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: ref.Name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, Record{}, ErrGone
		}
		return nil, Record{}, err
	}
	if string(cm.UID) != ref.UID {
		return nil, Record{}, ErrGone
	}
	record, err := read(&cm)
	if err != nil {
		return nil, Record{}, err
	}
	if record.Epoch != ref.Epoch {
		return nil, Record{}, ErrConflict
	}
	return &cm, record, nil
}

func (s *Store) change(ctx context.Context, ref Reference, mutate func(*stored) error) (Record, error) {
	for range 8 {
		cm, record, err := s.load(ctx, ref)
		if err != nil {
			return Record{}, err
		}
		state := stored{Version: 1, Registration: storedRegistration{Registration: record.Registration}, RunUID: record.RunUID, PodUID: record.PodUID, Phase: record.Phase, Receipt: record.Receipt}
		if err = mutate(&state); err != nil {
			return Record{}, err
		}
		document, err := encode(state)
		if err != nil {
			return Record{}, err
		}
		if cm.Data[dataKey] == document {
			return record, nil
		}
		cm.Data[dataKey] = document
		if err = s.kube.Update(ctx, cm); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return Record{}, err
		}
		return read(cm)
	}
	return Record{}, fmt.Errorf("mobility registry update conflicted after retries")
}

func encode(value stored) (string, error) {
	if err := validateStored(value); err != nil {
		return "", err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > maxRecordBytes {
		return "", fmt.Errorf("mobility record exceeds limit")
	}
	return string(data), nil
}

func read(cm *corev1.ConfigMap) (Record, error) {
	if !validUID(string(cm.UID)) || cm.Labels[registryLabel] != enabledValue || !cm.DeletionTimestamp.IsZero() || len(cm.OwnerReferences) != 0 || len(cm.BinaryData) != 0 || len(cm.Data) != 1 || len(cm.Data[dataKey]) == 0 || len(cm.Data[dataKey]) > maxRecordBytes {
		return Record{}, fmt.Errorf("invalid mobility registry object")
	}
	var value stored
	decoder := json.NewDecoder(bytes.NewBufferString(cm.Data[dataKey]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Record{}, fmt.Errorf("invalid mobility record: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Record{}, fmt.Errorf("mobility record has trailing data")
	}
	if err := validateStored(value); err != nil {
		return Record{}, err
	}
	if cm.Name != NameForRun(value.Registration.IntendedRunName) {
		return Record{}, fmt.Errorf("mobility record name mismatch")
	}
	return Record{Reference: Reference{Name: cm.Name, UID: string(cm.UID), Epoch: value.Registration.Epoch}, Registration: value.Registration.Registration,
		RunUID: value.RunUID, PodUID: value.PodUID, Phase: value.Phase, Receipt: value.Receipt}, nil
}

func validUID(uid string) bool { return identifier.MatchString(uid) }

func validateRegistration(r Registration) error {
	if err := r.Harness.Validate(); err != nil {
		return err
	}
	if err := workspace.ValidateIgnoredPaths(r.IncludeIgnored); err != nil {
		return err
	}
	if !identifier.MatchString(r.Principal) || !identifier.MatchString(r.SessionID) || !identifier.MatchString(r.OperationID) || r.Epoch == 0 ||
		!digestPattern.MatchString(r.InstructionDigest) ||
		r.OriginalDeadline.IsZero() || r.OriginalDeadline.Location() != time.UTC || len(validation.IsDNS1123Subdomain(r.TemplateName)) != 0 ||
		len(validation.IsDNS1123Subdomain(r.IntendedRunName)) != 0 || len(r.Ceilings) == 0 || len(r.Ceilings) > 32 {
		return fmt.Errorf("invalid mobility registration")
	}
	if err := validateNativeInput(r); err != nil {
		return err
	}
	if r.ProfileDigest != "" && !agentprofile.ValidDigest(r.ProfileDigest) {
		return fmt.Errorf("invalid mobility profile digest")
	}
	for capability, ceiling := range r.Ceilings {
		_, named := capability.MCPServerName()
		if !named && capability != api.CapabilityModelInference && capability != api.CapabilityKubernetesRead && capability != api.CapabilityGitHubPullRequest && capability != api.CapabilityNetworkEgress && capability != api.CapabilityPackagesInstall {
			return fmt.Errorf("invalid mobility capability")
		}
		if ceiling.MaxUnits <= 0 || ceiling.MaxCostMicros < 0 {
			return fmt.Errorf("invalid mobility allowance")
		}
	}
	return nil
}

func validateStored(r stored) error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported mobility record version")
	}
	if err := validateRegistration(r.Registration.Registration); err != nil {
		return err
	}
	if r.RunUID != "" && !validUID(r.RunUID) || r.PodUID != "" && (!validUID(r.PodUID) || r.RunUID == "") {
		return fmt.Errorf("invalid mobility execution identity")
	}
	switch r.Phase {
	case Creating:
		if r.RunUID != "" || r.PodUID != "" {
			return fmt.Errorf("creating record is already bound")
		}
	case Bound:
		if r.RunUID == "" {
			return fmt.Errorf("invalid bound record")
		}
	case Prepared, Active, Stopping, Stopped:
		if r.RunUID == "" || r.PodUID == "" {
			return fmt.Errorf("execution identity unavailable")
		}
	case Cancelled:
	default:
		return fmt.Errorf("invalid mobility phase")
	}
	if r.Phase == Stopped {
		if !uuidPattern.MatchString(r.Registration.NativeUUID) {
			return fmt.Errorf("stopped execution has no native identity")
		}
		return validateReceipt(r.Receipt, r.Registration.Ceilings)
	}
	if r.Phase == Cancelled && r.Receipt != nil {
		if r.RunUID == "" || r.PodUID == "" {
			return fmt.Errorf("cancellation receipt lacks exact execution identity")
		}
		return validateCancellationReceipt(r.Receipt, r.Registration.Ceilings)
	}
	if r.Receipt != nil {
		return fmt.Errorf("receipt requires stopped execution")
	}
	return nil
}

func validateReceipt(r *StopReceipt, ceilings map[api.CapabilityKind]api.EndpointBudget) error {
	if r == nil || !digestPattern.MatchString(r.CheckpointDigest) {
		return fmt.Errorf("incomplete mobility stop receipt")
	}
	return validateSeals(r.Seals, ceilings)
}

func validateCancellationReceipt(r *StopReceipt, ceilings map[api.CapabilityKind]api.EndpointBudget) error {
	if r == nil || r.CheckpointDigest != "" {
		return fmt.Errorf("invalid mobility cancellation receipt")
	}
	return validateSeals(r.Seals, ceilings)
}

func validateSeals(seals map[api.CapabilityKind]budget.TrustedUsage, ceilings map[api.CapabilityKind]api.EndpointBudget) error {
	// Registry ceilings always bound units; zero cost means unbounded spend.
	allowances := make(map[string]session.Allowance, len(ceilings))
	usage := make(map[string]budget.TrustedUsage, len(seals))
	for capability, ceiling := range ceilings {
		allowances[string(capability)] = session.Allowance{Units: session.Limit{Bounded: true, Value: ceiling.MaxUnits},
			CostMicros: session.Limit{Bounded: ceiling.MaxCostMicros > 0, Value: ceiling.MaxCostMicros}}
	}
	for capability, seal := range seals {
		usage[string(capability)] = seal
	}
	if err := session.CheckUsage(allowances, usage); err != nil {
		return fmt.Errorf("invalid mobility accounting seal: %w", err)
	}
	return nil
}

func validateNativeInput(r Registration) error {
	if r.Login != "" {
		if err := harness.ValidateLogin(r.Login); err != nil {
			return err
		}
	}
	if utf8.RuneCountInString(r.Task) > agentcontract.MaxTaskRunes || strings.ContainsRune(r.Task, 0) || r.Task != "" && (strings.TrimSpace(r.Task) == "" || r.Harness != harness.NativeCodex) {
		return fmt.Errorf("invalid native task")
	}
	if r.WorkspaceDigest != "" {
		if !digestPattern.MatchString(r.WorkspaceDigest) || r.CheckpointDigest != "" || r.NativeUUID != "" && !uuidPattern.MatchString(r.NativeUUID) {
			return fmt.Errorf("invalid initial workspace registration")
		}
	} else if !uuidPattern.MatchString(r.NativeUUID) || !digestPattern.MatchString(r.CheckpointDigest) {
		return fmt.Errorf("invalid checkpoint registration")
	}
	return nil
}

// Package placement owns one administrator-configured Kubernetes session profile.
package placement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	budgetkube "github.com/andrewmccall/sproozi/internal/budget/kubernetes"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	policyengine "github.com/andrewmccall/sproozi/internal/policy/kubernetes"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	registry "github.com/andrewmccall/sproozi/internal/runtime/kubernetes/session"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const fixedTask = "Continue the admitted native session"
const nativeContainer = "agent"
const helperPath = "/usr/local/bin/checkpoint"
const nativePath = "/usr/local/bin/native-session"
const nativeStatus = "status"
const nativeActivate = "activate"
const nativeActive = "active"
const nativePrepared = "prepared"
const nativeStopped = "stopped"
const nativeGate = "gate"
const nativeQuiesce = "quiesce"
const helperCapture = "capture-stopped"
const helperFinish = "finish"
const helperPrepare = "prepare"
const helperPrepareWorkspace = "prepare-workspace"

// Config is administrator-owned; requests cannot override templates, namespace,
// credentials, commands or limits. Directory must be private durable storage.
type Config struct {
	Harness                                                             harness.NativeKind
	Kube                                                                client.Client
	RESTConfig                                                          *rest.Config
	Namespace, AgentNamespace, TemplateName, Principal, Directory, Name string
	InstructionDigest                                                   string
	Allowances                                                          map[string]session.Allowance
	MaxDuration                                                         time.Duration
	MaxCheckpointBytes                                                  int64
}

type Placement struct {
	config   Config
	registry *registry.Store
	ledger   budget.Ledger
	exec     commandExecutor
	mu       sync.RWMutex
	journals session.JournalLocks
	lock     *os.File
}

// Open holds one filesystem owner lock for the configured profile. Kubernetes
// CAS records independently retain exact remote identity and immutable receipts.
func Open(config Config) (*Placement, error) {
	if config.AgentNamespace == "" {
		config.AgentNamespace = runtimekube.AgentsNamespace
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	config.Allowances = maps.Clone(config.Allowances)
	if err := os.MkdirAll(config.Directory, 0700); err != nil {
		return nil, err
	}
	if err := privatePath(config.Directory, true); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(config.Directory, "owner.lock"), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = privatePath(lock.Name(), false); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("placement: profile already owned")
	}
	store, err := registry.NewStore(config.Kube, config.Namespace)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	executor, err := newExecutor(config.RESTConfig)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Placement{config: config, registry: store, ledger: budgetkube.NewLedger(config.Kube, config.Namespace), exec: executor, lock: lock}, nil
}
func validateConfig(config Config) error {
	if config.Harness.Validate() != nil || config.Kube == nil || config.RESTConfig == nil || len(validation.IsDNS1123Label(config.Namespace)) != 0 || config.AgentNamespace != runtimekube.AgentsNamespace || len(validation.IsDNS1123Subdomain(config.TemplateName)) != 0 || config.Principal == "" || config.Name == "" || !digestValid(config.InstructionDigest) || config.MaxDuration <= 0 || config.MaxCheckpointBytes <= 0 || config.MaxCheckpointBytes > 256<<20 || !filepath.IsAbs(config.Directory) || len(config.Allowances) == 0 || len(config.Allowances) > 32 {
		return errors.New("placement: invalid fixed configuration")
	}
	for _, allowance := range config.Allowances {
		if !allowance.Units.Bounded || allowance.Units.Value <= 0 || allowance.CostMicros.Value < 0 || !allowance.CostMicros.Bounded && allowance.CostMicros.Value != 0 || allowance.CostMicros.Bounded && allowance.CostMicros.Value == 0 {
			return errors.New("placement: finite positive units and explicit cost limit required")
		}
	}
	return nil
}

func (p *Placement) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lock == nil {
		return nil
	}
	err := p.lock.Close()
	p.lock = nil
	return err
}

func (p *Placement) lockOperation(l session.Launch) (func(), error) {
	if l.Harness != p.config.Harness || !idValid(l.SessionID) || !idValid(l.AllocationID) {
		return nil, &profile.Rejection{Code: profile.CodeInvalid}
	}
	p.mu.RLock()
	// The epoch replay record is shared by allocations in one logical session;
	// allocation files must also remain exclusive under conflicting session IDs.
	unlockSession := p.journals.Lock("session/" + l.SessionID)
	unlockAllocation := p.journals.Lock("allocation/" + l.AllocationID)
	unlock := func() { unlockAllocation(); unlockSession(); p.mu.RUnlock() }
	if p.lock == nil {
		unlock()
		return nil, errors.New("placement: closed")
	}
	return unlock, nil
}
func digestValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func idValid(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}
func runName(l session.Launch) string {
	sum := sha256.Sum256([]byte(l.AllocationID))
	return "session-" + hex.EncodeToString(sum[:])[:40]
}
func inc(r registry.Record) session.Incarnation {
	return session.Incarnation{ExecutionID: r.RunUID, ResourceUID: r.PodUID}
}
func account(r registry.Record, name api.CapabilityKind) budget.AccountKey {
	return budget.AccountKey(r.RunUID + "/" + string(name))
}
func (p *Placement) liveProfile(ctx context.Context, l session.Launch) (map[api.CapabilityKind]api.EndpointBudget, error) {
	return p.liveProfileInput(ctx, l, false)
}

func (p *Placement) liveProfileInput(ctx context.Context, l session.Launch, initial bool) (map[api.CapabilityKind]api.EndpointBudget, error) {
	if err := p.validateInput(l, initial); err != nil {
		return nil, err
	}
	if !time.Now().Before(l.Deadline) {
		return nil, &profile.Rejection{Code: profile.CodeExpired}
	}
	var template api.AgentTemplate
	var policy api.AgentPolicy
	var runtime api.AgentRuntime
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: p.config.TemplateName}, &template); err != nil {
		return nil, err
	}
	if template.Annotations[registry.ManagedTemplateAnnotation] != "true" || registry.InstructionsDigest(template.Spec.Instructions) != l.InstructionDigest {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: template.Spec.PolicyRef.Name}, &policy); err != nil {
		return nil, err
	}
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: template.Spec.RuntimeRef.Name}, &runtime); err != nil {
		return nil, err
	}
	if runtime.Spec.ClientConfig.Harness != string(p.config.Harness) || runtime.Spec.ValidateRuntimeTemplate() != nil {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	capabilities := make([]api.CapabilityKind, 0, len(l.Allowances))
	for name := range l.Allowances {
		capabilities = append(capabilities, api.CapabilityKind(name))
	}
	if policyengine.Evaluate(policy.Spec, template.Spec, api.AgentRunSpec{Capabilities: capabilities}, runtime.Spec) != nil {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	duration := min(p.config.MaxDuration, policy.Spec.MaxExecutionDuration.Duration)
	if duration <= 0 || l.Deadline.After(time.Now().Add(duration)) {
		return nil, &profile.Rejection{Code: profile.CodeDenied}
	}
	return p.ceilings(l, policy.Spec)
}
func limitFits(want, cap session.Limit) bool {
	return !cap.Bounded || want.Bounded && want.Value <= cap.Value
}
func (p *Placement) ceilings(l session.Launch, policy api.AgentPolicySpec) (map[api.CapabilityKind]api.EndpointBudget, error) {
	ceilings := make(map[api.CapabilityKind]api.EndpointBudget, len(l.Allowances))
	for name, a := range l.Allowances {
		fixed, ok := p.config.Allowances[name]
		capability := api.CapabilityKind(name)
		if !ok || !slices.Contains(policy.AllowedCapabilities, capability) || !a.Units.Bounded || a.Units.Value <= 0 || a.CostMicros.Value < 0 || !a.CostMicros.Bounded && a.CostMicros.Value != 0 || a.CostMicros.Bounded && a.CostMicros.Value == 0 || !limitFits(a.Units, fixed.Units) || !limitFits(a.CostMicros, fixed.CostMicros) {
			return nil, &profile.Rejection{Code: profile.CodeDenied}
		}
		live := policy.Budgets[capability]
		if live.MaxUnits > 0 && a.Units.Value > live.MaxUnits || live.MaxCostMicros > 0 && (!a.CostMicros.Bounded || a.CostMicros.Value > live.MaxCostMicros) {
			return nil, &profile.Rejection{Code: profile.CodeDenied}
		}
		ceilings[capability] = api.EndpointBudget{MaxUnits: a.Units.Value, MaxCostMicros: a.CostMicros.Value}
	}
	return ceilings, nil
}
func (p *Placement) registration(l session.Launch, ceilings map[api.CapabilityKind]api.EndpointBudget) registry.Registration {
	return registry.Registration{Harness: p.config.Harness, Principal: p.config.Principal, SessionID: l.SessionID, Epoch: l.Epoch, OperationID: l.OperationID, NativeUUID: l.ThreadID, InstructionDigest: l.InstructionDigest, OriginalDeadline: l.Deadline.UTC(), TemplateName: p.config.TemplateName, Ceilings: ceilings, Task: l.Task, CheckpointDigest: l.CheckpointDigest, IncludeIgnored: append([]string(nil), l.IncludeIgnored...), IntendedRunName: runName(l)}
}

func (p *Placement) Prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Incarnation{}, err
	}
	defer unlock()
	ceilings, err := p.liveProfile(ctx, l)
	if err != nil {
		return session.Incarnation{}, err
	}
	meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
	if err != nil || meta.Harness != l.Harness || meta.ThreadID != l.ThreadID || meta.SHA256 != l.CheckpointDigest {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if err = checkpoint.ValidateContinuity(data, l.Harness, l.InstructionDigest, p.config.MaxCheckpointBytes); err != nil {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	local, err := p.persistIntent(l, data)
	if err != nil {
		return session.Incarnation{}, err
	}
	record, err := p.registry.Create(ctx, p.registration(l, ceilings))
	if err != nil {
		return session.Incarnation{}, semantic(err)
	}
	local.Reference = record.Reference
	if err = p.save(local); err != nil {
		return session.Incarnation{}, err
	}
	return p.prepare(ctx, local, record)
}

// CreateInitial restores a stopped workspace into a fresh native home. Only this
// explicit entry permits an empty native UUID/checkpoint; the client supplies
// the genuine UUID and the trusted placement binds it after observation.
func (p *Placement) CreateInitial(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Incarnation{}, err
	}
	defer unlock()
	if l.ThreadID != "" || l.CheckpointDigest != "" {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if len(data) == 0 || int64(len(data)) > p.config.MaxCheckpointBytes {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	// A lost prepare acknowledgement observes the original exact incarnation,
	// even after its bounded execution stopped or expired. This grants no activation.
	existing, loadErr := p.load(l)
	if loadErr == nil && existing.Initial != nil {
		if existing.WorkspaceDigest != digest {
			return session.Incarnation{}, &profile.Rejection{Code: profile.CodeConflict}
		}
		if existing.Reference.UID != "" {
			_, r, err := p.retained(ctx, l)
			if err != nil {
				return session.Incarnation{}, semantic(err)
			}
			if r.Phase == registry.Prepared || r.Phase == registry.Active || r.Phase == registry.Stopped {
				return inc(r), nil
			}
		}
	} else if loadErr != nil && !os.IsNotExist(loadErr) {
		return session.Incarnation{}, loadErr
	}
	ceilings, err := p.liveProfileInput(ctx, l, true)
	if err != nil {
		return session.Incarnation{}, err
	}
	local, err := p.persistIntent(l, data)
	if err != nil {
		return session.Incarnation{}, err
	}
	retainedInput, err := p.readArtifact(l.AllocationID, "input")
	if err != nil {
		return session.Incarnation{}, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(retainedInput)) != digest {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if local.Initial == nil {
		if local.Reference.UID != "" || local.OutputDigest != "" {
			return session.Incarnation{}, &profile.Rejection{Code: profile.CodeConflict}
		}
		original := l
		local.Initial, local.WorkspaceDigest = &original, digest
		if err = p.save(local); err != nil {
			return session.Incarnation{}, err
		}
	} else if local.WorkspaceDigest != digest {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	registration := p.registration(l, ceilings)
	registration.WorkspaceDigest = digest
	r, err := p.registry.Create(ctx, registration)
	if err != nil {
		return session.Incarnation{}, semantic(err)
	}
	local.Reference = r.Reference
	if err = p.save(local); err != nil {
		return session.Incarnation{}, err
	}
	return p.prepare(ctx, local, r)
}

func (p *Placement) prepare(ctx context.Context, local retained, r registry.Record) (session.Incarnation, error) {
	if r.Phase == registry.Prepared || r.Phase == registry.Active {
		return inc(r), nil
	}
	if r.Phase != registry.Creating && r.Phase != registry.Bound {
		return session.Incarnation{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	run, err := p.ensureRun(ctx, r)
	if err != nil {
		return session.Incarnation{}, err
	}
	r, err = p.registry.BindRun(ctx, r.Reference, string(run.UID))
	if err != nil {
		return session.Incarnation{}, semantic(err)
	}
	pod, err := p.waitPod(ctx, r, true)
	if err != nil {
		return session.Incarnation{}, err
	}
	r, err = p.registry.BindPod(ctx, r.Reference, string(pod.UID))
	if err != nil {
		return session.Incarnation{}, semantic(err)
	}
	data, err := p.readArtifact(local.Launch.AllocationID, "input")
	if err != nil {
		return session.Incarnation{}, err
	}
	operation := helperPrepare
	if local.Initial != nil {
		operation = helperPrepareWorkspace
	}
	arguments := []string{helperPath, operation, "--harness", string(r.Harness),
		"--max-bytes", strconv.FormatInt(p.config.MaxCheckpointBytes, 10)}
	if local.Initial == nil {
		arguments = append(arguments, "--instruction-digest", r.InstructionDigest)
	}
	output, err := p.control(ctx, pod, runtimekube.CaptureContainer, arguments, data, 64<<10)
	if err != nil {
		return session.Incarnation{}, err
	}
	var meta checkpoint.Metadata
	if err = decode(output, &meta); err != nil || meta.Harness != r.Harness || (local.Initial == nil && (meta.ThreadID != r.NativeUUID || meta.SHA256 != r.CheckpointDigest) || local.Initial != nil && (meta.ThreadID != "" || meta.SHA256 != local.WorkspaceDigest)) {
		return session.Incarnation{}, errors.New("placement: restored checkpoint metadata mismatch")
	}
	for name := range r.Ceilings {
		if err = p.ledger.Initialize(ctx, account(r, name)); err != nil {
			return session.Incarnation{}, err
		}
	}
	r, err = p.registry.Prepared(ctx, r.Reference, string(pod.UID))
	return inc(r), semantic(err)
}
func (p *Placement) ensureRun(ctx context.Context, r registry.Record) (*api.AgentRun, error) {
	caps := make([]api.CapabilityKind, 0, len(r.Ceilings))
	for c := range r.Ceilings {
		caps = append(caps, c)
	}
	slices.Sort(caps)
	task := r.Task
	if task == "" {
		task = fixedTask
	}
	wanted := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: p.config.Namespace, Name: r.IntendedRunName}, Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: p.config.TemplateName}, Task: task, Capabilities: caps}}
	err := p.config.Kube.Create(ctx, wanted)
	if err == nil {
		return wanted, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	var existing api.AgentRun
	if err = p.config.Kube.Get(ctx, client.ObjectKeyFromObject(wanted), &existing); err != nil {
		return nil, err
	}
	if existing.Spec.TemplateRef != wanted.Spec.TemplateRef || existing.Spec.Task != task || !slices.Equal(existing.Spec.Capabilities, caps) || len(existing.Spec.EventContext) != 0 || existing.Spec.Cancel || r.RunUID != "" && string(existing.UID) != r.RunUID {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	return &existing, nil
}
func semantic(err error) error {
	if errors.Is(err, registry.ErrConflict) || errors.Is(err, registry.ErrGone) {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	if errors.Is(err, registry.ErrExpired) {
		return &profile.Rejection{Code: profile.CodeExpired}
	}
	return err
}

var _ session.Placement = (*Placement)(nil)
var _ session.TerminalPlacement = (*Placement)(nil)

func (p *Placement) validateInput(l session.Launch, initial bool) error {
	if workspace.ValidateIgnoredPaths(l.IncludeIgnored) != nil {
		return &profile.Rejection{Code: profile.CodeDenied}
	}
	if l.Harness != p.config.Harness || !idValid(l.SessionID) || !idValid(l.OperationID) || !idValid(l.AllocationID) || (!initial && !idValid(l.ThreadID) || initial && l.ThreadID != "") || l.Epoch == 0 || l.Placement != p.config.Name || (!initial && !digestValid(l.CheckpointDigest) || initial && l.CheckpointDigest != "") || l.InstructionDigest != p.config.InstructionDigest || len(l.Allowances) == 0 || len(l.Allowances) > 32 {
		return &profile.Rejection{Code: profile.CodeDenied}
	}
	if len(l.Task) > 32<<10 || strings.ContainsRune(l.Task, 0) || l.Task != "" && (strings.TrimSpace(l.Task) == "" || l.Harness != harness.NativeCodex) {
		return &profile.Rejection{Code: profile.CodeDenied}
	}
	return nil
}

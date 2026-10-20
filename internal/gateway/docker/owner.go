// Package docker owns local execution grants. Only the trusted placement calls
// lifecycle hooks; the worker-facing proxy exposes authentication and operations.
package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/gateway"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const (
	initializing  = "initializing"
	inactive      = "inactive"
	active        = "active"
	revoked       = "revoked"
	engineRunning = "running"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var nativeUUID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

type Observer interface {
	Observe(context.Context, runtimedocker.Instance) (runtimedocker.Observation, error)
}

type Config struct {
	Directory          string
	Ledger             budget.Ledger
	Observer           Observer
	Policy             gateway.PolicySnapshot
	Template           gateway.TemplateSnapshot
	NativeCapabilities []api.CapabilityKind
}

// Owner retains one private registry writer and fixed administrator snapshots.
// It owns neither worker checkpoints nor the Docker engine lifecycle itself.
type Owner struct {
	mu                 sync.Mutex
	files              *registry
	ledger             budget.Ledger
	observer           Observer
	policy             gateway.PolicySnapshot
	template           gateway.TemplateSnapshot
	profileDigest      string
	instructionDigest  string
	nativeCapabilities []api.CapabilityKind
}

// recordVersion 2 keeps Launch exactly as initialized and records a first
// visit's bound native UUID in Thread. Version 1 rewrote Launch.ThreadID.
const recordVersion = 2

type record struct {
	Version       int                 `json:"version"`
	Launch        session.Launch      `json:"launch"`
	Incarnation   session.Incarnation `json:"incarnation"`
	ProfileDigest string              `json:"profileDigest"`
	// Thread is the genuine native UUID a first visit bound. Resumed launches
	// name theirs in Launch.ThreadID.
	Thread  string                         `json:"thread,omitempty"`
	Token   string                         `json:"token"`
	Phase   string                         `json:"phase"`
	Receipt map[string]budget.TrustedUsage `json:"receipt,omitempty"`
}

func Open(config Config) (*Owner, error) {
	if config.Ledger == nil || config.Observer == nil || config.Policy.Name == "" || config.Template.Name == "" || config.Template.Spec.Instructions == "" {
		return nil, errors.New("docker gateway: fixed policy, template, observer and durable accounting required")
	}
	for _, capability := range config.Policy.Spec.AllowedCapabilities {
		ceiling, ok := config.Policy.Spec.Budgets[capability]
		if !ok || ceiling.MaxUnits <= 0 || ceiling.MaxCostMicros < 0 {
			return nil, errors.New("docker gateway: fixed finite capability ceilings required")
		}
	}
	for _, capability := range config.NativeCapabilities {
		if capability != api.CapabilityKubernetesRead && capability != api.CapabilityGitHubPullRequest {
			return nil, errors.New("docker gateway: unsupported configured native capability")
		}
	}
	profile := (&gateway.ExecutionGrant{Policy: config.Policy, Template: config.Template}).Clone()
	data, err := json.Marshal(struct {
		Policy             gateway.PolicySnapshot
		Template           gateway.TemplateSnapshot
		NativeCapabilities []api.CapabilityKind
	}{profile.Policy, profile.Template, config.NativeCapabilities})
	if err != nil {
		return nil, err
	}
	files, err := openRegistry(config.Directory)
	if err != nil {
		return nil, err
	}
	return &Owner{files: files, ledger: config.Ledger, observer: config.Observer, policy: profile.Policy, template: profile.Template,
		profileDigest: fmt.Sprintf("%x", sha256.Sum256(data)), instructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(profile.Template.Spec.Instructions))), nativeCapabilities: slices.Clone(config.NativeCapabilities)}, nil
}

func (o *Owner) Close() error { o.mu.Lock(); defer o.mu.Unlock(); return o.files.close() }

func (o *Owner) validateIntent(launch session.Launch, instance session.Incarnation, initial bool) error {
	if err := launch.Harness.Validate(); err != nil {
		return err
	}
	if err := workspace.ValidateIgnoredPaths(launch.IncludeIgnored); err != nil {
		return err
	}
	for _, name := range []string{launch.AllocationID, launch.ExecutionID, launch.OperationID, launch.Placement} {
		if !identifier.MatchString(name) {
			return errors.New("docker gateway: invalid launch name")
		}
	}
	if launch.Epoch == 0 || launch.InstructionDigest != o.instructionDigest || launch.Deadline.IsZero() || instance.ExecutionID != launch.AllocationID || !hexDigest.MatchString(instance.ResourceUID) || len(launch.Allowances) == 0 || len(launch.Allowances) > 32 {
		return errors.New("docker gateway: invalid immutable launch binding")
	}
	if initial {
		if launch.CheckpointDigest != "" || (launch.ThreadID != "" && !nativeUUID.MatchString(launch.ThreadID)) {
			return errors.New("docker gateway: invalid initial native binding")
		}
	} else if !nativeUUID.MatchString(launch.ThreadID) || !hexDigest.MatchString(launch.CheckpointDigest) {
		return errors.New("docker gateway: resumed native identity and checkpoint required")
	}
	for name, a := range launch.Allowances {
		if err := o.validateAllocation(name, a); err != nil {
			return err
		}
	}
	return nil
}

func (o *Owner) validateAllocation(name string, a session.Allowance) error {
	capability := api.CapabilityKind(name)
	_, mcp := capability.MCPServerName()
	native := slices.Contains(o.nativeCapabilities, capability)
	if (capability != api.CapabilityModelInference && !mcp && !native) || !slices.Contains(o.policy.Spec.AllowedCapabilities, capability) || !a.Units.Bounded || a.Units.Value <= 0 || a.CostMicros.Value < 0 || (a.CostMicros.Bounded && a.CostMicros.Value == 0) || (!a.CostMicros.Bounded && a.CostMicros.Value != 0) {
		return errors.New("docker gateway: launch capability or finite allocation denied")
	}
	ceiling := o.policy.Spec.Budgets[capability]
	if a.Units.Value > ceiling.MaxUnits || (ceiling.MaxCostMicros > 0 && (!a.CostMicros.Bounded || a.CostMicros.Value > ceiling.MaxCostMicros)) || ((mcp || native) && a.CostMicros.Bounded) {
		return errors.New("docker gateway: allocation exceeds approved profile or lacks trusted pricing")
	}
	return nil
}

func account(allocation, capability string) budget.AccountKey {
	return budget.AccountKey(allocation + "/" + capability)
}

func (o *Owner) bound(ctx context.Context, r record, running bool) error {
	state, err := o.observer.Observe(ctx, runtimedocker.Instance{ID: r.Incarnation.ResourceUID, Execution: r.Incarnation.ExecutionID})
	if err != nil || state.ID != r.Incarnation.ResourceUID || state.Execution != r.Incarnation.ExecutionID {
		return errors.New("docker gateway: execution observation unavailable")
	}
	if running {
		if !state.Running || state.Status != engineRunning || state.PID <= 0 {
			return errors.New("docker gateway: execution is not running")
		}
	} else {
		started := state.Running && state.Status == engineRunning && state.PID > 0
		created := state.Status == "created" && !state.Running && state.PID == 0
		if !started && !created {
			return errors.New("docker gateway: execution cannot be initialized")
		}
	}
	return nil
}

func (o *Owner) load(id string) (record, error) {
	r, err := o.files.read(id)
	if err != nil {
		return record{}, err
	}
	if r.Version != recordVersion || r.Launch.AllocationID != id || r.ProfileDigest != o.profileDigest || o.validateIntent(r.Launch, r.Incarnation, r.initial()) != nil ||
		r.Thread != "" && (!r.initial() || !nativeUUID.MatchString(r.Thread)) {
		return record{}, errors.New("docker gateway: registry binding is unavailable")
	}
	separator := strings.LastIndex(r.Token, ".")
	if separator < 1 || r.Token[:separator] != id {
		return record{}, errors.New("docker gateway: invalid retained token")
	}
	secret, err := base64.RawURLEncoding.DecodeString(r.Token[separator+1:])
	if err != nil || len(secret) != 32 {
		return record{}, errors.New("docker gateway: invalid retained token")
	}
	if r.Phase != initializing && r.Phase != inactive && r.Phase != active && r.Phase != revoked {
		return record{}, errors.New("docker gateway: invalid retained lifecycle")
	}
	if r.Receipt != nil {
		if r.Phase != revoked || len(r.Receipt) != len(r.Launch.Allowances) {
			return record{}, errors.New("docker gateway: invalid retained receipt")
		}
		for cap, a := range r.Launch.Allowances {
			u, ok := r.Receipt[cap]
			if !ok || u.Units < 0 || u.CostMicros < 0 || u.Units > a.Units.Value || (a.CostMicros.Bounded && u.CostMicros > a.CostMicros.Value) {
				return record{}, errors.New("docker gateway: inconsistent retained receipt")
			}
		}
	}
	return r, nil
}

// initial reports a first visit started from a Git workspace, whose launch
// names no native UUID until the placement binds the genuine one.
func (r record) initial() bool { return r.Launch.CheckpointDigest == "" }

func (o *Owner) matched(launch session.Launch, instance session.Incarnation) (record, error) {
	launch = normalizeLaunch(launch)
	r, err := o.load(launch.AllocationID)
	if err != nil {
		return record{}, err
	}
	if !session.SameLaunch(r.Launch, launch) || r.Incarnation != instance {
		return record{}, errors.New("docker gateway: allocation intent changed")
	}
	return r, nil
}

func normalizeLaunch(launch session.Launch) session.Launch {
	launch.Deadline = launch.Deadline.UTC()
	launch.Allowances = maps.Clone(launch.Allowances)
	launch.IncludeIgnored = append([]string(nil), launch.IncludeIgnored...)
	return launch
}

// Initialize retains the credential before creating accounts so retries after
// partial writes converge. Inactive/active retries never recreate a lost account.
func (o *Owner) Initialize(ctx context.Context, launch session.Launch, instance session.Incarnation) (string, error) {
	launch = normalizeLaunch(launch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.validateIntent(launch, instance, launch.CheckpointDigest == ""); err != nil {
		return "", err
	}
	if !launch.Deadline.After(time.Now()) {
		return "", errors.New("docker gateway: launch expired")
	}
	r, err := o.load(launch.AllocationID)
	if errors.Is(err, errRecordMissing) {
		if launch.CheckpointDigest == "" && launch.ThreadID != "" {
			return "", errors.New("docker gateway: initial UUID must come from actual native binding")
		}
		for capability := range launch.Allowances {
			if _, stateErr := o.ledger.State(ctx, account(launch.AllocationID, capability)); !errors.Is(stateErr, budget.ErrAccountNotFound) {
				return "", errors.New("docker gateway: new allocation account is present or unavailable")
			}
		}
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			return "", err
		}
		r = record{Version: recordVersion, Launch: launch, Incarnation: instance, ProfileDigest: o.profileDigest, Token: launch.AllocationID + "." + base64.RawURLEncoding.EncodeToString(secret), Phase: initializing}
		if err = o.bound(ctx, r, false); err != nil {
			return "", err
		}
		if err = o.files.save(r); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if !session.SameLaunch(r.Launch, launch) || r.Incarnation != instance || r.Phase == revoked {
		return "", errors.New("docker gateway: allocation intent changed or revoked")
	}
	if r.Phase == initializing {
		for cap := range launch.Allowances {
			if err = o.ledger.Initialize(ctx, account(launch.AllocationID, cap)); err != nil {
				return "", err
			}
		}
		r.Phase = inactive
		if err = o.files.save(r); err != nil {
			return "", err
		}
	}
	for cap := range launch.Allowances {
		if _, err = o.ledger.State(ctx, account(launch.AllocationID, cap)); err != nil {
			return "", err
		}
	}
	return r.Token, nil
}

func (o *Owner) Activate(ctx context.Context, launch session.Launch, instance session.Incarnation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.matched(launch, instance)
	if err != nil {
		return err
	}
	if (r.Phase != inactive && r.Phase != active) || !r.Launch.Deadline.After(time.Now()) {
		return errors.New("docker gateway: allocation cannot activate")
	}
	if err = o.bound(ctx, r, true); err != nil {
		return err
	}
	for cap := range launch.Allowances {
		if _, err = o.ledger.State(ctx, account(launch.AllocationID, cap)); err != nil {
			return err
		}
	}
	r.Phase = active
	return o.files.save(r)
}

func (o *Owner) Revoke(_ context.Context, launch session.Launch, instance session.Incarnation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.matched(launch, instance)
	if err != nil {
		return err
	}
	if r.Phase == revoked {
		return nil
	}
	r.Phase = revoked
	return o.files.save(r)
}

func (o *Owner) Seal(ctx context.Context, launch session.Launch, instance session.Incarnation) (map[string]budget.TrustedUsage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.matched(launch, instance)
	if err != nil {
		return nil, err
	}
	if r.Phase != revoked {
		return nil, errors.New("docker gateway: revoke before sealing")
	}
	if r.initial() && r.Thread == "" {
		return nil, errors.New("docker gateway: genuine initial native UUID is not bound")
	}
	if r.Receipt != nil {
		return maps.Clone(r.Receipt), nil
	}
	usage := make(map[string]budget.TrustedUsage, len(launch.Allowances))
	for cap := range launch.Allowances {
		u, sealErr := o.ledger.Seal(ctx, account(launch.AllocationID, cap))
		if sealErr != nil {
			return nil, sealErr
		}
		usage[cap] = u
	}
	r.Receipt = usage
	if err = o.files.save(r); err != nil {
		return nil, err
	}
	return maps.Clone(usage), nil
}

// BindNative records the UUID generated by the real initial native process,
// without changing the launch. Only the trusted placement supplies that
// observation; it grants no additional allowance and cannot replace a
// previously observed UUID. It returns the bound UUID.
func (o *Owner) BindNative(_ context.Context, launch session.Launch, instance session.Incarnation, actualUUID string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !nativeUUID.MatchString(actualUUID) {
		return "", errors.New("docker gateway: invalid actual native UUID")
	}
	r, err := o.matched(launch, instance)
	if err != nil {
		return "", err
	}
	if !r.initial() || r.Phase == initializing || r.Receipt != nil {
		return "", errors.New("docker gateway: initial native binding is unavailable")
	}
	if r.Thread != "" && r.Thread != actualUUID {
		return "", errors.New("docker gateway: native UUID cannot change")
	}
	r.Thread = actualUUID
	if err = o.files.save(r); err != nil {
		return "", err
	}
	return r.Thread, nil
}

func (o *Owner) grant(r record) *gateway.ExecutionGrant {
	g := (&gateway.ExecutionGrant{ExecutionID: r.Launch.ExecutionID, Execution: gateway.Reference{Backend: gateway.BackendDocker, Name: r.Incarnation.ExecutionID, UID: r.Incarnation.ResourceUID}, Policy: o.policy, Template: o.template, Managed: true, Allocations: map[api.CapabilityKind]gateway.Allocation{}}).Clone()
	g.Capabilities = make([]api.CapabilityKind, 0, len(r.Launch.Allowances))
	for name, a := range r.Launch.Allowances {
		cap := api.CapabilityKind(name)
		g.Capabilities = append(g.Capabilities, cap)
		g.Allocations[cap] = gateway.Allocation{AccountKey: string(account(r.Launch.AllocationID, name)), Ceiling: api.EndpointBudget{MaxUnits: a.Units.Value, MaxCostMicros: a.CostMicros.Value}}
	}
	slices.Sort(g.Capabilities)
	return g
}

func (o *Owner) Authenticate(ctx context.Context, token string) (*gateway.ExecutionGrant, error) {
	separator := strings.LastIndex(token, ".")
	if separator < 1 || len(token) > 256 {
		return nil, gateway.ErrTokenInvalid
	}
	id := token[:separator]
	if !identifier.MatchString(id) {
		return nil, gateway.ErrTokenInvalid
	}
	r, err := o.activeRecord(id)
	if err != nil || subtle.ConstantTimeCompare([]byte(token), []byte(r.Token)) != 1 {
		return nil, gateway.ErrTokenInvalid
	}
	if err = o.bound(ctx, r, true); err != nil {
		return nil, gateway.ErrGrantRevoked
	}
	r, err = o.activeRecord(id)
	if err != nil || subtle.ConstantTimeCompare([]byte(token), []byte(r.Token)) != 1 {
		return nil, gateway.ErrGrantRevoked
	}
	return o.grant(r), nil
}

func (o *Owner) Revalidate(ctx context.Context, grant *gateway.ExecutionGrant) error {
	if grant == nil || grant.Execution.Backend != gateway.BackendDocker || !identifier.MatchString(grant.Execution.Name) {
		return gateway.ErrGrantRevoked
	}
	r, err := o.activeRecord(grant.Execution.Name)
	if err != nil || !reflect.DeepEqual(o.grant(r), grant) || o.bound(ctx, r, true) != nil {
		return gateway.ErrGrantRevoked
	}
	r, err = o.activeRecord(grant.Execution.Name)
	if err != nil || !reflect.DeepEqual(o.grant(r), grant) {
		return gateway.ErrGrantRevoked
	}
	return nil
}

// Engine observation may block. Registry revocation must remain independent of
// that call, and the grant is checked again after observation before returning.
func (o *Owner) activeRecord(id string) (record, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.load(id)
	if err != nil || r.Phase != active || !r.Launch.Deadline.After(time.Now()) {
		return record{}, gateway.ErrGrantRevoked
	}
	return r, nil
}

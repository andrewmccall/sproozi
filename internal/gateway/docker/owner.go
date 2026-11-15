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
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/gateway"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const engineRunning = "running"

// grantPhase is where a grant record is in its lifecycle. The record declares
// its phases and the events that move it (grantTransitions, ADR 0058). Only an
// active grant authenticates. Revoking an initializing grant first finishes
// creating its accounts, so every revoked grant can seal.
//
// An operation on a grant that already holds its outcome replays it without
// an event: Initialize of an inactive or active grant, Activate of an active
// one and Revoke of a revoked one.
type grantPhase string

const (
	initializing grantPhase = "initializing"
	inactive     grantPhase = "inactive"
	active       grantPhase = "active"
	revoked      grantPhase = "revoked"

	// grantNone is the phase before Initialize retains a record.
	grantNone grantPhase = ""
)

var grantPhases = []grantPhase{initializing, inactive, active, revoked}

// grantEvent is a request to the grant owner or the outcome of its work.
type grantEvent string

const (
	// evInitialize retains a new grant and its credential.
	evInitialize grantEvent = "initialize"
	// evInitialized: every account of the allocation exists.
	evInitialized grantEvent = "initialized"
	// evActivate lets the grant authenticate.
	evActivate grantEvent = "activate"
	// evRevoke ends the grant.
	evRevoke grantEvent = "revoke"
	// evSeal retains the receipt of a revoked grant; the phase stays.
	evSeal grantEvent = "seal"
	// evBindNative records a first visit's native UUID; the phase stays.
	evBindNative grantEvent = "bind native"
)

// grantRule is what an event may do.
type grantRule struct {
	// refusedAfterDeadline: the event starts or extends work, so it is not
	// attempted once the launch's deadline has passed. A launch without a
	// deadline refuses nothing (ADR 0056).
	refusedAfterDeadline bool
	// to maps each phase the event may happen in to the phase it leads to.
	to map[grantPhase]grantPhase
}

// grantTransitions declares every phase change the grant owner makes.
var grantTransitions = map[grantEvent]grantRule{
	evInitialize:  {refusedAfterDeadline: true, to: map[grantPhase]grantPhase{grantNone: initializing}},
	evInitialized: {refusedAfterDeadline: true, to: map[grantPhase]grantPhase{initializing: inactive}},
	evActivate:    {refusedAfterDeadline: true, to: map[grantPhase]grantPhase{inactive: active}},
	evRevoke:      {to: map[grantPhase]grantPhase{initializing: revoked, inactive: revoked, active: revoked}},
	evSeal:        {to: map[grantPhase]grantPhase{revoked: revoked}},
	evBindNative:  {to: map[grantPhase]grantPhase{inactive: inactive, active: active, revoked: revoked}},
}

// admit checks, before the owner attempts ev's side effect, that ev may
// happen in r's phase and, for an event that starts work, that the launch's
// deadline has not passed at now.
func (r record) admit(ev grantEvent, now time.Time) error {
	rule := grantTransitions[ev]
	if _, ok := rule.to[r.Phase]; !ok {
		return refused(r.Phase, ev)
	}
	if rule.refusedAfterDeadline && deadline.At(r.Launch.Deadline).Passed(now) {
		return fmt.Errorf("docker gateway: %s refused after the launch deadline", ev)
	}
	return nil
}

// apply records ev: it moves r to the phase grantTransitions declares, or
// refuses an undeclared transition and leaves r unchanged. It is the only
// place a grant's phase changes; save then validates the record.
func (r *record) apply(ev grantEvent) error {
	to, ok := grantTransitions[ev].to[r.Phase]
	if !ok {
		return refused(r.Phase, ev)
	}
	r.Phase = to
	return nil
}

func refused(from grantPhase, ev grantEvent) error {
	if from == grantNone {
		from = "none"
	}
	return fmt.Errorf("docker gateway: %s refused while the allocation is %s", ev, from)
}

// commit applies ev and saves the record.
func (o *Owner) commit(r *record, ev grantEvent) error {
	if err := r.apply(ev); err != nil {
		return err
	}
	return o.save(*r)
}

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
	// now is the clock deadlines are checked against.
	now func() time.Time
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
	Phase   grantPhase                     `json:"phase"`
	Receipt map[string]budget.TrustedUsage `json:"receipt,omitempty"`
}

func Open(config Config) (*Owner, error) {
	if config.Ledger == nil || config.Observer == nil || config.Policy.Name == "" || config.Template.Name == "" || config.Template.Spec.Instructions == "" {
		return nil, errors.New("docker gateway: fixed policy, template, observer and durable accounting required")
	}
	for _, capability := range config.Policy.Spec.AllowedCapabilities {
		ceiling, ok := config.Policy.Spec.Budgets[capability]
		if !ok || !gateway.CeilingValid(capability, ceiling) {
			return nil, errors.New("docker gateway: fixed capability ceilings required")
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
		profileDigest: fmt.Sprintf("%x", sha256.Sum256(data)), instructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(profile.Template.Spec.Instructions))), nativeCapabilities: slices.Clone(config.NativeCapabilities), now: time.Now}, nil
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
	if launch.Epoch == 0 || launch.InstructionDigest != o.instructionDigest || instance.ExecutionID != launch.AllocationID || !hexDigest.MatchString(instance.ResourceUID) || len(launch.Allowances) == 0 || len(launch.Allowances) > 32 {
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
	if (capability != api.CapabilityModelInference && !mcp && !native) || !slices.Contains(o.policy.Spec.AllowedCapabilities, capability) || !a.Valid(capability) {
		return errors.New("docker gateway: launch capability or allocation denied")
	}
	ceiling := o.policy.Spec.Budgets[capability]
	if (ceiling.MaxUnits > 0 && (!a.Units.Bounded || a.Units.Value > ceiling.MaxUnits)) || (ceiling.MaxCostMicros > 0 && (!a.CostMicros.Bounded || a.CostMicros.Value > ceiling.MaxCostMicros)) || ((mcp || native) && a.CostMicros.Bounded) {
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
	if err = o.validate(r, id); err != nil {
		return record{}, err
	}
	return r, nil
}

// save validates r as load does before writing it, so a record that
// contradicts itself is never retained.
func (o *Owner) save(r record) error {
	if err := o.validate(r, r.Launch.AllocationID); err != nil {
		return err
	}
	return o.files.save(r)
}

// validate checks a grant record retained, or about to be retained, under id.
func (o *Owner) validate(r record, id string) error {
	if r.Version != recordVersion || r.Launch.AllocationID != id || r.ProfileDigest != o.profileDigest || o.validateIntent(r.Launch, r.Incarnation, r.initial()) != nil ||
		r.Thread != "" && (!r.initial() || !nativeUUID.MatchString(r.Thread)) {
		return errors.New("docker gateway: registry binding is unavailable")
	}
	separator := strings.LastIndex(r.Token, ".")
	if separator < 1 || r.Token[:separator] != id {
		return errors.New("docker gateway: invalid retained token")
	}
	secret, err := base64.RawURLEncoding.DecodeString(r.Token[separator+1:])
	if err != nil || len(secret) != 32 {
		return errors.New("docker gateway: invalid retained token")
	}
	if !slices.Contains(grantPhases, r.Phase) {
		return errors.New("docker gateway: invalid retained lifecycle")
	}
	if r.Receipt != nil {
		if r.Phase != revoked || len(r.Receipt) != len(r.Launch.Allowances) {
			return errors.New("docker gateway: invalid retained receipt")
		}
		for cap, a := range r.Launch.Allowances {
			u, ok := r.Receipt[cap]
			if !ok || u.Units < 0 || u.CostMicros < 0 || (a.Units.Bounded && u.Units > a.Units.Value) || (a.CostMicros.Bounded && u.CostMicros > a.CostMicros.Value) {
				return errors.New("docker gateway: inconsistent retained receipt")
			}
		}
	}
	return nil
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
	r, err := o.load(launch.AllocationID)
	if errors.Is(err, errRecordMissing) {
		if launch.CheckpointDigest == "" && launch.ThreadID != "" {
			return "", errors.New("docker gateway: initial UUID must come from actual native binding")
		}
		r = record{Version: recordVersion, Launch: launch, Incarnation: instance, ProfileDigest: o.profileDigest}
		if err = r.admit(evInitialize, o.now()); err != nil {
			return "", err
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
		r.Token = launch.AllocationID + "." + base64.RawURLEncoding.EncodeToString(secret)
		if err = o.bound(ctx, r, false); err != nil {
			return "", err
		}
		if err = o.commit(&r, evInitialize); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if !session.SameLaunch(r.Launch, launch) || r.Incarnation != instance {
		return "", errors.New("docker gateway: allocation intent changed")
	}
	// An inactive or active grant already holds its accounts: a replay.
	if r.Phase != inactive && r.Phase != active {
		if err = r.admit(evInitialized, o.now()); err != nil {
			return "", err
		}
		if err = o.initializeAccounts(ctx, r); err != nil {
			return "", err
		}
		if err = o.commit(&r, evInitialized); err != nil {
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
	// An active grant already holds the outcome; the deadline does not refuse
	// its replay (ADR 0058).
	if r.Phase == active {
		return nil
	}
	if err = r.admit(evActivate, o.now()); err != nil {
		return err
	}
	if err = o.bound(ctx, r, true); err != nil {
		return err
	}
	for cap := range launch.Allowances {
		if _, err = o.ledger.State(ctx, account(launch.AllocationID, cap)); err != nil {
			return err
		}
	}
	return o.commit(&r, evActivate)
}

// initializeAccounts creates every account of the allocation. Ledger
// initialization is idempotent, so a retry after a partial write converges.
func (o *Owner) initializeAccounts(ctx context.Context, r record) error {
	for cap := range r.Launch.Allowances {
		if err := o.ledger.Initialize(ctx, account(r.Launch.AllocationID, cap)); err != nil {
			return err
		}
	}
	return nil
}

// Revoke ends the grant. An initializing grant never authenticated anything,
// but some of its accounts may be missing, and Seal refuses a missing account
// while Initialize refuses a revoked grant. Revoking it therefore finishes
// creating the accounts first, and stays initializing if that fails, so a
// revoked grant can always seal.
func (o *Owner) Revoke(ctx context.Context, launch session.Launch, instance session.Incarnation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.matched(launch, instance)
	if err != nil {
		return err
	}
	if r.Phase == revoked {
		return nil
	}
	if err = r.admit(evRevoke, o.now()); err != nil {
		return err
	}
	if r.Phase == initializing {
		if err = o.initializeAccounts(ctx, r); err != nil {
			return fmt.Errorf("docker gateway: revoking an initializing allocation: %w", err)
		}
	}
	return o.commit(&r, evRevoke)
}

func (o *Owner) Seal(ctx context.Context, launch session.Launch, instance session.Incarnation) (map[string]budget.TrustedUsage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.matched(launch, instance)
	if err != nil {
		return nil, err
	}
	if err = r.admit(evSeal, o.now()); err != nil {
		return nil, fmt.Errorf("%w: revoke before sealing", err)
	}
	// A first visit can stop with no bound UUID: cancelled before its
	// conversation started, or stopped turnless. Its usage still seals; the
	// receipt then refuses any later binding.
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
	if err = o.save(r); err != nil {
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
	if !r.initial() || r.admit(evBindNative, o.now()) != nil || r.Receipt != nil {
		return "", errors.New("docker gateway: initial native binding is unavailable")
	}
	if r.Thread != "" && r.Thread != actualUUID {
		return "", errors.New("docker gateway: native UUID cannot change")
	}
	r.Thread = actualUUID
	if err = o.save(r); err != nil {
		return "", err
	}
	return r.Thread, nil
}

func (o *Owner) grant(r record) *gateway.ExecutionGrant {
	g := (&gateway.ExecutionGrant{Execution: gateway.Reference{Backend: gateway.BackendDocker, Name: r.Incarnation.ExecutionID, UID: r.Incarnation.ResourceUID}, Policy: o.policy, Template: o.template, Managed: true, Allocations: map[api.CapabilityKind]gateway.Allocation{}}).Clone()
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
	if err != nil || r.Phase != active || deadline.At(r.Launch.Deadline).Passed(o.now()) {
		return record{}, gateway.ErrGrantRevoked
	}
	return r, nil
}

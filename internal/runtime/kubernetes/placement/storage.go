package placement

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

const recordSuffix = ".json"

// retained keeps the launch exactly as prepared. A first visit's genuine
// native UUID is bound in the registry record (NativeUUID), never in Launch.
type retained struct {
	WorkspaceDigest string             `json:"workspaceDigest,omitempty"`
	Launch          session.Launch     `json:"launch"`
	Reference       registry.Reference `json:"reference"`
	OutputDigest    string             `json:"outputDigest,omitempty"`
	NativeQuiet     bool               `json:"nativeQuiet,omitempty"`
	// Turnless records that the supervisor quiesced a first visit with no
	// native conversation (ADR 0032): its stop captures nothing.
	Turnless     bool `json:"turnless,omitempty"`
	WriterFenced bool `json:"writerFenced,omitempty"`
	// Exited records a harness that ended without a graceful stop, from the
	// supervisor's exit report or the container's exit code. It is a
	// workload diagnostic, never a writer fence or accounting fact.
	Exited *session.Exit `json:"exited,omitempty"`
	// PreparationAborted records that AbortPreparation fenced this launch:
	// Prepare refuses it and nothing creates its registration again.
	PreparationAborted bool `json:"preparationAborted,omitempty"`
}

func privatePath(path string, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	if !ok || owner.Uid != uint32(os.Getuid()) || info.IsDir() != dir || info.Mode().Perm() != mode || info.Mode()&os.ModeSymlink != 0 || !dir && !info.Mode().IsRegular() {
		return errors.New("placement: private owned storage required")
	}
	return nil
}
func (p *Placement) persistIntent(l session.Launch, data []byte) (retained, error) {
	l.Deadline = l.Deadline.UTC()
	old, err := p.load(l)
	if err == nil {
		return old, nil
	}
	if !os.IsNotExist(err) {
		return retained{}, err
	}
	if err = p.claimEpoch(l); err != nil {
		return retained{}, err
	}
	if err = p.atomic(p.artifactPath(l.AllocationID, "input"), data); err != nil {
		return retained{}, err
	}
	r := retained{Launch: l}
	return r, p.save(r, nil)
}

// claimEpoch records l as its session epoch's one allocation. One allocation
// per principal/session epoch prevents alternative allocation IDs from
// replaying the same epoch through another deterministic Run name; another
// launch for a claimed epoch is a conflict.
func (p *Placement) claimEpoch(l session.Launch) error {
	epochPath := filepath.Join(p.config.Directory, "epoch-"+l.ExecutionID+"-"+strconv.FormatUint(l.Epoch, 10)+recordSuffix)
	epochData, err := p.read(epochPath, 64<<10)
	if err == nil {
		var previous session.Launch
		if decode(epochData, &previous) != nil || !session.SameLaunch(l, previous) {
			return &profile.Rejection{Code: profile.CodeConflict}
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	encoded, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return p.atomic(epochPath, encoded)
}

// load reads l's retained record. A record whose facts contradict each other
// is returned with those facts forgotten, and with a *contradiction error.
func (p *Placement) load(l session.Launch) (retained, error) {
	if !idValid(l.AllocationID) {
		return retained{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	data, err := p.read(filepath.Join(p.config.Directory, l.AllocationID+recordSuffix), 64<<10)
	if err != nil {
		return retained{}, err
	}
	var r retained
	if err = decode(data, &r); err != nil {
		return retained{}, err
	}
	if !session.SameLaunch(r.Launch, l) {
		return retained{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	r, err = r.check(nil)
	return r, p.noted(err)
}

// A visit's facts are retained in the order they become true, and each
// needs the facts before it. The record holds its registration's reference
// once the registration exists. A stop that reached the active source then
// records it quiet (nativeQuiet) and, while quieting it, a harness that had
// exited (exited) and a first visit with no conversation (turnless). Once
// the registration records a stop or a cancellation and the native
// container is seen ended, the writer is fenced (writerFenced). Only then is
// what the stop keeps retained (outputDigest). The tombstone of an aborted
// preparation stands beside any of these.
//
// retainedRules lists each contradiction of that order, between the
// record's own facts or with its session registration, and its correction:
// what the registration proves is taken from it, and any other fact the
// contradiction makes untrustworthy is forgotten. A rule that needs the
// registration holds whenever there is none to compare with.
//
// A registration past the point a fact proves is the authority for it: its
// phase was recorded only after the executor retained the facts before it.
// A stopped registration's receipt proves the source quiet, the writer
// fenced and the output it names kept, so a record that lacks them takes
// them from the receipt rather than forgetting what it keeps. A cancelled
// registration with a receipt proves the writer fenced: it was sealed only
// once the fence was seen. A record lacking that fence is not a
// contradiction (an abort of a preparation this executor never saw writes
// one), and check adds it. A stopping registration proves only that the
// gateway's authority was revoked: Stop observes the source again for a
// record that lacks its quiet marker. The other phases prove no fact of the
// record.
var retainedRules = []struct {
	contradiction string
	broken        func(r retained, registration *registry.Record) bool
	correct       func(*retained, *registry.Record)
}{
	{"lacks what its stopped registration's receipt proves (a quiet source, a fenced writer and the output it names)",
		func(r retained, registration *registry.Record) bool {
			return stoppedReceipt(registration) != nil &&
				(!r.NativeQuiet || !r.WriterFenced || r.OutputDigest != stoppedReceipt(registration).CheckpointDigest)
		},
		func(r *retained, registration *registry.Record) {
			r.NativeQuiet, r.WriterFenced, r.OutputDigest = true, true, stoppedReceipt(registration).CheckpointDigest
		}},
	{"names a workspace, but the launch resumes a checkpoint",
		func(r retained, _ *registry.Record) bool { return r.WorkspaceDigest != "" && !r.fresh() },
		func(r *retained, _ *registry.Record) { r.WorkspaceDigest = "" }},
	{"records a stop, but has no session registration",
		func(r retained, _ *registry.Record) bool { return r.Reference.UID == "" && r.stopRecorded() },
		forgetStop},
	{"records a stop, but its session registration was never active",
		func(r retained, registration *registry.Record) bool {
			return registration != nil && r.stopRecorded() &&
				(registration.Phase == registry.Creating || registration.Phase == registry.Bound || registration.Phase == registry.Prepared)
		},
		forgetStop},
	{"records the writer fenced, but its session registration is still active",
		func(r retained, registration *registry.Record) bool {
			return registration != nil && registration.Phase == registry.Active && r.WriterFenced
		},
		func(r *retained, _ *registry.Record) { r.WriterFenced = false }},
	{"keeps output before the source was quiet and its writer fenced",
		func(r retained, _ *registry.Record) bool {
			return r.OutputDigest != "" && (!r.NativeQuiet || !r.WriterFenced)
		},
		func(r *retained, _ *registry.Record) { r.OutputDigest = "" }},
	{"is turnless, but the launch resumes a checkpoint",
		func(r retained, _ *registry.Record) bool { return r.Turnless && !r.fresh() },
		func(r *retained, _ *registry.Record) { r.Turnless = false }},
	{"is turnless before the source was quiet",
		func(r retained, _ *registry.Record) bool { return r.Turnless && !r.NativeQuiet },
		func(r *retained, _ *registry.Record) { r.Turnless = false }},
	{"is turnless, but its session registration bound a conversation",
		func(r retained, registration *registry.Record) bool {
			return registration != nil && r.Turnless && registration.NativeUUID != ""
		},
		func(r *retained, _ *registry.Record) { r.Turnless = false }},
}

// stoppedReceipt is the receipt of a stopped registration, or nil.
func stoppedReceipt(registration *registry.Record) *registry.StopReceipt {
	if registration == nil || registration.Phase != registry.Stopped {
		return nil
	}
	return registration.Receipt
}

// stopRecorded reports any fact a stop records.
func (r retained) stopRecorded() bool {
	return r.NativeQuiet || r.Turnless || r.Exited != nil || r.WriterFenced || r.OutputDigest != ""
}

func forgetStop(r *retained, _ *registry.Record) {
	r.NativeQuiet, r.Turnless, r.Exited, r.WriterFenced, r.OutputDigest = false, false, nil, false, ""
}

// contradiction is a retained record whose facts contradict each other or
// the session registration it names.
type contradiction struct {
	allocation string
	reasons    []string
}

// Unwrap makes a contradiction session.ErrRecordContradicted and, over the
// profile protocol, the permanent conflict a retry does not cure.
func (c *contradiction) Unwrap() []error {
	return []error{session.ErrRecordContradicted, &profile.Rejection{Code: profile.CodeConflict}}
}

func (c *contradiction) Error() string {
	return fmt.Sprintf("placement: the retained record of visit %s %s", c.allocation, strings.Join(c.reasons, "; it "))
}

// check is the one rule for which retained records are valid, alone
// (registration nil) or beside their session registration. It returns r
// with what the registration proves taken from it and every other fact a
// contradiction makes untrustworthy forgotten, and the contradictions, or r
// and nil. A rule's correction can break only a rule later in
// retainedRules (forgetting the fence breaks the output's), so one pass in
// order finds them all.
func (r retained) check(registration *registry.Record) (retained, error) {
	if registration != nil && registration.Phase == registry.Cancelled && registration.Receipt != nil {
		r.WriterFenced = true
	}
	var reasons []string
	for _, rule := range retainedRules {
		if rule.broken(r, registration) {
			reasons = append(reasons, rule.contradiction)
			rule.correct(&r, registration)
		}
	}
	if reasons == nil {
		return r, nil
	}
	return r, &contradiction{allocation: r.Launch.AllocationID, reasons: reasons}
}

// fresh reports a first visit started from a Git workspace rather than a checkpoint.
func (r retained) fresh() bool { return r.Launch.CheckpointDigest == "" }

// thread is the native thread the visit runs: the launch's, or the genuine one
// a first visit bound in the registry.
func (r retained) thread(record registry.Record) string {
	return cmp.Or(r.Launch.ThreadID, record.NativeUUID)
}

// save retains r, which must be valid beside registration, the session
// registration the caller holds, or nil before there is one.
func (p *Placement) save(r retained, registration *registry.Record) error {
	if _, err := r.check(registration); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return p.atomic(filepath.Join(p.config.Directory, r.Launch.AllocationID+recordSuffix), data)
}
func (p *Placement) artifactPath(id, kind string) string {
	return filepath.Join(p.config.Directory, id+"-"+kind+".checkpoint")
}
func (p *Placement) readArtifact(id, kind string) ([]byte, error) {
	return p.read(p.artifactPath(id, kind), p.config.MaxCheckpointBytes)
}
func (p *Placement) read(path string, max int64) ([]byte, error) {
	if err := privatePath(path, false); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("placement: retained data exceeds limit")
	}
	return data, nil
}
func (p *Placement) atomic(path string, data []byte) error {
	f, err := os.CreateTemp(p.config.Directory, ".pending-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(p.config.Directory)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func decode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("placement: trailing JSON")
	}
	return nil
}

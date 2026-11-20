// Package checkpoint transfers a stopped native conversation and its unfinished
// Git workspace as one bounded artifact. Authority and launch configuration are
// supplied independently by each placement.
package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const formatVersion = 2

type codexArtifact struct {
	Version   int                `json:"version"`
	Native    harness.CodexState `json:"native"`
	Workspace []byte             `json:"workspace"`
}

type nativeArtifact struct {
	Version   int                   `json:"version"`
	Harness   harness.NativeKind    `json:"harness"`
	Codex     *harness.CodexState   `json:"codex,omitempty"`
	Claude    *harness.ClaudeState  `json:"claude,omitempty"`
	Copilot   *harness.CopilotState `json:"copilot,omitempty"`
	Workspace []byte                `json:"workspace"`
}

// Metadata describes continuity data, never a placement's stop or usage receipt.
type Metadata struct {
	Harness   harness.NativeKind `json:"harness"`
	ThreadID  string             `json:"threadId"`
	TurnID    string             `json:"turnId"`
	SHA256    string             `json:"sha256"`
	Bytes     int                `json:"bytes"`
	Workspace workspace.Metadata `json:"workspace"`
}

// CaptureStopped must run after the placement independently confirms all
// native and workspace writers stopped. The native marker is a compatibility
// check, not that fence. Ignored files are included only by explicit selection.
func CaptureStopped(ctx context.Context, kind harness.NativeKind, home, repository, threadID string,
	selectedIgnored []string, maxBytes int64) ([]byte, Metadata, error) {
	artifact := nativeArtifact{Version: formatVersion, Harness: kind}
	h, err := harness.Managed(kind)
	if err != nil || h.CaptureState == nil {
		return nil, Metadata{}, errUnsupported
	}
	state, err := h.CaptureState(home, repository, threadID, maxBytes)
	if err != nil {
		return nil, Metadata{}, err
	}
	if err = artifact.setState(state); err != nil {
		return nil, Metadata{}, err
	}
	native, _, err := artifact.native(maxBytes)
	if err != nil {
		return nil, Metadata{}, RefuseState(err)
	}
	artifact.Workspace, err = workspace.Capture(ctx, repository, selectedIgnored, maxBytes-int64(native.Size()))
	if errors.Is(err, workspace.ErrByteLimit) {
		// The workspace with the conversation exceeds the bound.
		return nil, Metadata{}, RefuseState(err)
	}
	if err != nil {
		return nil, Metadata{}, err
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		return nil, Metadata{}, err
	}
	meta, _, err := inspect(data, maxBytes)
	return data, meta, RefuseState(err)
}

// Inspect validates native identity and candidate settled state, and reports the exact artifact
// digest. Workspace validation occurs during inert restore before publication.
func Inspect(data []byte, maxBytes int64) (Metadata, error) {
	meta, _, err := inspect(data, maxBytes)
	return meta, err
}

// Workspace exposes only the validated workspace portion, without native
// history or authority. Restore still verifies Git objects before activation.
func Workspace(data []byte, maxBytes int64) ([]byte, workspace.Metadata, error) {
	meta, artifact, err := inspect(data, maxBytes)
	if err != nil {
		return nil, workspace.Metadata{}, err
	}
	return artifact.Workspace, meta.Workspace, nil
}

// ValidateContinuity checks the explicit selected native adapter, then the
// state's own evidence of approved instructions: Codex compares the
// instructions its rollout started with; Claude Code and Copilot keep none, so
// the trusted launch must independently hold approved instructions invariant
// and supply them fresh, and this check reports continuity compatibility only.
// The digest must be well formed either way. This supplies no writer fence or
// authority.
func ValidateContinuity(data []byte, kind harness.NativeKind, instructionDigest string, maxBytes int64) error {
	_, artifact, err := inspect(data, maxBytes)
	if err != nil {
		return err
	}
	if artifact.Harness != kind {
		return errors.New("checkpoint: native harness conflicts with trusted launch")
	}
	state, err := artifact.state()
	if err != nil {
		return err
	}
	if err = state.ValidateContinuity(instructionDigest, maxBytes); err != nil {
		return err
	}
	digest, err := hex.DecodeString(instructionDigest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("checkpoint: invalid trusted instruction digest")
	}
	return nil
}

// errUnsupported refuses a kind with no checkpoint format.
var errUnsupported = errors.New("checkpoint: unsupported native harness")

// setState stores a captured state in its kind's field. A new managed
// harness's state type needs a field in nativeArtifact, a case here and a
// line in state.
func (a *nativeArtifact) setState(state harness.ConversationState) error {
	switch s := state.(type) {
	case harness.CodexState:
		a.Codex = &s
	case harness.ClaudeState:
		a.Claude = &s
	case harness.CopilotState:
		a.Copilot = &s
	default:
		return errUnsupported
	}
	return nil
}

// state returns the artifact's one native state, refusing none, more than
// one, or one of another kind than Harness.
func (a nativeArtifact) state() (harness.ConversationState, error) {
	var states []harness.ConversationState
	if a.Codex != nil {
		states = append(states, *a.Codex)
	}
	if a.Claude != nil {
		states = append(states, *a.Claude)
	}
	if a.Copilot != nil {
		states = append(states, *a.Copilot)
	}
	if len(states) != 1 || states[0].Kind() != a.Harness {
		return nil, errors.New("checkpoint: inconsistent native harness state")
	}
	return states[0], nil
}

// native returns the artifact's state and its completed turn.
func (a nativeArtifact) native(maxBytes int64) (harness.ConversationState, string, error) {
	state, err := a.state()
	if err != nil {
		return nil, "", err
	}
	turn, err := state.CompletedTurn(maxBytes)
	return state, turn, err
}

func decodeArtifact(data []byte) (nativeArtifact, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nativeArtifact{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact nativeArtifact
	switch header.Version {
	case 1:
		var old codexArtifact
		if err := decoder.Decode(&old); err != nil {
			return artifact, err
		}
		artifact = nativeArtifact{Version: formatVersion, Harness: harness.NativeCodex, Codex: &old.Native, Workspace: old.Workspace}
	case formatVersion:
		if err := decoder.Decode(&artifact); err != nil {
			return artifact, err
		}
	default:
		return artifact, errors.New("checkpoint: unsupported artifact version")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return artifact, errors.New("checkpoint: trailing artifact data")
	}
	return artifact, nil
}

func inspect(data []byte, maxBytes int64) (Metadata, nativeArtifact, error) {
	if maxBytes <= 0 || len(data) == 0 || int64(len(data)) > maxBytes {
		return Metadata{}, nativeArtifact{}, errors.New("checkpoint: artifact exceeds byte limit")
	}
	artifact, err := decodeArtifact(data)
	if err != nil {
		return Metadata{}, artifact, fmt.Errorf("checkpoint: invalid artifact: %w", err)
	}
	if len(artifact.Workspace) == 0 {
		return Metadata{}, artifact, errors.New("checkpoint: incomplete workspace")
	}
	state, turn, err := artifact.native(maxBytes)
	if err != nil {
		return Metadata{}, artifact, err
	}
	digest := sha256.Sum256(data)
	workspaceMeta, err := workspace.Inspect(artifact.Workspace, maxBytes)
	if err != nil {
		return Metadata{}, artifact, err
	}
	return Metadata{Harness: artifact.Harness, ThreadID: state.Thread(), TurnID: turn,
		SHA256: hex.EncodeToString(digest[:]), Bytes: len(data), Workspace: workspaceMeta}, artifact, nil
}

// Restore publishes an inert placement directory containing "workspace" and
// the explicit native directory only after both restore operations succeed. Target must be absent and
// its parent exclusively owned by the trusted placement. The native launch must
// use target/workspace as explicit cwd and independently generated configuration
// in target/codex. Untrusted Git restore needs quota-backed staging/process limits
// in addition to maxBytes; see workspace.Restore.
func Restore(ctx context.Context, data []byte, target string, maxBytes int64) (Metadata, error) {
	meta, artifact, err := inspect(data, maxBytes)
	if err != nil {
		return Metadata{}, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return Metadata{}, err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return Metadata{}, errors.New("checkpoint: destination must not exist")
	}
	parent := filepath.Dir(target)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Metadata{}, errors.New("checkpoint: destination parent must be a real owned directory")
	}
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return Metadata{}, errors.New("checkpoint: destination parent must be a real owned directory")
	}
	parent = canonical
	target = filepath.Join(parent, filepath.Base(target))
	staging, err := os.MkdirTemp(parent, ".sproozi-checkpoint-")
	if err != nil {
		return Metadata{}, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// Trusted staging must not inherit a volume parent's fsGroup setgid mode.
	if err = os.Chmod(staging, 0700); err != nil {
		return Metadata{}, err
	}
	directory, err := harness.NativeDirectory(artifact.Harness)
	if err != nil {
		return Metadata{}, err
	}
	home := filepath.Join(staging, directory)
	if err = os.Mkdir(home, 0700); err != nil {
		return Metadata{}, err
	}
	if err = os.Chmod(home, 0700); err != nil {
		return Metadata{}, err
	}
	state, err := artifact.state()
	if err != nil {
		return Metadata{}, err
	}
	if err = state.Restore(home, filepath.Join(target, "workspace"), maxBytes); err != nil {
		return Metadata{}, err
	}
	if err = workspace.Restore(ctx, artifact.Workspace, filepath.Join(staging, "workspace"), maxBytes-int64(state.Size())); err != nil {
		return Metadata{}, err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return Metadata{}, errors.New("checkpoint: destination appeared during restore")
	}
	if err = os.Rename(staging, target); err != nil {
		return Metadata{}, err
	}
	return meta, nil
}

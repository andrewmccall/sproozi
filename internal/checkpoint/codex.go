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
	Version   int                  `json:"version"`
	Harness   harness.NativeKind   `json:"harness"`
	Codex     *harness.CodexState  `json:"codex,omitempty"`
	Claude    *harness.ClaudeState `json:"claude,omitempty"`
	Workspace []byte               `json:"workspace"`
}

// Metadata describes continuity data, never a placement's stop or usage receipt.
type Metadata struct {
	Harness  harness.NativeKind `json:"harness"`
	ThreadID string             `json:"threadId"`
	TurnID   string             `json:"turnId"`
	SHA256   string             `json:"sha256"`
	Bytes    int                `json:"bytes"`
}

// CaptureStopped must run after the placement independently confirms all
// native and workspace writers stopped. The native marker is a compatibility
// check, not that fence. Ignored files are included only by explicit selection.
func CaptureStopped(ctx context.Context, kind harness.NativeKind, home, repository, threadID string,
	selectedIgnored []string, maxBytes int64) ([]byte, Metadata, error) {
	artifact := nativeArtifact{Version: formatVersion, Harness: kind}
	switch kind {
	case harness.NativeCodex:
		native, err := harness.CaptureCodexState(home, threadID, maxBytes)
		if err != nil {
			return nil, Metadata{}, err
		}
		artifact.Codex = &native
	case harness.NativeClaude:
		native, err := harness.CaptureClaudeState(home, repository, threadID, maxBytes)
		if err != nil {
			return nil, Metadata{}, err
		}
		artifact.Claude = &native
	default:
		return nil, Metadata{}, errors.New("checkpoint: unsupported native harness")
	}
	_, _, size, err := artifact.native(maxBytes)
	if err != nil {
		return nil, Metadata{}, err
	}
	artifact.Workspace, err = workspace.Capture(ctx, repository, selectedIgnored, maxBytes-int64(size))
	if err != nil {
		return nil, Metadata{}, err
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		return nil, Metadata{}, err
	}
	meta, _, err := inspect(data, maxBytes)
	return data, meta, err
}

// Inspect validates native identity and candidate settled state, and reports the exact artifact
// digest. Workspace validation occurs during inert restore before publication.
func Inspect(data []byte, maxBytes int64) (Metadata, error) {
	meta, _, err := inspect(data, maxBytes)
	return meta, err
}

// ValidateContinuity checks the explicit selected native adapter. Codex also
// compares native instruction evidence; Claude instructions remain an independent
// trusted launch invariant. This supplies no writer fence or authority.
func ValidateContinuity(data []byte, kind harness.NativeKind, instructionDigest string, maxBytes int64) error {
	_, artifact, err := inspect(data, maxBytes)
	if err != nil {
		return err
	}
	if artifact.Harness != kind {
		return errors.New("checkpoint: native harness conflicts with trusted launch")
	}
	if artifact.Codex != nil {
		return harness.ValidateCodexContinuity(*artifact.Codex, instructionDigest, maxBytes)
	}
	// Claude has no native instruction evidence. The source/destination trusted
	// launch must independently hold approved instructions invariant and supply
	// them fresh; this check reports continuity compatibility only.
	digest, err := hex.DecodeString(instructionDigest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("checkpoint: invalid trusted instruction digest")
	}
	return nil
}

func (a nativeArtifact) native(maxBytes int64) (string, string, int, error) {
	switch a.Harness {
	case harness.NativeCodex:
		if a.Codex == nil || a.Claude != nil {
			break
		}
		turn, err := harness.CompletedCodexTurn(*a.Codex, maxBytes)
		return a.Codex.ThreadID, turn, len(a.Codex.Rollout), err
	case harness.NativeClaude:
		if a.Claude == nil || a.Codex != nil {
			break
		}
		leaf, err := harness.ClaudeTranscriptLeaf(*a.Claude, maxBytes)
		return a.Claude.ThreadID, leaf, len(a.Claude.Transcript), err
	}
	return "", "", 0, errors.New("checkpoint: inconsistent native harness state")
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
	id, turn, _, err := artifact.native(maxBytes)
	if err != nil {
		return Metadata{}, artifact, err
	}
	digest := sha256.Sum256(data)
	return Metadata{Harness: artifact.Harness, ThreadID: id, TurnID: turn, SHA256: hex.EncodeToString(digest[:]), Bytes: len(data)}, artifact, nil
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
	switch artifact.Harness {
	case harness.NativeCodex:
		err = harness.RestoreCodexState(home, *artifact.Codex, maxBytes)
	case harness.NativeClaude:
		err = harness.RestoreClaudeState(home, filepath.Join(target, "workspace"), *artifact.Claude, maxBytes)
	}
	if err != nil {
		return Metadata{}, err
	}
	_, _, nativeBytes, err := artifact.native(maxBytes)
	if err != nil {
		return Metadata{}, err
	}
	if err = workspace.Restore(ctx, artifact.Workspace, filepath.Join(staging, "workspace"), maxBytes-int64(nativeBytes)); err != nil {
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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const helperSocket = "/tmp/sproozi-checkpoint/control.sock"
const helperReceiptName = "restore.json"

// The helper's private directory and the published pair. Tests point them
// at temporary directories.
var (
	helperDirectory = "/tmp/sproozi-checkpoint"
	helperTarget    = "/workspace/session"
)

// Receipt stages. A restore writes restoring before it publishes the target
// and restored once the published pair is durable, so every interruption
// leaves a receipt that names how far it got.
const (
	stageRestoring = "restoring"
	stageRestored  = "restored"
)

// restoreReceipt is the helper's private record of the one artifact it
// restores. It cannot be supplied by the native worker or the checkpoint.
type restoreReceipt struct {
	Stage    string              `json:"stage"`
	Metadata checkpoint.Metadata `json:"metadata"`
}

// readReceipt reads the receipt; ok is false when there is none.
func readReceipt() (restoreReceipt, bool, error) {
	var r restoreReceipt
	data, err := privatefs.ReadFile(filepath.Join(helperDirectory, helperReceiptName), 64<<10)
	if os.IsNotExist(err) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.More() || (r.Stage != stageRestoring && r.Stage != stageRestored) {
		return r, false, errors.New("checkpoint helper restore receipt is unreadable")
	}
	return r, true, nil
}

// writeReceipt replaces the receipt atomically: a crash leaves the previous
// receipt or the new one, never part of either.
func writeReceipt(r restoreReceipt) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return privatefs.Replace(helperDirectory, helperReceiptName, data)
}

// requireRestored refuses capture before an acknowledged restore.
func requireRestored() error {
	r, ok, err := readReceipt()
	if err != nil {
		return err
	}
	if !ok || r.Stage != stageRestored {
		return errors.New("checkpoint helper has no acknowledged restore to capture")
	}
	return nil
}

// finishAllowed refuses to end the helper while a restore it started is
// unacknowledged; repeating prepare completes it. A helper that never
// restored may finish.
func finishAllowed() error {
	r, ok, err := readReceipt()
	if err != nil {
		return err
	}
	if ok && r.Stage == stageRestoring {
		return errors.New("checkpoint helper restore is unfinished; repeat prepare before finish")
	}
	return nil
}

// hold keeps the quota-backed volume mounted after the native container stops.
// Its private socket and receipt are never mounted into the native container.
func hold(deadline time.Time) error {
	if !deadline.After(time.Now()) {
		return errors.New("checkpoint helper deadline expired")
	}
	if err := os.Mkdir(helperDirectory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(helperDirectory, 0700); err != nil {
		return err
	}
	listener, err := net.Listen("unix", helperSocket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err = os.Chmod(helperSocket, 0600); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ctx, expire := context.WithCancelCause(ctx)
	defer expire(nil)
	go func() {
		select {
		case <-ctx.Done():
		case <-nativepty.AtDeadline(deadline, ctx.Done()):
			expire(context.DeadlineExceeded)
		}
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return err
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		data, readErr := io.ReadAll(io.LimitReader(conn, 8))
		if readErr == nil && bytes.Equal(data, []byte("finish\n")) {
			_, _ = conn.Write([]byte("finished\n"))
			_ = conn.Close()
			return nil
		}
		_ = conn.Close()
	}
}

func finish() error {
	if err := finishAllowed(); err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", helperSocket, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err = conn.Write([]byte("finish\n")); err != nil {
		return err
	}
	if err = conn.(*net.UnixConn).CloseWrite(); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(conn, 16))
	if err != nil || !bytes.Equal(data, []byte("finished\n")) {
		return fmt.Errorf("checkpoint helper finish was not acknowledged")
	}
	return nil
}

// prepare retains an exact restore acknowledgement in the helper's private
// filesystem. It cannot be supplied by the native worker or the checkpoint.
func prepare(ctx context.Context, data []byte, maxBytes int64, output io.Writer) error {
	meta, err := checkpoint.Inspect(data, maxBytes)
	if err != nil {
		return err
	}
	return restoreOnce(meta, "checkpoint helper restore intent conflicts", output, func() error {
		_, err := checkpoint.Restore(ctx, data, helperTarget, maxBytes)
		return err
	})
}

// restoreOnce publishes the pair for meta through publish at most once per
// helper, and writes meta to output once the pair is acknowledged. An
// identical retry of an acknowledged restore returns the same metadata; a
// different artifact conflicts. A retry of an unacknowledged one redoes it:
// the agent stays inert until prepare succeeds, so whatever is at the target
// is that restore's own unfinished work. A target with no receipt was not
// published by this helper and is refused.
func restoreOnce(meta checkpoint.Metadata, conflict string, output io.Writer, publish func() error) error {
	r, ok, err := readReceipt()
	if err != nil {
		return err
	}
	if ok && r.Metadata != meta {
		return errors.New(conflict)
	}
	switch {
	case ok && r.Stage == stageRestored:
		if err = validateRestoredPair(meta.Harness); err != nil {
			return err
		}
		return writeMetadata(meta, output)
	case ok:
		if err = os.RemoveAll(helperTarget); err != nil {
			return err
		}
	default:
		if _, err = os.Lstat(helperTarget); !os.IsNotExist(err) {
			return errors.New("checkpoint helper target exists without a restore receipt")
		}
		if err = writeReceipt(restoreReceipt{Stage: stageRestoring, Metadata: meta}); err != nil {
			return err
		}
	}
	if err = publish(); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Dir(helperTarget)); err != nil {
		return err
	}
	if err = writeReceipt(restoreReceipt{Stage: stageRestored, Metadata: meta}); err != nil {
		return err
	}
	return writeMetadata(meta, output)
}

// writeMetadata writes the restore acknowledgement the placement reads.
func writeMetadata(meta checkpoint.Metadata, output io.Writer) error {
	document, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = output.Write(document)
	return err
}

func syncDirectory(path string) error {
	parent, err := os.Open(path)
	if err != nil {
		return err
	}
	err = parent.Sync()
	closeErr := parent.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func validateRestoredPair(kind harness.NativeKind) error {
	directory, err := harness.NativeDirectory(kind)
	if err != nil {
		return err
	}
	paths := []string{helperTarget, filepath.Join(helperTarget, directory), filepath.Join(helperTarget, "workspace")}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return errors.New("checkpoint helper restored pair is missing or unsafe")
		}
	}
	return nil
}

func artifactDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func prepareWorkspace(ctx context.Context, kind harness.NativeKind,
	data []byte, maxBytes int64, output io.Writer) error {
	if len(data) == 0 || int64(len(data)) > maxBytes {
		return errors.New("initial workspace exceeds checkpoint limit")
	}
	directory, err := harness.NativeDirectory(kind)
	if err != nil {
		return err
	}
	meta := checkpoint.Metadata{Harness: kind, SHA256: artifactDigest(data), Bytes: len(data)}
	return restoreOnce(meta, "checkpoint helper initial workspace conflicts", output, func() error {
		return publishWorkspace(ctx, directory, data, maxBytes)
	})
}

// publishWorkspace stages the workspace beside an empty native home and
// publishes both together at the absent target.
func publishWorkspace(ctx context.Context, directory string, data []byte, maxBytes int64) error {
	staging, err := os.MkdirTemp(filepath.Dir(helperTarget), ".initial-session-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err = os.Chmod(staging, 0700); err != nil {
		return err
	}
	if err = workspace.Restore(ctx, data, filepath.Join(staging, "workspace"), maxBytes); err != nil {
		return err
	}
	if err = os.Mkdir(filepath.Join(staging, directory), 0700); err != nil {
		return err
	}
	if err = os.Chmod(filepath.Join(staging, directory), 0700); err != nil {
		return err
	}
	return os.Rename(staging, helperTarget)
}

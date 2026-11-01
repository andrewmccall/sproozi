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
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const helperDirectory = "/tmp/sproozi-checkpoint"
const helperSocket = helperDirectory + "/control.sock"
const helperReceipt = helperDirectory + "/restore.json"
const helperTarget = "/workspace/session"

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
	existing, err := os.ReadFile(helperReceipt)
	if err == nil {
		var observed checkpoint.Metadata
		if json.Unmarshal(existing, &observed) != nil || observed != meta {
			return errors.New("checkpoint helper restore intent conflicts")
		}
		if err = validateRestoredPair(meta.Harness); err != nil {
			return err
		}
		_, err = output.Write(existing)
		return err
	}
	if !os.IsNotExist(err) {
		return err
	}
	if _, err = checkpoint.Restore(ctx, data, helperTarget, maxBytes); err != nil {
		return err
	}
	return persistReceipt(meta, output)
}

func persistReceipt(meta checkpoint.Metadata, output io.Writer) error {
	if err := syncDirectory(filepath.Dir(helperTarget)); err != nil {
		return err
	}
	document, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(helperReceipt, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(document)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = syncDirectory(filepath.Dir(helperReceipt)); err != nil {
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
	if existing, err := os.ReadFile(helperReceipt); err == nil {
		var observed checkpoint.Metadata
		if json.Unmarshal(existing, &observed) != nil || observed != meta {
			return errors.New("checkpoint helper initial workspace conflicts")
		}
		if err = validateRestoredPair(meta.Harness); err != nil {
			return err
		}
		_, err = output.Write(existing)
		return err
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(helperTarget); !os.IsNotExist(err) {
		return errors.New("initial workspace target already exists")
	}
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
	if err = os.Rename(staging, helperTarget); err != nil {
		return err
	}
	return persistReceipt(meta, output)
}

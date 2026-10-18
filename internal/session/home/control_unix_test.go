//go:build linux || darwin

package home

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixControlRejectsUnsafePaths(t *testing.T) {
	address := controlAddress(t)
	if err := os.WriteFile(address, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenControl(address); err == nil {
		_ = listener.Close()
		t.Fatal("replaced regular control file")
	}
	data, err := os.ReadFile(address)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing control file changed: %q, %v", data, err)
	}
	if err = os.Remove(address); err != nil {
		t.Fatal(err)
	}
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err = os.Chmod(address, 0666); err != nil {
		t.Fatal(err)
	}
	if conn, err := dialControl(context.Background(), address); err == nil {
		_ = conn.Close()
		t.Fatal("connected to shared control socket")
	}
	if other, err := listenControl(address); err == nil {
		_ = other.Close()
		t.Fatal("replaced shared control socket")
	}
	if err = os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(address)
	if err = os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(parent, 0700) }()
	if conn, err := dialControl(context.Background(), address); err == nil {
		_ = conn.Close()
		t.Fatal("connected through shared control directory")
	}
}

func TestUnixControlReplacesPrivateStaleSocket(t *testing.T) {
	address := controlAddress(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err = os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	if err = stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
}

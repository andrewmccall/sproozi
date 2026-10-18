//go:build !windows

package main

import (
	"net"
	"os"
)

func listenTestControl(address string) (net.Listener, error) {
	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(address, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

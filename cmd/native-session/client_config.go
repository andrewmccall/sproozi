package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// installNativeClients writes only fresh trusted configuration outside the
// portable native-home/workspace pair. A resumed checkpoint supplies none of it.
func installNativeClients(c config, directory string) ([]string, error) {
	if c.ClientConfig == nil || !c.ClientConfig.Kubernetes {
		return nil, nil
	}
	trust := harness.NativeLocalTrustBundle
	if c.Gateway != nil {
		trust = c.Gateway.TrustBundle
	}
	path := filepath.Join(directory, "kubeconfig")
	data := []byte(harness.NativeKubeconfig(trust))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	if os.IsExist(err) {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
			return nil, errors.New("native Kubernetes client binding is not an immutable regular file")
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existing, data) {
			return nil, errors.New("native Kubernetes client binding changed")
		}
	} else if err != nil {
		return nil, errors.New("native Kubernetes client binding installation failed")
	} else {
		_, err = f.Write(data)
		if err = errors.Join(err, f.Close()); err != nil {
			return nil, err
		}
	}
	return []string{"KUBECONFIG=" + path}, nil
}

// The administrator admitted this fixed, isolated workspace. Write the native
// project's trust setting before launch so a scheduled task does not wait for
// a folder prompt. This fresh setting is excluded from portable native state.
func installCodexProjectTrust(home string) error {
	path := filepath.Join(home, "config.toml")
	data := []byte("[projects.\"" + workspace + "\"]\ntrust_level = \"trusted\"\n")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return errors.New("native project trust binding is not a regular file")
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existing, data) {
			return errors.New("native project trust binding changed")
		}
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

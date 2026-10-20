package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

const actionInit = "init"

func initialize(directory string, out io.Writer) error {
	if directory == "" {
		config, err := defaultConfigPath()
		if err != nil {
			return err
		}
		directory = filepath.Dir(config)
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if err = privatefs.EnsureDir(absolute); err != nil {
		return err
	}
	// The profile holds guidance, not credentials, so it keeps ordinary
	// permissions inside the private directory.
	if err = os.MkdirAll(filepath.Join(absolute, home.ProfileFolder, "skills"), 0755); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, absolute)
	return err
}

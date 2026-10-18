package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/andrewmccall/sproozi/internal/privatefs"
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
	_, err = fmt.Fprintln(out, absolute)
	return err
}

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

const actionDoctor = "doctor"

const (
	levelOK    = "ok"
	levelWarn  = "warn"
	levelError = "error"
)

// doctor checks the configuration file and the profile beside it. Every
// launch sends the profile as an archive, so it warns before the profile
// outgrows that limit and fails when any check fails.
func doctor(configPath string, out io.Writer) error {
	failed := false
	report := func(level, format string, args ...any) {
		failed = failed || level == levelError
		_, _ = fmt.Fprintf(out, "%-5s %s\n", level, fmt.Sprintf(format, args...))
	}
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	if _, err = os.Stat(absolute); errors.Is(err, fs.ErrNotExist) {
		report(levelWarn, "configuration %s does not exist; the home daemon needs one", absolute)
	} else if _, err = home.ReadConfig(absolute); err != nil {
		report(levelError, "configuration %s: %v", absolute, err)
	} else {
		report(levelOK, "configuration %s", absolute)
	}
	checkProfile(filepath.Join(filepath.Dir(absolute), home.ProfileFolder), report)
	if failed {
		return errors.New("doctor found errors")
	}
	return nil
}

func checkProfile(folder string, report func(level, format string, args ...any)) {
	if _, err := os.Stat(folder); errors.Is(err, fs.ErrNotExist) {
		report(levelWarn, "profile %s does not exist; run sproozi init to create it", folder)
		return
	}
	profile, err := agentprofile.LoadDirectory(folder)
	if err != nil {
		report(levelError, "profile %s: %v", folder, err)
		return
	}
	archive, _, err := profile.Archive()
	if err != nil {
		report(levelError, "profile %s cannot be sent to a session: %v", folder, err)
		return
	}
	names := make([]string, 0, len(profile.Skills))
	for _, s := range profile.Skills {
		names = append(names, s.Name)
	}
	summary := fmt.Sprintf("profile %s: %d bytes of instructions, %d skills [%s], archive %d of %d bytes",
		folder, len(profile.Instructions), len(names), strings.Join(names, ", "), len(archive), agentprofile.MaxArchiveBytes)
	if len(archive) > agentprofile.MaxArchiveBytes*3/4 {
		report(levelWarn, "%s; near the profile size limit", summary)
	} else {
		report(levelOK, "%s", summary)
	}
}

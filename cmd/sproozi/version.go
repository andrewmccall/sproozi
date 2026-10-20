package main

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
)

// Release builds set these with -ldflags. Source builds need no configuration:
// build reads the commit that go build records from the checkout.
var (
	version = sourceVersion
	commit  = unknownBuild
	date    = unknownBuild
)

const (
	sourceVersion = "dev"
	unknownBuild  = "unknown"
	// sourceBuildLine is the --version line when no release or commit is recorded.
	sourceBuildLine = "sproozi dev (source build; use the checkout you built from)"
)

// pseudoVersion matches the suffix Go gives an untagged commit, such as
// 0.1.1-0.20261019120000-75ebc11eabcd.
var pseudoVersion = regexp.MustCompile(`[0-9]{14}-[0-9a-f]{12}$`)

// buildInfo is the version, commit and date this binary reports.
type buildInfo struct{ version, commit, date string }

// build is what this binary reports and which documentation it links to.
var build = describeBuild(version, commit, date, readBuildInfo())

func readBuildInfo() *debug.BuildInfo {
	info, _ := debug.ReadBuildInfo()
	return info
}

// describeBuild fills what -ldflags left unset from the module and version
// control facts the Go toolchain records.
func describeBuild(v, c, d string, info *debug.BuildInfo) buildInfo {
	b := buildInfo{version: v, commit: c, date: d}
	if c != unknownBuild || info == nil {
		return b
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if revision := settings["vcs.revision"]; revision != "" {
		b.commit = revision
		if settings["vcs.modified"] == "true" {
			b.commit += " with uncommitted changes"
		}
		if t := settings["vcs.time"]; t != "" {
			b.date = t + " (commit time)"
		}
	}
	// go install MODULE@vX.Y.Z, or go build at a clean tagged checkout, records the release.
	if tagged := strings.TrimPrefix(info.Main.Version, "v"); v == sourceVersion && releaseVersion.MatchString(tagged) &&
		!pseudoVersion.MatchString(tagged) && !strings.Contains(tagged, "+") {
		b.version = tagged
	}
	return b
}

// String is the --version line. A build with neither a release nor a recorded
// commit says where its source is instead of printing unknowns.
func (b buildInfo) String() string {
	if b.version == sourceVersion && b.commit == unknownBuild {
		return sourceBuildLine
	}
	return fmt.Sprintf("sproozi %s (commit %s, built %s)", b.version, b.commit, b.date)
}

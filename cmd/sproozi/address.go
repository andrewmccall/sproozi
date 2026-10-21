package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

// parseSessionArguments accepts the session UUID as one positional argument,
// before or after the flags, or as --id.
func parseSessionArguments(c command, fs *flag.FlagSet, args []string, id *string) error {
	positional := ""
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return err
			}
			if name, unknown := strings.CutPrefix(err.Error(), "flag provided but not defined: -"); unknown {
				return fmt.Errorf("%s does not accept --%s; run \"sproozi %s -h\" for its flags", c.name, name, c.name)
			}
			return fmt.Errorf("%s: %w; run \"sproozi %s -h\" for its flags", c.name, err, c.name)
		}
		// Parsing stops at the first non-flag; resume after it so flags may follow.
		if fs.NArg() == 0 {
			break
		}
		if positional != "" || !c.accepts("id") {
			return fmt.Errorf("%s: unexpected argument %q; usage: %s", c.name, fs.Arg(0), c.line())
		}
		positional, args = fs.Arg(0), fs.Args()[1:]
	}
	if positional != "" {
		if *id != "" {
			return errors.New("choose a positional session UUID or --id")
		}
		*id = positional
	}
	return nil
}

// useDocker is the repair for every message that rejects a name addressing home.
const useDocker = "use --executor docker (home's executor name) or omit --executor"

// canonicalUUIDRule describes the session and operation UUID format.
const canonicalUUIDRule = "use a non-zero UUID in lowercase 8-4-4-4-12 hexadecimal form, " +
	"such as 3f2b8c1e-7d4a-4e5b-9c6d-0a1b2c3d4e5f"

// prepareAddress checks the owner and session choices and fills defaults before
// reading credentials or making a request. A selected remote owner never falls
// back home.
func (q *options) prepareAddress(c command) error {
	remote := q.executor != "" && q.executor != dockerExecutor && q.executor != home.ReservedHome
	switch {
	case c.owner == remoteOwner && q.executor == "":
		return fmt.Errorf("%s requires --executor NAME naming a remote executor configured under %s in %s; see %s",
			c.name, remoteKeys, q.configPath, remoteSetupDoc)
	case c.owner == remoteOwner && !remote:
		role := "docker is home's executor name"
		if q.executor == home.ReservedHome {
			role = "home names the home daemon, whose executor name is docker"
		}
		return fmt.Errorf("%s does not accept --executor %s: %s, and run submits only to "+
			"a remote executor configured under %s in %s; to work at home, use \"sproozi start --workspace PATH\", "+
			"or see %s to configure a remote executor", c.name, q.executor, role, remoteKeys, q.configPath, remoteSetupDoc)
	case q.executor == home.ReservedHome:
		return fmt.Errorf("executor %q names the home daemon, not an executor; %s", q.executor, useDocker)
	case c.owner == homeOwner && remote:
		return fmt.Errorf("%s requires the local home owner; %s", c.name, useDocker)
	}
	if q.id != "" && !work.CanonicalID(q.id) {
		hint := ""
		if c.name == home.ActionStart {
			hint = `; start takes the directory as "--workspace PATH"`
		}
		return fmt.Errorf("session UUID %q is not a canonical UUID: %s%s", q.id, canonicalUUIDRule, hint)
	}
	if q.id == "" && c.accepts("id") && c.name != home.ActionStart {
		return fmt.Errorf("%s requires a session UUID; usage: %s", c.name, c.line())
	}
	if q.operation != "" && !work.CanonicalID(q.operation) {
		return fmt.Errorf("--operation %q is not a canonical UUID: %s", q.operation, canonicalUUIDRule)
	}
	switch c.name {
	case home.ActionPull:
		return q.preparePull(remote)
	case home.ActionStart:
		if q.workspace == "" {
			return errors.New("start requires --workspace PATH")
		}
		if q.id == "" {
			q.id = uuid.NewString()
		}
		absolute, err := filepath.Abs(q.workspace)
		if err != nil {
			return err
		}
		q.workspace = absolute
	case home.ActionMove:
		if q.destination == "" {
			return errors.New("move requires --to NAME naming a configured placement: local.name or a name under \"remote\"")
		}
	}
	return nil
}

// preparePull resolves the remote executor a pull reads from.
func (q *options) preparePull(remote bool) error {
	if q.source == "" && remote {
		q.source = q.executor
	}
	if q.source == "" {
		return fmt.Errorf("pull requires --from NAME naming the remote executor to pull from, "+
			"configured under %s in %s", remoteKeys, q.configPath)
	}
	if remote && q.source != q.executor {
		return fmt.Errorf("pull --from %s conflicts with --executor %s; name the remote executor once, "+
			"with --from or --executor", q.source, q.executor)
	}
	// A plain retry repeats the same pull, so an interrupted import converges.
	if q.operation == "" {
		q.operation = uuid.NewSHA1(uuid.NameSpaceOID, []byte("sproozi/pull/"+q.source+"/"+q.id)).String()
	}
	return nil
}

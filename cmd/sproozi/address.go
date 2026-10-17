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

// parseSessionArguments accepts the session UUID as the first argument or --id.
func parseSessionArguments(c command, fs *flag.FlagSet, args []string, id *string) error {
	positional := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		if name, unknown := strings.CutPrefix(err.Error(), "flag provided but not defined: -"); unknown {
			return fmt.Errorf("%s does not accept --%s; run \"sproozi %s -h\" for its flags", c.name, name, c.name)
		}
		return fmt.Errorf("%s: %w; run \"sproozi %s -h\" for its flags", c.name, err, c.name)
	}
	if fs.NArg() != 0 || positional != "" && !c.accepts("id") {
		return fmt.Errorf("%s: unexpected argument; usage: %s", c.name, c.line())
	}
	if positional != "" {
		if *id != "" {
			return errors.New("choose a positional session UUID or --id")
		}
		*id = positional
	}
	return nil
}

// prepareAddress checks the owner and session choices and fills defaults before
// reading credentials or making a request. A selected remote owner never falls
// back home.
func (q *options) prepareAddress(c command) error {
	remote := q.executor != "" && q.executor != dockerExecutor
	switch {
	case c.owner == remoteOwner && !remote:
		return fmt.Errorf("%s requires an explicitly configured remote --executor", c.name)
	case c.owner == homeOwner && remote:
		return fmt.Errorf("%s requires the local home owner; use --executor docker or omit --executor", c.name)
	}
	if q.id != "" && !work.CanonicalID(q.id) {
		return errors.New("canonical stable session UUID required")
	}
	if q.id == "" && c.accepts("id") && c.name != home.ActionStart {
		return fmt.Errorf("%s requires a session UUID; usage: %s", c.name, c.line())
	}
	if q.operation != "" && !work.CanonicalID(q.operation) {
		return errors.New("canonical operation UUID required")
	}
	switch c.name {
	case home.ActionPull:
		if q.source == "" && remote {
			q.source = q.executor
		}
		if q.source == "" {
			return errors.New("pull requires --from configured-source")
		}
		if remote && q.source != q.executor {
			return errors.New("pull source conflicts with the selected executor; choose one configured source")
		}
		// A plain retry repeats the same pull, so an interrupted import converges.
		if q.operation == "" {
			q.operation = uuid.NewSHA1(uuid.NameSpaceOID, []byte("sproozi/pull/"+q.source+"/"+q.id)).String()
		}
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
			return errors.New("move requires --to configured-destination")
		}
	}
	return nil
}

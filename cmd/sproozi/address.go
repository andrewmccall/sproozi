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

// validateOptions resolves supported command/target choices before reading
// credentials or making a request. A selected remote owner never falls back home.
func validateOptions(action string, fs *flag.FlagSet, id, executor string) error {
	allowed := map[string]bool{"config": true, "socket": true, "executor": true}
	remote := executor != "" && executor != dockerExecutor
	switch action {
	case actionRun:
		if !remote || id != "" {
			return errors.New("run requires an explicitly configured remote --executor")
		}
		for _, option := range []string{
			"operation", "harness", "repository", "task", "task-file", "project",
			"workspace-from", "checkpoint-digest", "revision",
		} {
			allowed[option] = true
		}
	case home.ActionSessions:
		allowed["project"] = true
		if id != "" {
			return errors.New("sessions accepts no session UUID")
		}
	case home.ActionStart, home.ActionAttach, home.ActionMove, home.ActionResume, home.ActionStop:
		if remote {
			return fmt.Errorf("%s requires the local home owner; use --executor docker or omit --executor", action)
		}
		allowed["id"] = true
		if action != home.ActionAttach {
			allowed["operation"] = true
		}
		switch action {
		case home.ActionStart:
			allowed["workspace"], allowed["include-ignored"] = true, true
		case home.ActionMove:
			allowed["to"] = true
		case home.ActionResume:
			allowed["detach"] = true
		}
	case home.ActionPull:
		allowed["id"], allowed["operation"], allowed["from"] = true, true, true
	case home.ActionStatus, home.ActionCancel, home.ActionRecover:
		allowed["id"] = true
		if action == home.ActionCancel {
			allowed["operation"] = true
		}
	default:
		return errors.New("unsupported command; use " +
			"run|sessions|pull|resume|stop|serve|start|attach|move|status|cancel|recover")
	}
	var err error
	fs.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] && err == nil {
			err = fmt.Errorf("--%s is not supported by %s", f.Name, action)
		}
	})
	if err != nil {
		return err
	}
	if id != "" && !work.CanonicalID(id) {
		return errors.New("canonical stable session UUID required")
	}
	if id == "" && action != actionRun && action != home.ActionSessions && action != home.ActionStart {
		return errors.New("stable session UUID required")
	}
	return nil
}

func parseSessionArguments(fs *flag.FlagSet, args []string, id *string) error {
	positional := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if positional != "" {
		if *id != "" {
			return errors.New("choose a positional session UUID or --id")
		}
		*id = positional
	}
	return nil
}

func (q *options) prepareAddress(fs *flag.FlagSet) error {
	if err := validateOptions(q.action, fs, q.id, q.executor); err != nil {
		return err
	}
	if q.action == home.ActionPull {
		if q.source != "" && q.executor != "" && q.executor != dockerExecutor && q.source != q.executor {
			return errors.New("pull source conflicts with the selected executor; choose one configured source")
		}
		if q.source == "" && (q.executor == "" || q.executor == dockerExecutor) {
			return errors.New("pull requires --from configured-source")
		}
	}
	if q.action == home.ActionStart {
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
	}
	if q.action == home.ActionMove && q.destination == "" {
		return errors.New("move requires --to configured-destination")
	}
	if q.operation != "" && !work.CanonicalID(q.operation) {
		return errors.New("canonical operation UUID required")
	}
	return nil
}

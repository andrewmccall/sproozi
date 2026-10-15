package home

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session"
)

func validateProjectApprovals(c Config) error {
	if len(c.ProjectApprovals) > 64 {
		return errors.New("home: at most 64 source-qualified project approvals are supported")
	}
	seen := map[string]bool{}
	for _, approval := range c.ProjectApprovals {
		key := approval.Source + "/" + string(approval.Project)
		_, remote := c.Remote[approval.Source]
		_, source := c.WorkSources[approval.Source]
		if !project.ValidID(approval.Project) || !remote && !source || seen[key] {
			return fmt.Errorf("home: invalid, unconfigured or duplicate project approval %q", key)
		}
		seen[key] = true
		if _, err := project.Narrow(approval.Capabilities, c.Allowances); err != nil {
			return fmt.Errorf("home: project approval %q: %w", key, err)
		}
	}
	return nil
}

func (s *Service) projectAllowances(r localSession) (map[string]session.Allowance, error) {
	if r.Original.Project == "" {
		return maps.Clone(s.config.Allowances), nil
	}
	for _, approval := range s.config.ProjectApprovals {
		if approval.Source == r.Source && approval.Project == r.Original.Project {
			return project.Narrow(approval.Capabilities, s.config.Allowances)
		}
	}
	return nil, fmt.Errorf("home: configure projectApprovals for source %q and project %q before native resume",
		r.Source, r.Original.Project)
}

func cloneProjectApprovals(approvals []ProjectApproval) []ProjectApproval {
	out := slices.Clone(approvals)
	for i := range out {
		out[i].Capabilities = slices.Clone(approvals[i].Capabilities)
	}
	return out
}

// Check every path that can prepare or activate an imported Project execution.
// Cancellation and stop deliberately bypass this gate so revocation cannot
// strand a writer or its sealed accounting.
func (s *Service) executionProjectApproval(r initial) error {
	if r.ProductID == "" {
		return nil
	}
	owner, err := s.readCatalog(r.ProductID)
	if err != nil {
		return err
	}
	if owner.Original.Project == "" {
		return nil
	}
	ceilings, err := s.projectAllowances(owner)
	if err != nil {
		return err
	}
	for name, want := range r.Launch.Allowances {
		ceiling, ok := ceilings[name]
		if !ok || !projectLimitFits(want.Units, ceiling.Units) || !projectLimitFits(want.CostMicros, ceiling.CostMicros) {
			return fmt.Errorf("home: retained execution capability %q exceeds current Project approval; cancel it before a fresh resume", name)
		}
	}
	return nil
}

func projectLimitFits(want, ceiling session.Limit) bool {
	return !ceiling.Bounded || want.Bounded && want.Value <= ceiling.Value
}

// A retained stop or cancellation only fences/accounts; it needs no new grant.
func recoveryActivates(r initial, record session.Record) bool {
	if record.CancelOperation != "" {
		return false
	}
	if r.Destination != "" && !hasOperation(record, r.OperationID) {
		return true
	}
	for _, operation := range record.Operations {
		if operation.ID == record.PendingOperation {
			return !operation.Finalize
		}
	}
	return false
}

// Package project resolves administrator-defined project constraints.
// Associations describe history; only current local configuration grants access.
package project

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/session"
)

type ID string

const urnScheme = "urn"

var identifier = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func ValidID(id ID) bool { return identifier.MatchString(string(id)) }

// Definition references an existing independent template and optional workspace.
// Members are authenticated installation principals, not individual bot users.
type Definition struct {
	Template     string   `json:"template"`
	Workspace    string   `json:"workspace,omitempty"`
	Capabilities []string `json:"capabilities"`
	Members      []string `json:"members"`
}

// Association is immutable provenance from the authenticated owner catalogue.
// It contains no capability grant, credential or local filesystem path.
type Association struct {
	Project             ID     `json:"project,omitempty"`
	Template            string `json:"template,omitempty"`
	Repository          string `json:"repository,omitempty"`
	RepositoryReference string `json:"repositoryReference,omitempty"`
}

func Clone(definitions map[ID]Definition) map[ID]Definition {
	out := make(map[ID]Definition, len(definitions))
	for id, d := range definitions {
		d.Capabilities, d.Members = slices.Clone(d.Capabilities), slices.Clone(d.Members)
		out[id] = d
	}
	return out
}

// Validate enforces the fixed-template executor contract before any work starts.
func Validate(definitions map[ID]Definition, template string, repositories map[string]bool,
	ceilings map[string]session.Allowance) error {
	if len(definitions) > 64 {
		return errors.New("project: at most 64 definitions are supported")
	}
	for id, d := range definitions {
		if !ValidID(id) || template == "" || d.Template != template {
			return fmt.Errorf("project %q: reference this executor's fixed template %q; use another executor for another template", id, template)
		}
		if d.Workspace != "" && !repositories[d.Workspace] {
			return fmt.Errorf("project %q: workspace repository is not configured", id)
		}
		if _, err := Narrow(d.Capabilities, ceilings); err != nil {
			return fmt.Errorf("project %q: %w", id, err)
		}
		if len(d.Members) == 0 || len(d.Members) > 64 {
			return fmt.Errorf("project %q: explicit installation principal members required", id)
		}
		seen := map[string]bool{}
		for _, member := range d.Members {
			if member == "" || strings.TrimSpace(member) != member || strings.ContainsAny(member, "\x00\r\n") ||
				len(member) > 128 || seen[member] {
				return fmt.Errorf("project %q: invalid or duplicate member", id)
			}
			seen[member] = true
		}
	}
	return nil
}

func Member(definitions map[ID]Definition, id ID, principal string) bool {
	if id == "" {
		return true
	}
	d, ok := definitions[id]
	return ok && slices.Contains(d.Members, principal)
}

// Narrow copies a subset of current ceilings without changing any numeric limit.
func Narrow(capabilities []string, ceilings map[string]session.Allowance) (map[string]session.Allowance, error) {
	if len(capabilities) == 0 || len(capabilities) > 32 {
		return nil, errors.New("project: explicit capability subset required")
	}
	out := make(map[string]session.Allowance, len(capabilities))
	for _, name := range capabilities {
		a, ok := ceilings[name]
		if _, duplicate := out[name]; !ok || duplicate {
			return nil, fmt.Errorf("project: capability %q is unavailable or duplicated", name)
		}
		out[name] = a
	}
	if _, ok := out["model.inference"]; !ok {
		return nil, errors.New("project: model.inference is required for native work")
	}
	return out, nil
}

// ValidateReference accepts portable repository identity, never credential URLs.
// An identity is descriptive and is not used as an external destination.
func ValidateReference(reference string) error {
	if reference == "" {
		return nil
	}
	u, err := url.Parse(reference)
	if err != nil || len(reference) > 2048 || strings.ContainsAny(reference, "\x00\r\n") ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		(u.Scheme != "https" && u.Scheme != "ssh" && u.Scheme != urnScheme) ||
		(u.Scheme == urnScheme && u.Opaque == "" || u.Scheme != urnScheme && u.Host == "") {
		return errors.New("project: repository reference must be a credential-free HTTPS, SSH or URN identity")
	}
	return nil
}

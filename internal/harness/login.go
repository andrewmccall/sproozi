package harness

import (
	"fmt"
	"regexp"
)

// DefaultLogin is the login a session signs in with when none is named.
const DefaultLogin = "default"

// LoginMaxLength keeps a login name short enough to fit a Docker volume name
// and a Kubernetes claim name with its harness, such as
// sproozi-login-claude-code-<name>.
const LoginMaxLength = 24

var loginName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateLogin checks a named login: lowercase letters, digits and hyphens,
// starting and ending with a letter or digit, at most LoginMaxLength long.
// Each harness has its own logins, so codex/default and copilot/default are
// separate stores (ADR 0049).
func ValidateLogin(name string) error {
	if len(name) > LoginMaxLength || !loginName.MatchString(name) {
		return fmt.Errorf("login name %q must be 1 to %d lowercase letters, digits and hyphens, "+
			"starting and ending with a letter or digit", name, LoginMaxLength)
	}
	return nil
}

// LoginOrDefault is name, or DefaultLogin when it is empty.
func LoginOrDefault(name string) string {
	if name == "" {
		return DefaultLogin
	}
	return name
}

package workspace

import "fmt"

// ValidateIgnoredPaths checks the bounded explicit file selection independently
// of a filesystem. Capture also checks each selected file and its ancestry.
// Empty and nil selections both mean no ignored files. Order remains immutable
// session intent; duplicate paths are rejected rather than silently broadened.
func ValidateIgnoredPaths(paths []string) error {
	if len(paths) > 32 {
		return fmt.Errorf("workspace: too many ignored file selections")
	}
	seen := make(map[string]bool, len(paths))
	for _, name := range paths {
		switch {
		case seen[name]:
			return fmt.Errorf("workspace: ignored file %q is selected twice", name)
		case len(name) > 512 || !validPath(name):
			return fmt.Errorf("workspace: ignored file %q must be a path inside the repository, relative to its "+
				"top level, with no \"..\" or .git", name)
		}
		seen[name] = true
	}
	return nil
}

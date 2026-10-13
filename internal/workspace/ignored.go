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
		if len(name) > 512 || !validPath(name) || seen[name] {
			return fmt.Errorf("workspace: invalid ignored file selection %q", name)
		}
		seen[name] = true
	}
	return nil
}

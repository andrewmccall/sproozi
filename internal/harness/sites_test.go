package harness

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// harnessSites are the files outside the registry that may still branch on a
// managed kind, name a selector or repeat a registry value, with how many such
// lines each has and why. Each is behaviour, which stays in code (ADR 0039), or
// a value that only looks like a harness fact. A new line elsewhere fails
// TestNoHarnessFactOutsideTheRegistry.
var harnessSites = map[string]struct {
	lines int
	why   string
}{
	"internal/harness/native_kind.go": {3, "defines the managed kind constants"},
	"internal/harness/native_launch.go": {3,
		"defines the OpenAI and Anthropic key variables and Copilot's token placeholder that registry entries name"},
	"internal/agentprofile/agentprofile.go": {1, "the profile's own instructions file, for every harness"},
	"internal/harness/native_args.go":       {3, "parses Codex's -c/--config overrides and -m: behaviour"},
	"internal/checkpoint/artifact.go":       {9, "captures, checks and restores each native state format: behaviour"},
	"internal/nativeconfig/claude.go": {3, "api imports nativeconfig, so it cannot read the registry; " +
		"TestManagedKindsAreWired keeps it equal to the managed tier"},
	"internal/localsetup/credentials.go":     {1, "names GitHub's Copilot model provider, not the harness"},
	"internal/endpoints/model/copilot.go":    {1, "names GitHub's Copilot model provider, not the harness"},
	"internal/runtime/kubernetes/managed.go": {1, "an AgentRun's OpenAI placeholder key, whatever its harness"},
	"cmd/gateway/main.go":                    {4, "the gateway's own provider key variables"},
	"cmd/native-session/launch.go":           {3, "the supervisor adapter of each managed kind: behaviour"},
	"cmd/native-session/main.go":             {2, "graceful-stop wait text from each kind's turn observation: behaviour"},
	"cmd/native-session/claude.go":           {1, "Claude Code's lifecycle hooks: behaviour"},
	"cmd/native-session/copilot.go":          {1, "Copilot's unpacked app cache directory"},
}

// harnessSitePatterns match a line that branches on a managed kind (==, !=,
// case or a map literal key), names a registered selector, or repeats a
// registered credential variable, placeholder, instructions file or
// executable.
func harnessSitePatterns() []*regexp.Regexp {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(==|!=)\s*(harness\.)?Native(Codex|Claude|Copilot)\b`),
		regexp.MustCompile(`\bNative(Codex|Claude|Copilot)\s*(==|!=)`),
		regexp.MustCompile(`\bcase\s+(harness\.)?Native(Codex|Claude|Copilot)\b`),
		regexp.MustCompile(`^\s*(harness\.)?Native(Codex|Claude|Copilot)\s*:`),
	}
	literals := slices.Clone(Selectors())
	for _, h := range registry {
		for _, value := range []string{h.KeyEnv, h.TokenEnv, h.TokenPlaceholder, h.InstructionsFile, h.Executable} {
			if value != "" && !slices.Contains(literals, value) {
				literals = append(literals, value)
			}
		}
	}
	quoted := make([]string, 0, len(literals))
	for _, l := range literals {
		quoted = append(quoted, regexp.QuoteMeta(l))
	}
	return append(patterns, regexp.MustCompile("[\"`]("+strings.Join(quoted, "|")+")[\"`=]"))
}

// harnessSiteLines lists, by file, the lines under internal, cmd and api,
// outside the registry and tests, that harnessSitePatterns match. Comments
// are skipped. Packages under hack/ are verifiers, which list harnesses by
// design (ADR 0039).
func harnessSiteLines(t *testing.T) map[string][]string {
	t.Helper()
	patterns := harnessSitePatterns()
	found := map[string][]string{}
	for _, root := range []string{"internal", "cmd", "api"} {
		err := filepath.WalkDir("../../"+root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.HasPrefix(d.Name(), "zz_generated") {
				return err
			}
			name := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if name == registryFile {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				if slices.ContainsFunc(patterns, func(p *regexp.Regexp) bool { return p.MatchString(line) }) {
					found[name] = append(found[name], fmt.Sprintf("%s:%d: %s", name, i+1, strings.TrimSpace(line)))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

// The per-harness facts live in the registry: no file outside it branches on a
// managed kind, names a selector or repeats a registry value, except the
// behaviour sites in harnessSites.
func TestNoHarnessFactOutsideTheRegistry(t *testing.T) {
	found := harnessSiteLines(t)
	for name, lines := range found {
		site, ok := harnessSites[name]
		switch {
		case !ok:
			t.Errorf("%s branches on a harness or repeats a registry value outside %s:\n  %s\nread the fact from "+
				"the harness's registry entry, adding a field if it has none (docs/contributing/add-a-harness.md); "+
				"only behaviour may stay, listed in harnessSites in internal/harness/sites_test.go",
				name, registryFile, strings.Join(lines, "\n  "))
		case len(lines) != site.lines:
			t.Errorf("%s has %d harness-specific lines, but harnessSites allows %d (%s):\n  %s\nread a fact from "+
				"the registry instead, or correct the count when behaviour changed", name, len(lines), site.lines,
				site.why, strings.Join(lines, "\n  "))
		}
	}
	for name := range harnessSites {
		if _, ok := found[name]; !ok {
			t.Errorf("%s no longer has harness-specific lines; remove it from harnessSites", name)
		}
	}
}

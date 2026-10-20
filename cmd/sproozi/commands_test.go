//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func helpFlags(t *testing.T, text string) []string {
	t.Helper()
	var names []string
	for line := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "  --"); ok {
			names = append(names, strings.Fields(rest)[0])
		}
	}
	slices.Sort(names)
	return names
}

func TestCommandHelpListsOnlyItsFlagsAndSucceeds(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli([]string{"pull", "-h"}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("pull -h exited %d: %s", code, stderr.String())
	}
	want := []string{"config", "executor", "from", "id", "operation", "socket"}
	if got := helpFlags(t, stdout.String()); !slices.Equal(got, want) ||
		!strings.HasPrefix(stdout.String(), "Usage: sproozi pull SESSION_UUID --from NAME [flags]") {
		t.Fatalf("pull help listed %v:\n%s", got, stdout.String())
	}
}

func TestBareCommandPrintsUsageAndFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cli(nil, &stdout, &stderr); code != 2 || stdout.Len() != 0 ||
		!strings.HasPrefix(stderr.String(), "Usage: sproozi COMMAND [flags]") {
		t.Fatalf("bare sproozi exited %d: %q %q", code, stdout.String(), stderr.String())
	}
}

func TestHelpNamesValuesAndPlacements(t *testing.T) {
	for args, want := range map[string]string{
		"run":  "  --harness NAME\n",
		"move": "placement NAME: the value of local.name (for example local) or a name under \"remote\"",
	} {
		var stdout, stderr bytes.Buffer
		if code := cli([]string{args, "-h"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), want) {
			t.Fatalf("%s -h exited %d without %q:\n%s", args, code, want, stdout.String())
		}
	}
}

func TestTopLevelHelpListsEveryCommand(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if code := cli(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr.String())
		}
		for _, c := range commands {
			if !strings.Contains(stdout.String(), c.name) || !strings.Contains(stdout.String(), c.summary) {
				t.Fatalf("%v omitted %s:\n%s", args, c.name, stdout.String())
			}
		}
	}
}

func TestUnsupportedFlagOrCommandPointsToHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"pull", uuid.NewString(), "--task", "x"}, `pull does not accept --task; run "sproozi pull -h"`},
		{[]string{"bogus"}, `unknown command "bogus"; run "sproozi -h"`},
		{[]string{"doctor"}, `unknown command "doctor"; run "sproozi -h" for commands. The setup check is ` +
			`"python3 hack/verify/quickstart.py doctor" in a repository checkout, see ` +
			docURL("docs/guides/getting-started.md")},
	} {
		var stdout, stderr bytes.Buffer
		if code := cli(tc.args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
			t.Fatalf("%v exited %d: %q", tc.args, code, stderr.String())
		}
	}
}

// The command reference's flag table must match what each command registers.
func TestCommandReferenceListsEachCommandsFlags(t *testing.T) {
	data, err := os.ReadFile("../../docs/reference/session-commands.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	_, section, _ := strings.Cut(text, "\n## Command flags\n")
	section, _, _ = strings.Cut(section, "\n## ")
	row := regexp.MustCompile("(?m)^\\| `([a-z]+)` \\| (.*) \\|$")
	flagName := regexp.MustCompile("`--([a-z-]+)`")
	documented := map[string]bool{}
	for _, match := range row.FindAllStringSubmatch(section, -1) {
		documented[match[1]] = true
		c, ok := findCommand(match[1])
		if !ok {
			t.Fatalf("Reference documents unknown command %s", match[1])
		}
		var got []string
		for _, f := range flagName.FindAllStringSubmatch(match[2], -1) {
			got = append(got, f[1])
		}
		if !slices.Equal(got, c.flags) {
			t.Errorf("Reference lists %s flags %v; command registers %v", c.name, got, c.flags)
		}
	}
	for _, c := range commands {
		if !documented[c.name] {
			t.Errorf("Reference omits %s", c.name)
		}
	}
	for name := range flagHelp {
		if !strings.Contains(section, "| `--"+name+" ") && !strings.Contains(section, ", `--"+name+" ") &&
			!strings.Contains(section, "| `--"+name+"` |") {
			t.Errorf("Reference does not describe --%s", name)
		}
	}
}

package home

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// readTestConfig writes a client configuration with the given extra members and reads it.
func readTestConfig(t *testing.T, extra string) (Config, error) {
	t.Helper()
	dir := testutil.PrivateDir(t)
	path := filepath.Join(dir, "config.json")
	data := `{"directory":` + strconv.Quote(dir) + `,"model":"fixture","local":{"name":"local"},"maxDuration":"1h",
 "allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}` + extra + `}`
	if err := testutil.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return ReadConfig(path)
}

func TestReadConfigChecksLocalNameAndRequiredKeysByPath(t *testing.T) {
	t.Parallel()
	for extra, want := range map[string]string{
		`,"local":{"name":"docker"}`:    `local.name: "docker" is reserved: ` + ReservedNames + "; choose another name",
		`,"local":{"name":"home"}`:      `local.name: "home" is reserved: ` + ReservedNames + "; choose another name",
		`,"local":{"name":"Local Box"}`: `local.name: "Local Box" is not a valid name; use 1 to 63 lowercase letters`,
		`,"local":{"name":""},"model":""`: "local.name: required; use 1 to 63 " +
			"lowercase letters, digits and hyphens, starting and ending with a letter or digit",
	} {
		_, err := readTestConfig(t, extra)
		var invalid *InvalidConfigError
		if !errors.As(err, &invalid) || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v, want %q", extra, err, want)
		}
	}
}

func TestReadConfigNamesUnknownKeysByPath(t *testing.T) {
	t.Parallel()
	for extra, want := range map[string]string{
		`,"bogus":1`: `unknown key "bogus"`,
		`,"remote":{"engineering":{"url":"https://a.example"}}`: `unknown key "remote": remote executors are no longer ` +
			`configured here; remove it and reach the cluster with --context (docs/reference/remote-executors.md)`,
		`,"local":{"name":"local","agentLimits":{"bogus":1}}`: `local.agentLimits: unknown key "bogus"`,
		`,"projectApprovals":[{"source":"a"},{"bogus":1}]`:    `projectApprovals[1]: unknown key "bogus"`,
		`,"gateway":{"Bogus":1}`:                              `gateway: unknown key "Bogus"`,
	} {
		if _, err := readTestConfig(t, extra); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", extra, err, want)
		}
	}
}

func TestReadConfigNamesTypeErrorsByKey(t *testing.T) {
	t.Parallel()
	for extra, want := range map[string]string{
		`,"dockerBinary":1`: "dockerBinary: number is not a string",
	} {
		if _, err := readTestConfig(t, extra); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", extra, err, want)
		}
	}
	dir := testutil.PrivateDir(t)
	path := filepath.Join(dir, "config.json")
	for data, want := range map[string]string{
		`{"local":{"GatewayPort":-1}}`:                    "local.gatewayPort: number -1 is not a whole number from 0 to 65535",
		`{"local":{"agentLimits":{"MemoryBytes":"x"}}}`:   "local.agentLimits.MemoryBytes: string is not a whole number",
		`{"allowances":{"model.inference":{"units":[]}}}`: "allowances.model.inference.units: array is not an object",
	} {
		if err := testutil.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(path); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", data, err, want)
		}
	}
}

func TestAllowanceProblemsNameEachKey(t *testing.T) {
	t.Parallel()
	c := Config{Allowances: map[string]session.Allowance{
		testCapability:        {Units: session.Limit{Bounded: true, Value: -1}, CostMicros: session.Limit{Value: 5}},
		"mcp.docs":            {Units: session.Limit{Bounded: true, Value: 1}},
		"github.pull_request": {Units: session.Limit{Bounded: true, Value: 1}},
	}}
	problems := c.AllowanceProblems()
	keys := make([]string, 0, len(problems))
	for _, problem := range problems {
		keys = append(keys, strings.Fields(problem)[0])
	}
	want := []string{"allowances.github.pull_request", "allowances.mcp.docs", "allowances.model.inference.units",
		"allowances.model.inference.costMicros"}
	if !slices.Equal(keys, want) {
		t.Fatalf("allowance problems %v, want %v", keys, want)
	}
	if problems := composedConfig(t).AllowanceProblems(); len(problems) != 0 {
		t.Fatalf("valid allowances rejected: %v", problems)
	}
}

// Model inference may go without a unit limit; every other capability needs
// one (ADR 0051).
func TestOnlyModelInferenceMayRunWithoutAUnitLimit(t *testing.T) {
	t.Parallel()
	c := composedConfig(t)
	c.Allowances = maps.Clone(c.Allowances)
	c.Allowances[testCapability] = session.Allowance{}
	if problems := c.AllowanceProblems(); len(problems) != 0 {
		t.Fatalf("a model allowance without limits was rejected: %v", problems)
	}
	if _, err := approvedPolicy(c); err != nil {
		t.Fatalf("policy without a model limit: %v", err)
	}
	c.Allowances["mcp.docs"] = session.Allowance{}
	if problems := c.AllowanceProblems(); len(problems) == 0 || !strings.HasPrefix(problems[0], "allowances.mcp.docs.units") {
		t.Fatalf("an MCP allowance without a unit limit: %v", problems)
	}
}

func TestReadConfigExplainsTrailingContent(t *testing.T) {
	t.Parallel()
	dir := testutil.PrivateDir(t)
	path := filepath.Join(dir, "config.json")
	object := `{"directory":` + strconv.Quote(dir) + `}`
	if err := testutil.WriteFile(path, []byte(object+"\n}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadConfig(path)
	want := fmt.Sprintf(`unexpected content after the closing "}" that ends at byte %d; `+
		"the file must hold exactly one JSON object", len(object))
	var invalid *InvalidConfigError
	if !errors.As(err, &invalid) || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

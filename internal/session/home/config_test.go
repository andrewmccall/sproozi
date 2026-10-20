package home

import (
	"errors"
	"fmt"
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

func TestReadConfigNamesEachInvalidConnectionKey(t *testing.T) {
	t.Parallel()
	token := strconv.Quote(filepath.Join(testutil.PrivateDir(t), "token"))
	for _, tc := range []struct {
		extra string
		want  []string
	}{
		{`,"remote":{"engineering":{}}`, []string{"remote.engineering.url: ", "remote.engineering.tokenFile: ",
			"remote.engineering.maxCheckpointBytes: "}},
		{`,"workSources":{"tasks":{"url":"http://tasks.example","tokenFile":` + token + `,"maxCheckpointBytes":1}}`,
			[]string{"workSources.tasks.url: fixed HTTPS origin"}},
		{`,"remote":{"docker":{}}`, []string{`remote.docker: "docker" is reserved: ` + ReservedNames + "; choose another name"}},
		{`,"workSources":{"local":{}}`, []string{`workSources.local: "local" is local.name`}},
		{`,"remote":{"home":{}}`, []string{`remote.home: "home" is reserved: ` + ReservedNames + "; choose another name"}},
		{`,"remote":{"Bad Name":{}}`, []string{`remote.Bad Name: "Bad Name" is not a valid name; use 1 to 63`}},
		{`,"workSources":{"../x":{}}`, []string{`workSources.../x: "../x" is not a valid name`}},
		{`,"remote":{"big":{"maxCheckpointBytes":104857600}}`, []string{
			"remote.big.maxCheckpointBytes: 104857600 is out of range; 1 byte to 64 MiB, equal to local.maxCheckpointBytes"}},
		{`,"local":{"name":"local","maxCheckpointBytes":8388608},"remote":{"engineering":{"maxCheckpointBytes":4194304}}`,
			[]string{"remote.engineering.maxCheckpointBytes: 4194304 differs from local.maxCheckpointBytes (8388608)"}},
		{`,"remote":{"-a":{},"a-":{}}`, []string{`remote.-a: "-a" is not a valid`, `remote.a-: "a-" is not a valid`}},
	} {
		_, err := readTestConfig(t, tc.extra)
		for _, want := range tc.want {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v lacks %q", tc.extra, err, want)
			}
		}
	}
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
		`,"remote":{"a":{"url":"https://a.example"},"engineering":{"bogus":1}}`: `remote.engineering: unknown key "bogus"`,
		`,"local":{"name":"local","agentLimits":{"bogus":1}}`:                   `local.agentLimits: unknown key "bogus"`,
		`,"projectApprovals":[{"source":"a"},{"bogus":1}]`:                      `projectApprovals[1]: unknown key "bogus"`,
		`,"gateway":{"Bogus":1}`:                                                `gateway: unknown key "Bogus"`,
	} {
		if _, err := readTestConfig(t, extra); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", extra, err, want)
		}
	}
}

func TestReadConfigAcceptsCamelCaseAndLegacyConnectionKeys(t *testing.T) {
	t.Parallel()
	token := filepath.Join(testutil.PrivateDir(t), "token")
	for _, entry := range []string{
		`{"url":"https://profile.example:8443","tokenFile":` + strconv.Quote(token) + `,"maxCheckpointBytes":8388608}`,
		`{"URL":"https://profile.example:8443","TokenFile":` + strconv.Quote(token) + `,"MaxCheckpointBytes":8388608}`,
	} {
		c, err := readTestConfig(t, `,"remote":{"engineering":`+entry+`}`)
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		if got := c.Remote["engineering"]; got.URL != "https://profile.example:8443" || got.TokenFile != token ||
			got.MaxCheckpointBytes != 8<<20 {
			t.Fatalf("%s decoded as %+v", entry, got)
		}
	}
}

func TestReadConfigNamesTypeErrorsByKey(t *testing.T) {
	t.Parallel()
	for extra, want := range map[string]string{
		`,"dockerBinary":1`: "dockerBinary: number is not a string",
		`,"remote":{"a":{"url":"https://a.example"},"engineering":{"MaxCheckpointBytes":"x"}}`: "remote.engineering.maxCheckpointBytes: string is not a whole number",
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

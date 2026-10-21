package home

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"
	budgetfile "github.com/andrewmccall/sproozi/internal/budget/file"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

func TestServeProblemsAcceptComposedConfigAndNameEveryGap(t *testing.T) {
	if problems := composedConfig(t).ServeProblems(); len(problems) != 0 {
		t.Fatalf("composed configuration rejected: %v", problems)
	}
	starter := Config{Directory: "/private/home", Model: "fixture", MaxDuration: "30m",
		Local: runtimedocker.PlacementConfig{Harness: harness.NativeCodex, Name: "local"}}
	problems := starter.ServeProblems()
	keys := make([]string, 0, len(problems))
	for _, problem := range problems {
		keys = append(keys, strings.Fields(problem)[0])
	}
	for _, key := range []string{"dockerBinary", "dockerSocket", "dockerConfig", "local.nativeImageID",
		"local.helperImageID", "local.anchorImageID", "local.networkName", "local.gatewayIPv4", "local.gatewayPort",
		"local.instructions", "local.agentLimits", "local.volumeBytes", "local.maxCheckpointBytes",
		"gateway.listen", "gateway.certificateFile", "gateway.keyFile", "gateway.trustBundleFile",
		"gateway.providerURL", "gateway.providerTokenFile"} {
		if !slices.Contains(keys, key) {
			t.Errorf("starter configuration problems omit %s: %v", key, keys)
		}
	}
	if slices.Contains(keys, "local.harness") || slices.Contains(keys, "local.name") || slices.Contains(keys, "model") {
		t.Errorf("valid starter settings reported: %v", keys)
	}
	// An omitted model lets the harness use its own default.
	starter.Model = ""
	for _, problem := range starter.ServeProblems() {
		if strings.HasPrefix(problem, "model ") {
			t.Errorf("omitted model reported: %v", problem)
		}
	}
}

// Whitespace is as missing as an empty value for required text settings.
func TestServeProblemsTreatBlankTextAsMissing(t *testing.T) {
	c := composedConfig(t)
	c.Model, c.Local.Instructions = "   ", "\t\n"
	problems := c.ServeProblems()
	for _, want := range []string{"model (" + modelWant + ")", "local.instructions (approved instructions)"} {
		if !slices.Contains(problems, want) {
			t.Errorf("blank setting not reported as %q: %v", want, problems)
		}
	}
}

// A provider token file still holding the setup guide's placeholder is named
// before serve starts, and Open refuses it with the same file.
func TestProviderTokenPlaceholderIsRefused(t *testing.T) {
	c := composedConfig(t)
	if problem := c.ProviderTokenProblem(); problem != "" {
		t.Fatalf("real token reported: %s", problem)
	}
	if err := testutil.WriteFile(c.Gateway.ProviderTokenFile, []byte("REPLACE_WITH_PROVIDER_API_KEY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	want := "gateway.providerTokenFile (replace REPLACE_WITH_PROVIDER_API_KEY in " + c.Gateway.ProviderTokenFile +
		" with the provider API key)"
	if problem := c.ProviderTokenProblem(); problem != want {
		t.Fatalf("got %q, want %q", problem, want)
	}
	_, err := Open(c, io.Discard)
	if err == nil || err.Error() != "home: gateway.providerTokenFile "+c.Gateway.ProviderTokenFile+
		" still holds the placeholder REPLACE_WITH_PROVIDER_API_KEY; write the provider API key there" {
		t.Fatalf("Open: %v", err)
	}
}

// An empty provider token file is named with its path before serve starts and
// by Open, not reported as a bare invalid credential.
func TestEmptyProviderTokenFileIsNamed(t *testing.T) {
	c := composedConfig(t)
	if err := testutil.WriteFile(c.Gateway.ProviderTokenFile, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := c.ProviderTokenProblem(), "gateway.providerTokenFile ("+c.Gateway.ProviderTokenFile+
		" is empty; write the provider API key there)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	_, err := Open(c, io.Discard)
	if want := "home: gateway.providerTokenFile " + c.Gateway.ProviderTokenFile +
		" is empty; write the provider API key there"; err == nil || err.Error() != want {
		t.Fatalf("Open: %v, want %q", err, want)
	}
}

// gateway.maxModelCallSeconds may lower or raise the 1200 s default within
// the gateway's one-hour limit; omitting it keeps the default, and 0 or a
// value that would overflow a duration is refused.
func TestServeProblemsBoundMaxModelCallSeconds(t *testing.T) {
	want := "gateway.maxModelCallSeconds (1 to 3600 seconds; omit it for 1200)"
	if slices.Contains(composedConfig(t).ServeProblems(), want) {
		t.Error("omitted maxModelCallSeconds reported")
	}
	for seconds, bad := range map[int]bool{1: false, 3600: false, 0: true, -1: true, 3601: true,
		9_223_372_046: true, 1 << 62: true} {
		c := composedConfig(t)
		value := CallSeconds(seconds)
		c.Gateway.MaxModelCallSeconds = &value
		if got := slices.Contains(c.ServeProblems(), want); got != bad {
			t.Errorf("maxModelCallSeconds %d: reported %v, want %v: %v", seconds, got, bad, c.ServeProblems())
		}
	}
}

// Closing the daemon drains model calls while the ledger can still record
// their settlements.
func TestCloseDrainsModelCalls(t *testing.T) {
	drain := model.NewDrain()
	s := &Service{drain: drain}
	_ = s.Close()
	if !drain.Stopping() {
		t.Fatal("Close did not drain model calls")
	}
}

// Any integral JSON number decodes, so a huge one is reported as out of range
// by ServeProblems; only a fraction or a non-number is not a whole number.
func TestCallSecondsDecodesIntegralNumbers(t *testing.T) {
	for raw, want := range map[string]CallSeconds{"1000.0": 1000, "1e3": 1000, "60": 60,
		"99999999999999999999": math.MaxInt32, "-99999999999999999999": -1,
		"1e1000000000": math.MaxInt32, "-1e99999999999999999999": -1, "0e999999999": 0} {
		var seconds CallSeconds
		if err := json.Unmarshal([]byte(raw), &seconds); err != nil || seconds != want {
			t.Errorf("%s decoded as %d, %v; want %d", raw, seconds, err, want)
		}
	}
	for _, raw := range []string{"60.5", `"60"`, "true", "1e-1000000000"} {
		var seconds CallSeconds
		err := json.Unmarshal([]byte(raw), &seconds)
		if err == nil || err.Error() != "gateway.maxModelCallSeconds: "+raw+" is not a whole number of seconds" {
			t.Errorf("%s: %v", raw, err)
		}
	}
	c := composedConfig(t)
	huge := CallSeconds(math.MaxInt32)
	c.Gateway.MaxModelCallSeconds = &huge
	if !slices.Contains(c.ServeProblems(), "gateway.maxModelCallSeconds (1 to 3600 seconds; omit it for 1200)") {
		t.Errorf("huge value not reported out of range: %v", c.ServeProblems())
	}
}

// Home repeats the recovery of reservations from before leases on every
// sweep until it succeeds once, then sweeps only expired leases.
func TestHomeRetriesLegacyRecoveryUntilItSucceeds(t *testing.T) {
	dir := filepath.Join(testutil.PrivateDir(t), "accounting")
	ledger, err := budgetfile.NewLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	if _, err = ledger.Reserve(t.Context(), "account", budget.UsageReservation{MaxUnits: 100}); err != nil {
		t.Fatal(err)
	}
	// An unreadable record fails the first sweep, so legacy recovery repeats.
	if err = os.Mkdir(filepath.Join(dir, "account-unreadable.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Service{ledger: ledger, log: io.Discard}
	s.sweepLedger()
	if s.legacySwept.Load() {
		t.Fatal("a failed legacy recovery was marked done")
	}
	if err = os.Remove(filepath.Join(dir, "account-unreadable.json")); err != nil {
		t.Fatal(err)
	}
	s.sweepLedger()
	if state, err := ledger.State(t.Context(), "account"); err != nil || !s.legacySwept.Load() ||
		state.UnitsUsed != 100 || state.ReservedUnits != 0 {
		t.Fatalf("state=%+v err=%v swept=%v", state, err, s.legacySwept.Load())
	}
}

// The digest problem says exactly what form the value takes.
func TestInstructionDigestProblemNamesItsForm(t *testing.T) {
	c := composedConfig(t)
	c.Local.InstructionDigest = "sha256:" + c.Local.InstructionDigest
	want := "local.instructionDigest (SHA-256 of local.instructions as 64 lowercase hex characters, without a sha256: prefix)"
	if !slices.Contains(c.ServeProblems(), want) {
		t.Fatalf("problems = %v", c.ServeProblems())
	}
}

// Each further harness entry is checked by its own key; a home-owned
// certificate store replaces the administrator certificate files.
func TestServeProblemsNameEachHarnessEntryAndAcceptManagedCertificates(t *testing.T) {
	c := composedConfig(t)
	c.Gateway.CertificateFile, c.Gateway.KeyFile, c.Gateway.TrustBundleFile = "", "", ""
	c.Gateway.PKIDirectory = filepath.Join(c.Directory, "pki")
	c.Harnesses = []LocalHarness{{Harness: "bogus", Name: "Bad Name", Model: " ", ProviderURL: "ftp://x",
		ProviderTokenFile: "relative"}}
	problems := c.ServeProblems()
	for _, key := range []string{"harnesses[0].harness", "harnesses[0].name", "harnesses[0].gatewayPort",
		"harnesses[0].model", "harnesses[0].providerURL", "harnesses[0].providerTokenFile"} {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, key+" (") }) {
			t.Errorf("no problem names %s: %q", key, problems)
		}
	}
	if slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, "gateway.") }) {
		t.Errorf("managed certificates reported as missing gateway files: %q", problems)
	}
}

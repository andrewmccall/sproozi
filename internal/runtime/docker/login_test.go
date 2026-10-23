package docker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func claudeLogin() LoginMount {
	return LoginMount{Volume: "local-login", File: ".credentials.json", Target: "/workspace/session/claude/.credentials.json"}
}

func TestLoginVolumeIsCreatedOnceAndPreparedByTheHelper(t *testing.T) {
	f := newFakeDocker()
	e := &Engine{execute: f.run}
	helper := "sha256:" + strings.Repeat("b", 64)
	for range 2 {
		if err := e.EnsureLoginVolume(context.Background(), claudeLogin(), harness.NativeClaude, helper); err != nil {
			t.Fatal(err)
		}
	}
	creates := slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "volume create --name local-login") })
	if creates < 0 || slices.ContainsFunc(f.calls[creates+1:], func(c string) bool { return strings.HasPrefix(c, "volume create") }) {
		t.Fatalf("login volume created other than once: %q", f.calls)
	}
	want := "run --rm --network none --read-only --cap-drop ALL --cap-add CHOWN --security-opt no-new-privileges " +
		"--user 0:0 --mount type=volume,source=local-login,target=/login --entrypoint /usr/local/bin/checkpoint " +
		helper + " init-login --file /login/.credentials.json"
	if !slices.Contains(f.calls, want) {
		t.Fatalf("helper did not prepare the login file: %q", f.calls)
	}
	// A volume with another owner is never adopted.
	f.volumes["local-login"]["Labels"] = map[string]string{loginLabel: "codex"}
	if err := e.EnsureLoginVolume(context.Background(), claudeLogin(), harness.NativeClaude, helper); err == nil {
		t.Fatal("adopted another harness's login volume")
	}
}

func TestOnlyTheAgentMountsItsLoginFile(t *testing.T) {
	f := newFakeDocker()
	e := &Engine{execute: f.run}
	volume, err := e.EnsureVolume(context.Background(), "x-workspace", "allocation", 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	login := claudeLogin()
	spec := ManagedSpec{Name: "x-agent", Execution: "allocation", Role: roleAgent, ImageID: "sha256:" + strings.Repeat("a", 64),
		NetworkMode: "fixture-network", Volume: &volume, Login: &login, MemoryBytes: 64 << 20, CPUMilli: 100, PIDs: 16,
		Deadline: "2030-01-01T00:00:00Z"}
	instance, err := e.EnsureManaged(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(f.calls, func(c string) bool {
		return strings.Contains(c, "--mount type=volume,source=local-login,target=/workspace/session/claude/.credentials.json,volume-subpath=.credentials.json")
	}) {
		t.Fatalf("agent lacks its login file: %q", f.calls)
	}
	withoutLogin := spec
	withoutLogin.Login = nil
	if err = e.VerifyManaged(context.Background(), instance, withoutLogin); err == nil {
		t.Fatal("an agent with an unexpected mount was verified")
	}
	helper := spec
	helper.Name, helper.Role = "x-helper", roleHelper
	if _, err = e.EnsureManaged(context.Background(), helper); err == nil {
		t.Fatal("the helper was given the login file")
	}
	escaped := login
	escaped.Target = "/home/agent/.credentials.json"
	spec.Name, spec.Login = "x-other", &escaped
	if _, err = e.EnsureManaged(context.Background(), spec); err == nil {
		t.Fatal("a login file outside the session folder was mounted")
	}
}

func TestPlacementMountsEachHarnessesOwnSignInFile(t *testing.T) {
	c := fixtureConfig()
	c.SignIn = true
	for kind, want := range map[harness.NativeKind]string{
		harness.NativeCodex:  "/workspace/session/codex/auth.json",
		harness.NativeClaude: "/workspace/session/claude/.credentials.json",
	} {
		c.Harness = kind
		if err := c.validate(); err != nil {
			t.Fatal(err)
		}
		got := (&Placement{config: c}).login()
		if got == nil || got.Target != want || got.Volume != c.NetworkName+"-"+c.Name+"-login" || !strings.HasSuffix(want, "/"+got.File) {
			t.Fatalf("%s login mount: %+v", kind, got)
		}
	}
	p := &Placement{config: c}
	if p.spec(placementRecord{}, roleHelper).Login != nil || p.spec(placementRecord{}, roleAgent).Login == nil {
		t.Fatal("only the agent mounts the login file")
	}
	c.SignIn = false
	if (&Placement{config: c}).login() != nil {
		t.Fatal("a gateway-held key still mounted a login file")
	}
}

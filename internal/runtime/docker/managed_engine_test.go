package docker

import (
	"io"
	"strings"
	"testing"
)

const fixtureBridge = "bridge"

const missingFixtureAccount = "missing"

func TestManagedAnchorCapabilitySpellings(t *testing.T) {
	t.Parallel()
	s := ManagedSpec{Name: "anchor", Role: roleAnchor, ImageID: "sha256:fixed", NetworkMode: fixtureBridge, IntentDigest: "intent"}
	i := Instance{ID: "container-id"}
	cmd := []string{networkCommandPath, "192.168.65.254", "18443"}
	r := managedInspection{ID: i.ID, Image: s.ImageID, Name: "/anchor"}
	r.Config.User = "0:0"
	r.Config.Cmd, r.Config.Entrypoint = cmd[1:], cmd[:1]
	r.Config.Labels = map[string]string{launchLabel: s.IntentDigest}
	r.HostConfig.ReadonlyRootfs = true
	r.HostConfig.NetworkMode = fixtureBridge
	r.HostConfig.CapDrop = []string{allCapabilities}
	for _, test := range []struct {
		name string
		caps []string
		want bool
	}{
		{"short", []string{"NET_ADMIN"}, true},
		{"canonical", []string{canonicalNetAdmin}, true},
		{missingFixtureAccount, nil, false},
		{"extra", []string{canonicalNetAdmin, "CAP_SYS_ADMIN"}, false},
		{"different", []string{"CAP_SYS_ADMIN"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r.HostConfig.CapAdd = test.caps
			if got := matchesManagedLaunch(r, i, s, cmd); got != test.want {
				t.Fatalf("managed launch accepted=%v, want %v", got, test.want)
			}
		})
	}
}

// io.Copy from a reader without WriteTo, as exec does for a pipe, must not
// bypass the limit through a ReadFrom method.
func TestBoundedOutputEnforcesItsLimitThroughCopy(t *testing.T) {
	large := strings.Repeat("x", 200000)
	out := &boundedOutput{limit: 1024}
	if _, err := io.Copy(out, struct{ io.Reader }{strings.NewReader(large)}); err == nil {
		t.Fatal("output over the limit was accepted")
	}
	if len(out.Bytes()) > 1024 {
		t.Fatalf("kept %d bytes over a 1024-byte limit", len(out.Bytes()))
	}
	errout := &boundedOutput{limit: 1024, truncate: true}
	if _, err := io.Copy(errout, struct{ io.Reader }{strings.NewReader(large)}); err != nil {
		t.Fatal(err)
	}
	if got, want := errout.String(), large[:1024]+" [output truncated]"; got != want {
		t.Fatalf("truncated output has %d bytes, want %d", len(got), len(want))
	}
}

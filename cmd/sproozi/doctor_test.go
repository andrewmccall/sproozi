//nolint:goconst // Flag names read best as literals in command lines.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

func writeProfileFile(t *testing.T, root, name string, data []byte) {
	t.Helper()
	path := filepath.Join(root, "profile", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// incompressible returns n bytes that gzip cannot shrink.
func incompressible(n int) []byte {
	data := make([]byte, n)
	state := uint32(7)
	for i := range data {
		state = state*1664525 + 1013904223
		data[i] = byte(state >> 24)
	}
	return data
}

func runDoctor(t *testing.T, root string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := doctor(filepath.Join(root, "config.json"), &out)
	return out.String(), err
}

func TestDoctorReportsTheProfileAndItsKubernetesSize(t *testing.T) {
	root := testutil.PrivateDir(t)
	skill := []byte("---\nname: review\ndescription: Review code\n---\n")
	writeProfileFile(t, root, "AGENTS.md", []byte("Be brief.\n"))
	writeProfileFile(t, root, "skills/review/SKILL.md", skill)
	out, err := runDoctor(t, root)
	if err != nil || !strings.Contains(out, "warn  configuration") || !strings.Contains(out, "ok    profile") ||
		!strings.Contains(out, "1 skills [review]") {
		t.Fatalf("doctor = %q, %v", out, err)
	}

	writeProfileFile(t, root, "skills/review/data.bin", incompressible(agentprofile.MaxArchiveBytes*7/8))
	out, err = runDoctor(t, root)
	if err != nil || !strings.Contains(out, "near the Kubernetes limit") {
		t.Fatalf("near limit = %q, %v", out, err)
	}

	writeProfileFile(t, root, "skills/review/data.bin", incompressible(agentprofile.MaxArchiveBytes+1024))
	out, err = runDoctor(t, root)
	if err == nil || !strings.Contains(out, "error profile") || !strings.Contains(out, "cannot move to Kubernetes") {
		t.Fatalf("over limit = %q, %v", out, err)
	}
}

func TestDoctorFailsOnAnInvalidProfileOrConfiguration(t *testing.T) {
	root := testutil.PrivateDir(t)
	writeProfileFile(t, root, "skills/sproozi-access/SKILL.md", []byte("---\nname: sproozi-access\n---\n"))
	if err := testutil.WriteFile(filepath.Join(root, "config.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runDoctor(t, root)
	if err == nil || !strings.Contains(out, "error configuration") || !strings.Contains(out, "reserved") {
		t.Fatalf("doctor = %q, %v", out, err)
	}
}

func TestDoctorIsACommand(t *testing.T) {
	root := testutil.PrivateDir(t)
	out := captureCLIOutput(t, []string{actionInit, "--directory", root})
	if strings.TrimSpace(out) != root {
		t.Fatal(out)
	}
	out = captureCLIOutput(t, []string{actionDoctor, "--config", filepath.Join(root, "config.json")})
	if !strings.Contains(out, "ok    profile") {
		t.Fatalf("doctor = %q", out)
	}
}

package manifests_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCRDInstallUsesServerSideApply(t *testing.T) {
	// Exercise the Makefile invocation, not just its text. Client-side apply
	// cannot install the PodTemplate-bearing AgentRuntime CRD in a real cluster.
	kubectl := filepath.Join(t.TempDir(), "kubectl")
	script := `#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *' --server-side '*) cat >/dev/null ;;
  *) echo 'CRD last-applied annotation exceeds 262144 bytes' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(kubectl, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("make", "install", "KUBECTL="+kubectl)
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CRD installation used client-side apply: %v\n%s", err, out)
	}
}

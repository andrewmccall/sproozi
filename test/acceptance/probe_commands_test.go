package acceptance

import (
	"os/exec"
	"strings"
	"testing"
)

func TestLiveErrorsRedactCredentialsBeforeTruncation(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "provider-test-secret")
	input := "provider-test-secret https://sproozi:eyJtest.abcdefghijk.abcdefghijkl@gateway:8443 " +
		"sk-test-secret ghs_testsecret " + strings.Repeat("x", 600)
	got := redact(input)
	for _, secret := range []string{"provider-test-secret", "eyJtest", "sk-test-secret", "ghs_testsecret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redaction leaked %s", secret)
		}
	}
}

// This checks shell argument construction, not network enforcement. The live
// suite runs these same commands in the run's sandbox, or in a separate Pod
// for the probes that must be refused.
func TestDenialProbeShellArguments(t *testing.T) {
	const clients = `
cat() { printf test-token; }
curl() {
  previous=; user=; target=
  for arg do
    if [ "$previous" = --proxy-user ]; then user="$arg"; fi
    previous="$arg"
    case "$arg" in https://*) target="$arg" ;; esac
  done
  case "$target" in */version) return 28 ;; esac
  [ "$user" = sproozi:test-token ] || return 41
  case "$*" in *http_connect*) printf 401; return ;; esac
  case "$target" in
    */secrets) printf 'kubernetes request denied HTTP_STATUS=403' ;;
    *not-authorized.git*) printf 'repository not permitted by policy HTTP_STATUS=403' ;;
    */pods) printf 200 ;;
    *) return 42 ;;
  esac
}
`
	for _, probe := range demoDenialProbes() {
		t.Run(probe.name, func(t *testing.T) {
			out, err := exec.Command("/bin/sh", "-eu", "-c", clients+probe.script()).CombinedOutput()
			if err != nil {
				t.Fatalf("probe shell did not pass correctly quoted arguments: %v: %s", err, out)
			}
		})
	}
}

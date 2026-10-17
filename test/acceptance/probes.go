package acceptance

import (
	"os"
	"regexp"
	"strings"
)

// These helpers are shared by the live suite and its offline tests, so they
// stay outside the live build tag and run under make test.

const gatewayAudience = "sproozi-gateway"

type denialProbe struct {
	name, audience, command string
}

// Each command runs verbatim as one shell line in a probe Pod.
//
//nolint:lll
func demoDenialProbes() []denialProbe {
	// Establish a working authenticated route before testing denials. An image
	// without curl, a broken proxy, bad trust or an inactive run must fail here.
	control := `request() { curl --silent --show-error --connect-timeout 5 --max-time 15 --noproxy '' --proxy https://sproozi-gateway.sproozi-system.svc:8443 --proxy-cacert /etc/sproozi/trust/ca-bundle.pem --cacert /etc/sproozi/trust/ca-bundle.pem --proxy-user "sproozi:$(cat /var/run/sproozi/token)" "$@"; }; code="$(request -o /dev/null -w '%{http_code}' https://kubernetes.default.svc/api/v1/namespaces/sproozi-demo/pods)"; test "$code" = 200; `
	return []denialProbe{
		{
			name:     "direct-network",
			audience: gatewayAudience,
			command:  control + `unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; rc=0; curl --silent --show-error --noproxy '*' --connect-timeout 5 --max-time 10 https://kubernetes.default.svc/version >/dev/null 2>&1 || rc=$?; printf 'direct-network curl exit=%s\n' "$rc"; test "$rc" = 28`,
		},
		{
			name:     "wrong-audience",
			audience: "sproozi-wrong-audience",
			command:  `code="$(curl --silent --show-error --connect-timeout 5 --max-time 10 --noproxy '' --proxy https://sproozi-gateway.sproozi-system.svc:8443 --proxy-cacert /etc/sproozi/trust/ca-bundle.pem --proxy-user "sproozi:$(cat /var/run/sproozi/token)" -o /dev/null -w '%{http_connect}' https://kubernetes.default.svc/api/v1/namespaces/sproozi-demo/pods 2>/dev/null || true)"; test "$code" = 401`,
		},
		{
			name:     "github-scope",
			audience: gatewayAudience,
			command:  control + `result="$(request -w ' HTTP_STATUS=%{http_code}' 'https://github.com/sproozi-denied/not-authorized.git/info/refs?service=git-upload-pack')"; case "$result" in *'repository not permitted by policy'*' HTTP_STATUS=403') ;; *) exit 42 ;; esac`,
		},
		{
			name: "kubernetes-secrets", audience: gatewayAudience,
			command: control + `result="$(request -w ' HTTP_STATUS=%{http_code}' https://kubernetes.default.svc/api/v1/namespaces/sproozi-demo/secrets)"; case "$result" in *'kubernetes request denied'*' HTTP_STATUS=403') ;; *) exit 42 ;; esac`,
		},
	}
}

func redact(value string) string {
	value = strings.TrimSpace(value)
	for _, name := range []string{"OPENAI_API_KEY", "GITHUB_TOKEN", "GH_TOKEN"} {
		if secret := os.Getenv(name); secret != "" {
			value = strings.ReplaceAll(value, secret, "<REDACTED>")
		}
	}
	value = regexp.MustCompile(`(?i)(https?://)[^\s/]+@`).ReplaceAllString(value, "${1}<REDACTED>@")
	value = regexp.MustCompile(`(?:sk-|gh[pousr]_|github_pat_)[A-Za-z0-9_-]+`).ReplaceAllString(value, "<REDACTED>")
	value = regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`).ReplaceAllString(value, "<REDACTED>")
	privateKey := regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	value = privateKey.ReplaceAllString(value, "<REDACTED>")
	if len(value) > 512 {
		return value[:512] + "..."
	}
	return value
}

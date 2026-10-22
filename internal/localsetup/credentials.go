package localsetup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
)

// Public provider origins the gateway uses by default.
const (
	anthropicURL = "https://api.anthropic.com"
	openAIURL    = "https://api.openai.com"
)

// Provider describes the upstream model service for a native harness.
type Provider struct {
	Name string
	URL  string
}

// ProviderFor is the gateway-owned provider each harness uses by default.
// SignsIn reports a harness that signs in to its own model provider, so the
// local gateway forwards its sign-in and holds no key for it (ADR 0047).
func SignsIn(kind harness.NativeKind) bool {
	h, err := harness.Managed(kind)
	return err == nil && h.SignInHost != ""
}

func ProviderFor(kind harness.NativeKind) Provider {
	h, _ := harness.Managed(kind)
	if h.Protocol == harness.ProtocolAnthropic {
		return Provider{Name: string(harness.ProtocolAnthropic), URL: anthropicURL}
	}
	return Provider{Name: string(harness.ProtocolOpenAI), URL: openAIURL}
}

// CredentialInput supplies a secret without a command-line value: a protected
// file, standard input, or an interactive prompt that does not echo.
type CredentialInput struct {
	File   string
	Stdin  io.Reader
	Prompt func(label string) (string, error)
}

// StoreCredential reads the provider credential and publishes it atomically
// under credentials/. The value is never printed or logged.
func StoreCredential(l Layout, name string, in CredentialInput) (string, error) {
	var value string
	switch {
	case in.File != "":
		data, err := privatefs.ReadFile(in.File, 16<<10)
		if err != nil {
			return "", fail("credential", fmt.Errorf("read %s: %w", in.File, err), "store the key in an owner-only file (chmod 600)")
		}
		value = string(data)
	case in.Stdin != nil:
		data, err := io.ReadAll(io.LimitReader(in.Stdin, 16<<10))
		if err != nil {
			return "", err
		}
		value = string(data)
	case in.Prompt != nil:
		var err error
		if value, err = in.Prompt(fmt.Sprintf("%s API key", name)); err != nil {
			return "", err
		}
	default:
		return "", fail("credential", fmt.Errorf("no %s credential supplied", name), fmt.Sprintf("sproozi credentials add %s", name))
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \r\n\t\x00") || len(value) > 8<<10 {
		next := "give the key alone, with no spaces; nothing was saved"
		switch {
		case in.Stdin != nil:
			next = "pipe the key itself, for example: printf '%s' \"$API_KEY\" | sproozi init --credential-stdin; " +
				"nothing was saved"
		case in.File != "":
			next = "put only the key in " + in.File + "; nothing was saved"
		}
		return "", fail("credential", errors.New("the key is empty or contains whitespace"), next)
	}
	if err := privatefs.EnsureDir(l.Credentials()); err != nil {
		return "", err
	}
	if err := privatefs.Replace(l.Credentials(), name, []byte(value+"\n")); err != nil {
		return "", err
	}
	return filepath.Join(l.Credentials(), name), nil
}

// StoreProviderCA copies a private provider endpoint's CA bundle into
// credentials/ as an owner-only file, which the gateway reads at startup. The
// source can be readable by anyone, since a CA certificate is public; it must
// not be writable by other accounts, since whoever can change it could make the
// gateway trust another endpoint with the provider key. Later changes to the
// source take effect only when setup copies it again.
func StoreProviderCA(l Layout, provider, source string) (string, error) {
	next := "pass --provider-ca-file a PEM file holding the endpoint's CA certificates"
	info, err := os.Stat(source)
	if err != nil {
		return "", fail("provider", fmt.Errorf("read --provider-ca-file: %w", err), next)
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return "", fail("provider", fmt.Errorf("--provider-ca-file %s is not a regular file of at most 64 KiB", source), next)
	}
	if info.Mode().Perm()&0o002 != 0 {
		return "", fail("provider", fmt.Errorf("--provider-ca-file %s is writable by every account", source),
			"remove that access (chmod o-w), or copy the file somewhere only you can change")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fail("provider", fmt.Errorf("read --provider-ca-file: %w", err), next)
	}
	if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(data) {
		return "", fail("provider", fmt.Errorf("--provider-ca-file %s holds no PEM certificate", source), next)
	}
	if err = privatefs.EnsureDir(l.Credentials()); err != nil {
		return "", err
	}
	name := provider + "-ca.pem"
	if err = privatefs.Replace(l.Credentials(), name, data); err != nil {
		return "", err
	}
	return filepath.Join(l.Credentials(), name), nil
}

// HasCredential reports whether a stored credential exists.
func HasCredential(l Layout, name string) bool {
	_, err := privatefs.ReadFile(filepath.Join(l.Credentials(), name), 16<<10)
	return err == nil
}

// CheckCredential asks the provider to list models with the stored key. It
// makes no inference request. An authentication rejection is an error; an
// unreachable provider is reported separately.
func CheckCredential(ctx context.Context, kind harness.NativeKind, providerURL, caFile, credentialFile string) error {
	key, err := privatefs.ReadFile(credentialFile, 16<<10)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(providerURL, "/")+"/v1/models", nil)
	if err != nil {
		return err
	}
	if kind == harness.NativeClaude {
		request.Header.Set("x-api-key", strings.TrimSpace(string(key)))
		request.Header.Set("anthropic-version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		data, err := os.ReadFile(caFile)
		if err != nil {
			return err
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		tlsConfig.RootCAs.AppendCertsFromPEM(data)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	var unknownAuthority x509.UnknownAuthorityError
	var verification *tls.CertificateVerificationError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &unknownAuthority) || errors.As(err, &verification) || errors.As(err, &hostname):
		return fail("provider", fmt.Errorf("the provider's certificate is not trusted: %w", err),
			"for a private endpoint, pass its CA bundle with --provider-ca-file FILE")
	case err != nil:
		return fail("provider", fmt.Errorf("provider unreachable: %w", err), "check network access, or pass --skip-credential-check")
	}
	_ = response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return fail("provider", fmt.Errorf("the provider rejected the credential (HTTP %d)", response.StatusCode), "replace it with sproozi credentials add")
	case response.StatusCode >= 300:
		return fail("provider", fmt.Errorf("credential check returned HTTP %d", response.StatusCode), "retry, or pass --skip-credential-check")
	}
	return nil
}

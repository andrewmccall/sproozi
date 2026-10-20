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

// Provider describes the upstream model service for a native harness.
type Provider struct {
	Name string
	URL  string
}

// ProviderFor is the gateway-owned provider each harness uses by default.
func ProviderFor(kind harness.NativeKind) Provider {
	if kind == harness.NativeClaude {
		return Provider{Name: "anthropic", URL: "https://api.anthropic.com"}
	}
	return Provider{Name: "openai", URL: "https://api.openai.com"}
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
		return "", fail("credential", errors.New("the credential is empty or contains whitespace"), "")
	}
	if err := privatefs.EnsureDir(l.Credentials()); err != nil {
		return "", err
	}
	if err := privatefs.Replace(l.Credentials(), name, []byte(value+"\n")); err != nil {
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
	if err != nil {
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

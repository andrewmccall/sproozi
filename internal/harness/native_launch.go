package harness

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// NativeLaunch is the trusted fixed native terminal launch contract. Portable
// checkpoint contents do not supply these instructions, options or credentials.
type NativeLaunch struct {
	Harness           NativeKind     `json:"harness"`
	Deadline          time.Time      `json:"deadline"`
	ThreadID          string         `json:"threadId"`
	Instructions      string         `json:"instructions"`
	InstructionDigest string         `json:"instructionDigest"`
	Env               []string       `json:"env"`
	RolloutMaxBytes   int64          `json:"rolloutMaxBytes"`
	Model             string         `json:"model,omitempty"`
	Options           []string       `json:"options,omitempty"`
	Gateway           *NativeGateway `json:"gateway,omitempty"`
	TrustBundlePEM    []byte         `json:"trustBundlePEM,omitempty"`
}

// NativeGateway projects a fresh workload token only at activation. Its fixed
// token/trust paths are runtime-owned mounts outside the portable state pair.
type NativeGateway struct {
	Endpoint    string `json:"endpoint"`
	TokenPath   string `json:"tokenPath"`
	TrustBundle string `json:"trustBundle"`
}

const (
	sslTrustEnvironment      = "SSL_CERT_FILE"
	requestsTrustEnvironment = "REQUESTS_CA_BUNDLE"
	nodeTrustEnvironment     = "NODE_EXTRA_CA_CERTS"
	gitTrustEnvironment      = "GIT_SSL_CAINFO"
	curlTrustEnvironment     = "CURL_CA_BUNDLE"
	NativeLocalTrustBundle   = "/tmp/sproozi-native/ca.pem"
	NativeGatewayTokenPath   = "/var/run/sproozi/tokens/gateway/token"
	NativeGatewayTrustBundle = "/etc/sproozi/trust/ca-bundle.pem"
)

func (g NativeGateway) Validate(env []string) error {
	endpoint, err := url.Parse(g.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.ForceQuery || g.TokenPath != NativeGatewayTokenPath || g.TrustBundle != NativeGatewayTrustBundle {
		return errors.New("invalid projected native gateway binding")
	}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", sslTrustEnvironment, nodeTrustEnvironment, gitTrustEnvironment, requestsTrustEnvironment, curlTrustEnvironment:
			return errors.New("projected gateway conflicts with literal proxy environment")
		}
	}
	return nil
}
func (c NativeLaunch) Environment() ([]string, error) {
	env := append([]string{}, c.Env...)
	if len(c.TrustBundlePEM) > 0 {
		if c.Gateway != nil {
			return nil, errors.New("local and projected trust bindings conflict")
		}
		if err := installNativeTrust(c.TrustBundlePEM); err != nil {
			return nil, err
		}
		for _, key := range []string{sslTrustEnvironment, nodeTrustEnvironment, gitTrustEnvironment, requestsTrustEnvironment, curlTrustEnvironment} {
			env = append(env, key+"="+NativeLocalTrustBundle)
		}
	}
	if c.Gateway == nil {
		return env, nil
	}
	if err := c.Gateway.Validate(env); err != nil {
		return nil, err
	}
	f, err := os.Open(c.Gateway.TokenPath)
	if err != nil {
		return nil, errors.New("projected native token is unavailable")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 16385))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(data) > 16384 {
		return nil, errors.New("projected native token is unreadable or oversized")
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, "\x00\r\n\t ") {
		return nil, errors.New("invalid projected native token")
	}
	proxy, _ := url.Parse(c.Gateway.Endpoint)
	proxy.User = url.UserPassword("sproozi", token)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		env = append(env, key+"="+proxy.String())
	}
	env = append(env, "NO_PROXY=", "no_proxy=")
	for _, key := range []string{sslTrustEnvironment, nodeTrustEnvironment, gitTrustEnvironment, requestsTrustEnvironment, curlTrustEnvironment} {
		env = append(env, fmt.Sprintf("%s=%s", key, c.Gateway.TrustBundle))
	}
	return env, nil
}

func installNativeTrust(data []byte) error {
	if len(data) > 64<<10 {
		return errors.New("native trust bundle exceeds limit")
	}
	remaining := data
	count := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return errors.New("native trust bundle must contain only certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return errors.New("invalid native trust certificate")
		}
		remaining = rest
		count++
	}
	if count == 0 {
		return errors.New("native trust bundle has no certificates")
	}
	f, err := os.OpenFile(NativeLocalTrustBundle, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	if os.IsExist(err) {
		existing, readErr := os.ReadFile(NativeLocalTrustBundle)
		if readErr != nil || !bytes.Equal(existing, data) {
			return errors.New("native trust bundle changed")
		}
		return nil
	}
	if err != nil {
		return errors.New("native trust bundle installation failed")
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

package harness

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/nativeconfig"
)

const NativeRolloutMaxBytes int64 = 64 << 20
const nativeOpenAIKey = "OPENAI_API_KEY"
const nativeAnthropicKey = "ANTHROPIC_API_KEY"

// NativeLaunch is the trusted fixed native terminal launch contract. Portable
// checkpoint contents do not supply these instructions, options or credentials.
type NativeLaunch struct {
	Harness           NativeKind          `json:"harness"`
	Deadline          time.Time           `json:"deadline"`
	ThreadID          string              `json:"threadId"`
	ClientConfig      *NativeClientConfig `json:"clientConfig,omitempty"`
	Task              string              `json:"task,omitempty"`
	Instructions      string              `json:"instructions"`
	InstructionDigest string              `json:"instructionDigest"`
	Env               []string            `json:"env"`
	RolloutMaxBytes   int64               `json:"rolloutMaxBytes"`
	Model             string              `json:"model,omitempty"`
	Options           []string            `json:"options,omitempty"`
	Gateway           *NativeGateway      `json:"gateway,omitempty"`
	TrustBundlePEM    []byte              `json:"trustBundlePEM,omitempty"`
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
	// NativeProfileArchive is the session's stored profile on Kubernetes;
	// NativeProfileDirectory is the live folder Docker mounts.
	NativeProfileArchive   = "/etc/sproozi/profile-archive/profile.tar.gz"
	NativeProfileDirectory = "/etc/sproozi/profile"
)

func (g NativeGateway) Validate(env []string) error {
	endpoint, err := url.Parse(g.Endpoint)
	if err != nil || endpoint.Scheme != nativeHTTPScheme || endpoint.Host == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.ForceQuery || g.TokenPath != NativeGatewayTokenPath || g.TrustBundle != NativeGatewayTrustBundle {
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
	proxy.User = url.UserPassword(nativeContextName, token)
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

// Validate checks the trusted launch at the native process boundary.
func (c NativeLaunch) Validate(now time.Time) error {
	if err := c.Harness.Validate(); err != nil {
		return err
	}
	if !c.Deadline.After(now) || (c.ThreadID != "" && !codexThreadID.MatchString(c.ThreadID)) ||
		c.Env == nil || c.RolloutMaxBytes <= 0 || c.RolloutMaxBytes > NativeRolloutMaxBytes {
		return errors.New("invalid native launch contract")
	}
	if len(c.Task) > 32<<10 || strings.ContainsRune(c.Task, 0) ||
		c.Task != "" && (strings.TrimSpace(c.Task) == "" || c.Harness != NativeCodex) {
		return errors.New("native task requires bounded nonempty Codex input")
	}
	if c.Instructions == "" || fmt.Sprintf("%x", sha256.Sum256([]byte(c.Instructions))) != c.InstructionDigest {
		return errors.New("trusted instructions do not match digest")
	}
	if c.Gateway != nil {
		if err := c.Gateway.Validate(c.Env); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, entry := range c.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsAny(key+value, "\x00") || seen[key] {
			return errors.New("invalid explicit native environment")
		}
		seen[key] = true
		switch key {
		case "HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PWD", "KUBECONFIG",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_AUTOUPDATER",
			"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":
			return errors.New("native environment cannot replace fixed home or workspace")
		}
	}
	if err := c.ClientConfig.Validate(c.Harness); err != nil {
		return err
	}
	return nativeconfig.ValidateHarness(string(c.Harness), c.Model, c.Options)
}

// NativeBaseEnvironment is the public, credential-free environment shared by
// both backends. Each authority adds its own fresh proxy/token binding.
func NativeBaseEnvironment(kind NativeKind, executionUID string) ([]string, error) {
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	apiKey := nativeOpenAIKey
	if kind == NativeClaude {
		apiKey = nativeAnthropicKey
	}
	return []string{"PATH=/opt/git/bin:/usr/local/bin:/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8",
		"GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "GH_TOKEN=sproozi-worker-placeholder",
		"SPROOZI_EXECUTION_UID=" + executionUID, apiKey + "=sproozi-worker-placeholder"}, nil
}

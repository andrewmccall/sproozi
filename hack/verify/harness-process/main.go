// Command harness-process exercises installed stock clients against an isolated
// authenticated TLS CONNECT gateway and deterministic local model providers.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/budget"
	mcpbroker "github.com/andrewmccall/sproozi/internal/endpoints/mcp"
	"github.com/andrewmccall/sproozi/internal/endpoints/model"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const opencodeHarness = "opencode"
const modelFlag = "--model"
const fixtureName = "fixture"
const claudeHarness = "claude-code"
const successScenario = "success"

const answer = "SPROOZI_FIXTURE_COMPLETE"
const providerKey = "fixture-gateway-provider-key"
const proxyToken = "fixture-projected-token"

type result struct {
	Harness                string `json:"harness"`
	Mode                   string `json:"mode"`
	Scenario               string `json:"scenario"`
	Version                string `json:"version"`
	ExitCode               int    `json:"exitCode"`
	TimedOut               bool   `json:"timedOut"`
	ModelRequests          int    `json:"modelRequests"`
	ToolCalls              int    `json:"toolCalls"`
	ReceivedToolResult     bool   `json:"receivedToolResult"`
	AuthenticatedSessions  int    `json:"authenticatedSessions"`
	UnitsUsed              int64  `json:"unitsUsed"`
	ReservedUnits          int64  `json:"reservedUnits"`
	ToolUnitsUsed          int64  `json:"toolUnitsUsed"`
	MissingIdentityDenied  bool   `json:"missingIdentityDenied"`
	UnknownAuthorityDenied bool   `json:"unknownAuthorityDenied"`
	Output                 string `json:"output"`
	SessionExport          string `json:"sessionExport,omitempty"`
	Passed                 bool   `json:"passed"`
}
type fixture struct {
	mu                        sync.Mutex
	requests, tools, sessions int
	violations                []string
	tool                      bool
	receivedToolResult        bool
}

func main() {
	selected := flag.String("harness", "all", "all, claude-code or opencode")
	output := flag.String("output", ".local/verification/harness-process.json", "evidence output path")
	withTool := flag.Bool("tool", true, "require a generated native MCP configuration and actual tool call")
	flag.Parse()
	names := []string{claudeHarness, opencodeHarness}
	if *selected != "all" {
		names = []string{*selected}
	}
	results := make([]result, 0, 4*len(names))
	passed := true
	for _, name := range names {
		modes := []bool{false}
		if name == opencodeHarness {
			modes = append(modes, true)
		}
		for _, supervised := range modes {
			for _, scenario := range []string{successScenario, "denied"} {
				r, err := run(name, scenario, *withTool, supervised)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					passed = false
				}
				results = append(results, r)
				passed = passed && r.Passed
				fmt.Printf("%s %s %s: passed=%v exit=%d model_requests=%d tool_calls=%d units=%d\n",
					name, r.Mode,
					scenario,
					r.Passed,
					r.ExitCode,
					r.ModelRequests,
					r.ToolCalls,
					r.UnitsUsed)
			}
		}
	}
	evidence := map[string]any{"results": results, "limitations": []string{
		"Local mocked model providers; no paid provider inference or Kubernetes Pod/CNI proof",
		"In-memory ledger; persistent ledger, policy revocation and cancellation have separate existing verification",
		"Stock OpenCode can exit zero on provider errors; evidence retains the actual process status",
	}}
	data, _ := json.MarshalIndent(evidence, "", "  ")
	if err := os.MkdirAll(filepath.Dir(*output), 0755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(*output, append(data, '\n'), 0600); err != nil {
		panic(err)
	}
	if !passed {
		os.Exit(1)
	}
}

func run(selected, scenario string, withTool, supervised bool) (result, error) {
	out := result{Harness: selected, Scenario: scenario, ExitCode: -1, Mode: "stock"}
	if supervised {
		out.Mode = "supervised"
	}
	binary := selected
	if selected == claudeHarness {
		binary = "claude"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return out, err
	}
	directory, err := os.MkdirTemp("", "sproozi-harness-process-")
	if err != nil {
		return out, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	home := filepath.Join(directory, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		return out, err
	}
	cert, ca, err := certificate()
	if err != nil {
		return out, err
	}
	caPath := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		return out, err
	}
	f := &fixture{tool: withTool}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		f.model(w,
			r,
			selected)
	}))
	defer upstream.Close()
	ledger := budget.NewMemoryLedger()
	tracker := budget.NewTrackerWithLedger(ledger)
	capabilities := []api.CapabilityKind{api.CapabilityModelInference, api.CapabilityKind("mcp.fixture")}
	runCaps := capabilities
	if scenario == "denied" {
		runCaps = []api.CapabilityKind{api.CapabilityKind("mcp.fixture")}
	}
	identity := &gateway.RunIdentity{Run: &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: fixtureName,
		Namespace: fixtureName,
		UID:       types.UID("fixture-run")},
		Spec: api.AgentRunSpec{Capabilities: runCaps}},
		Policy: &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: fixtureName,
			UID: types.UID("fixture-policy")},
			Spec: api.AgentPolicySpec{AllowedCapabilities: capabilities,
				Budgets: map[api.CapabilityKind]api.EndpointBudget{api.CapabilityModelInference: {MaxUnits: 1000000},
					api.CapabilityKind("mcp.fixture"): {MaxUnits: 20}}}}}
	identity.Policy.Spec.MCPServers = map[string]api.MCPServerScope{
		fixtureName: {Tools: map[string]api.MCPToolScope{"verify": {}}},
	}
	config := model.HandlerConfig{UpstreamURL: upstream.URL,
		ProviderAuth: modelauth.APIKey{Key: providerKey},
		HTTPClient:   upstream.Client(),
		AuditLogger:  audit.NewLogger(io.Discard)}
	var modelHandler http.Handler = &model.Handler{Config: config}
	authority := "api.openai.com:443"
	if selected == claudeHarness {
		authority = "api.anthropic.com:443"
		config.ProviderAuth = modelauth.AnthropicAPIKey{Key: providerKey}
		modelHandler = model.NewAnthropicHandler(config)
	}
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: fixtureName, Version: "1"}, nil)
	mcp.AddTool(mcpServer,
		&mcp.Tool{Name: "verify",
			Description: "Return the acceptance fixture marker. Call this tool to complete the task."},
		func(_ context.Context,
			_ *mcp.CallToolRequest,
			_ map[string]any) (*mcp.CallToolResult,
			any,
			error) {
			f.mu.Lock()
			f.tools++
			f.mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}}, nil, nil
		})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{JSONResponse: true,
			DisableLocalhostProtection: true})
	mcpUpstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "" {
			f.mu.Lock()
			f.violations = append(f.violations, "workload credential reached MCP provider")
			f.mu.Unlock()
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer mcpUpstream.Close()
	registryJSON, _ := json.Marshal(map[string]any{
		"servers": map[string]any{fixtureName: map[string]any{"url": mcpUpstream.URL}},
	})
	registry, err := mcpbroker.LoadRegistry(registryJSON, directory)
	if err != nil {
		return out, err
	}
	broker := mcpbroker.NewHandler(mcpbroker.HandlerConfig{Registry: registry,
		HTTPClient:  mcpUpstream.Client(),
		Budgets:     tracker,
		AuditLogger: audit.NewLogger(io.Discard)})
	routes := gateway.Dispatcher{authority: {Capability: string(api.CapabilityModelInference),
		Tier:    audit.TierSemantic,
		Handler: modelHandler,
		Budgets: tracker},
		"gateway.fixture:443": {Capability: "mcp.fixture",
			Tier:    audit.TierProtocol,
			Budgets: tracker,
			Handler: http.HandlerFunc(func(w http.ResponseWriter,
				r *http.Request) {
				if _,
					ok := gateway.IdentityFromContext(r.Context()); !ok ||
					r.Header.Get("Authorization") != "" ||
					r.Header.Get("Proxy-Authorization") != "" {
					http.Error(w, "invalid fixture identity", http.StatusForbidden)
					return
				}
				if r.URL.Path != "/mcp/fixture" {
					http.NotFound(w, r)
					return
				}
				broker.ServeHTTP(w, r)
			})}}
	transport,
		err := proxytransport.New(proxytransport.Config{Certificates: map[string]tls.Certificate{"api.openai.com": cert,
		"api.anthropic.com": cert,
		"gateway.fixture":   cert},
		Authenticate: func(_ context.Context,
			token string) (*gateway.RunIdentity,
			error) {
			if token != proxyToken {
				return nil, fmt.Errorf("invalid projected token")
			}
			f.mu.Lock()
			f.sessions++
			f.mu.Unlock()
			return identity, nil
		},
		Revalidate: func(context.Context,
			*gateway.RunIdentity) error {
			return nil
		},
		AllowedAuthority: routes.Allows,
		Handler:          routes})
	if err != nil {
		return out, err
	}
	proxy := httptest.NewUnstartedServer(transport)
	proxy.EnableHTTP2 = false
	proxy.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	proxy.StartTLS()
	defer proxy.Close()
	out.MissingIdentityDenied = connectStatus(proxy.URL, authority, "", ca) == http.StatusUnauthorized
	out.UnknownAuthorityDenied = connectStatus(proxy.URL,
		"unregistered.fixture:443",
		proxyToken,
		ca) == http.StatusForbidden
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("sproozi", proxyToken)
	env := []string{"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + directory,
		"XDG_CONFIG_HOME=" + filepath.Join(home,
			"config"),
		"XDG_DATA_HOME=" + filepath.Join(home,
			"data"),
		"XDG_CACHE_HOME=" + filepath.Join(home,
			"cache"),
		"XDG_STATE_HOME=" + filepath.Join(home,
			"state"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(home,
			".claude"),
		"NODE_EXTRA_CA_CERTS=" + caPath,
		"SSL_CERT_FILE=" + caPath,
		"HTTP_PROXY=" + proxyURL.String(),
		"HTTPS_PROXY=" + proxyURL.String(),
		"http_proxy=" + proxyURL.String(),
		"https_proxy=" + proxyURL.String(),
		"NO_PROXY=localhost,127.0.0.1",
		"no_proxy=localhost,127.0.0.1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
		"OPENCODE_DISABLE_AUTOUPDATE=true",
		"OPENCODE_DISABLE_MODELS_FETCH=true",
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		"OPENCODE_DISABLE_CLAUDE_CODE=true",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=true",
		"OPENCODE_DISABLE_DEFAULT_PLUGINS=true",
		"OPENCODE_DISABLE_SHARE=true",
		"OPENCODE_DISABLE_LSP_DOWNLOAD=true"}
	v := exec.Command(path, "--version")
	v.Dir = directory
	v.Env = env
	version, err := v.CombinedOutput()
	if err != nil {
		return out, err
	}
	out.Version = strings.TrimSpace(string(version))
	env, args, err := clientCommand(directory, selected, withTool, env)
	if err != nil {
		return out, err
	}
	commandPath := path
	if supervised {
		commandPath, args, err = supervisorCommand(directory, withTool)
		if err != nil {
			return out, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, commandPath, args...)
	process.WaitDelay = time.Second
	process.Dir = directory
	process.Env = env
	output, processErr := process.CombinedOutput()
	out.Output = string(output)
	if selected == opencodeHarness {
		list := exec.Command(path, "--pure", "session", "list", "--format", "json")
		list.Dir, list.Env = directory, env
		data, _ := list.Output()
		var sessions []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &sessions) == nil && len(sessions) == 1 {
			export := exec.Command(path, "--pure", "export", sessions[0].ID)
			export.Dir, export.Env = directory, env
			data, _ := export.Output()
			out.SessionExport = string(data)
		}
	}
	out.TimedOut = ctx.Err() != nil
	if process.ProcessState != nil {
		out.ExitCode = process.ProcessState.ExitCode()
	}
	return inspectResult(out, f, ledger, selected, scenario, withTool, processErr)
}

func supervisorCommand(directory string, withTool bool) (string, []string, error) {
	path, err := exec.LookPath("python3")
	if err != nil {
		return "", nil, err
	}
	launch := filepath.Join(directory, "opencode-launch.py")
	if err := os.WriteFile(launch, []byte(harness.OpenCodeLaunch), 0600); err != nil {
		return "", nil, err
	}
	request := filepath.Join(directory, "request.json")
	task := "Call the fixture verify MCP tool and report its exact result."
	if !withTool {
		task = "Reply with " + answer
	}
	data, _ := json.Marshal(map[string]any{"task": task})
	if err := os.WriteFile(request, data, 0600); err != nil {
		return "", nil, err
	}
	return path, []string{launch, modelFlag, "fixture-model", "--request", request}, nil
}

func connectStatus(proxyURL, authority, token string, ca []byte) int {
	endpoint, _ := url.Parse(proxyURL)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	connection, err := tls.Dial("tcp", endpoint.Host, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13})
	if err != nil {
		return 0
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	header := ""
	if token != "" {
		header = "Proxy-Authorization: Bearer " + token + "\r\n"
	}
	_, err = fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", authority, authority, header)
	if err != nil {
		return 0
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0
	}
	defer func() { _ = response.Body.Close() }()
	return response.StatusCode
}

//nolint:goconst // Match literal fields in the clients' native terminal records.
func completion(selected string, r result) (bool, string) {
	if selected == claudeHarness {
		var data struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal([]byte(r.Output), &data) != nil {
			return false, ""
		}
		return !data.IsError && data.Subtype == successScenario, data.Result
	}
	var data struct {
		Messages []struct {
			Info struct {
				Role   string          `json:"role"`
				Finish string          `json:"finish"`
				Error  json.RawMessage `json:"error"`
				Time   struct {
					Completed int64 `json:"completed"`
				} `json:"time"`
			} `json:"info"`
			Parts []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Reason string `json:"reason"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(r.SessionExport), &data) != nil || len(data.Messages) == 0 {
		return false, ""
	}
	last := data.Messages[len(data.Messages)-1]
	if last.Info.Role != "assistant" ||
		last.Info.Finish != "stop" ||
		len(last.Info.Error) > 0 ||
		last.Info.Time.Completed == 0 {
		return false, ""
	}
	var text strings.Builder
	stop := false
	for _, p := range last.Parts {
		if p.Type == "text" {
			text.WriteString(p.Text)
		}
		if p.Type == "step-finish" && p.Reason == "stop" {
			stop = true
		}
	}
	return stop, text.String()
}

func certificate() (tls.Certificate, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Sproozi local harness fixture"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"api.openai.com",
			"api.anthropic.com",
			"gateway.fixture"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(ca, private)
	return cert, ca, err
}

func clientCommand(directory, selected string, withTool bool, env []string) ([]string, []string, error) {
	files, err := harness.MCPConfig(selected, []string{"mcp.fixture"}, "https://gateway.fixture")
	if err != nil {
		return nil, nil, err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
			return nil, nil, err
		}
	}
	prompt := "Complete the fixture task. Call the fixture verify MCP tool and report its exact result."
	if !withTool {
		prompt = "Reply with " + answer
	}
	var args []string
	if selected == claudeHarness {
		env = append(env, "ANTHROPIC_API_KEY=sproozi-local-placeholder", "ANTHROPIC_BASE_URL=https://api.anthropic.com")
		args = []string{"--print",
			modelFlag,
			"claude-sonnet-4-5-20250929",
			"--output-format",
			"json",
			"--setting-sources",
			"",
			"--settings",
			`{"disableAllHooks":true}`,
			"--disable-slash-commands",
			"--strict-mcp-config",
			"--mcp-config",
			filepath.Join(directory,
				"claude-mcp.json"),
			"--no-session-persistence",
			"--dangerously-skip-permissions",
			prompt}
	} else {
		env = append(env, "OPENCODE_CONFIG="+filepath.Join(directory, "opencode-mcp.json"), `OPENCODE_CONFIG_CONTENT={
"enabled_providers":["sproozi"],
"provider":{"sproozi":{"npm":"@ai-sdk/openai",
	"options":{"baseURL":"https://api.openai.com/v1",
	"apiKey":"sproozi-local-placeholder"},

"models":{"fixture-model":{"name":"fixture-model","limit":{"context":200000,"output":8192}}}}},
"share":"disabled","autoupdate":false,"plugin":[],"lsp":false,"permission":"allow"}`)
		args = []string{"--pure", "run", modelFlag, "sproozi/fixture-model", "--format", "json", prompt}
	}
	return env, args, nil
}

func inspectResult(out result,
	f *fixture,
	ledger *budget.MemoryLedger,
	selected,
	scenario string,
	withTool bool,
	processErr error) (result,
	error) {
	f.mu.Lock()
	out.ModelRequests = f.requests
	out.ToolCalls = f.tools
	out.ReceivedToolResult = f.receivedToolResult
	out.AuthenticatedSessions = f.sessions
	violations := append([]string{}, f.violations...)
	f.mu.Unlock()
	state := ledger.State(context.Background(), budget.AccountKey("fixture-run/model.inference"))
	out.UnitsUsed = state.UnitsUsed
	out.ReservedUnits = state.ReservedUnits
	out.ToolUnitsUsed = ledger.State(context.Background(), budget.AccountKey("fixture-run/mcp.fixture")).UnitsUsed
	completed, response := completion(selected, out)
	if scenario == successScenario {
		out.Passed = processErr == nil &&
			!out.TimedOut &&
			completed &&
			response == answer &&
			out.ModelRequests > 0 &&
			out.AuthenticatedSessions > 0 &&
			out.UnitsUsed == int64(15*out.ModelRequests) &&
			out.ReservedUnits == 0 &&
			(!withTool ||
				(out.ToolCalls == 1 &&
					out.ToolUnitsUsed == 1 && out.ReceivedToolResult)) &&
			len(violations) == 0
	} else {
		out.Passed = !out.TimedOut &&
			!completed &&
			out.ModelRequests == 0 &&
			out.UnitsUsed == 0 &&
			out.AuthenticatedSessions > 0 &&
			(strings.Contains(out.Output+out.SessionExport,
				"capability_not_granted") ||
				strings.Contains(out.Output+out.SessionExport,
					"model.inference capability not granted"))
	}
	out.Passed = out.Passed && out.MissingIdentityDenied && out.UnknownAuthorityDenied
	if out.Mode == "supervised" {
		if scenario == successScenario {
			out.Passed = out.Passed && recoveredAnswer(out.Output) == answer
		} else {
			out.Passed = out.Passed && out.ExitCode > 0
		}
	}
	if !out.Passed {
		return out,
			fmt.Errorf("%s %s did not meet acceptance; violations=%v output=%s",
				selected,
				scenario,
				violations,
				out.Output)
	}
	return out, nil
}

func recoveredAnswer(output string) string {
	for line := range strings.SplitSeq(output, "\n") {
		var record struct {
			Type    string `json:"type"`
			Harness string `json:"harness"`
			Result  string `json:"result"`
		}
		if json.Unmarshal([]byte(line), &record) == nil &&
			record.Type == "sproozi_harness_completed" && record.Harness == opencodeHarness {
			return record.Result
		}
	}
	return ""
}

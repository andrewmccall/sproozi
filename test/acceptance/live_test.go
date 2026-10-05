//go:build live
// +build live

// The live suite deliberately has no fake clients or HTTP fixtures. It submits
// one real AgentRun to an already provisioned cluster and verifies the result
// through the Kubernetes API and the GitHub CLI. Provisioning credentials and
// images is intentionally outside this test so they cannot accidentally be
// committed to the repository.
package acceptance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	agentRunGroupVersion = "sproozi.com/v1alpha1"
	agentsNamespace      = "sproozi-agents"

	runUIDLabel = "sproozi.com/agentrun-uid"
)

var (
	githubRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	kubernetesName   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

type liveConfig struct {
	kubeconfig   string
	repository   string
	namespace    string
	template     string
	baseBranch   string
	task         string
	capabilities []string
	cniSelector  string
	timeout      time.Duration
	report       string
}

type runDocument struct {
	Metadata struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		TemplateRef struct {
			Name string `json:"name"`
		} `json:"templateRef"`
	} `json:"spec"`
	Status struct {
		Phase    string `json:"phase"`
		Identity struct {
			ServiceAccountName string `json:"serviceAccountName"`
			SandboxName        string `json:"sandboxName"`
		} `json:"identity"`
	} `json:"status"`
}

type templateDocument struct {
	Spec struct {
		RuntimeRef struct {
			Name string `json:"name"`
		} `json:"runtimeRef"`
		PolicyRef struct {
			Name string `json:"name"`
		} `json:"policyRef"`
	} `json:"spec"`
}

type policyDocument struct {
	Spec struct {
		KubernetesRead struct {
			Namespaces []string `json:"namespaces"`
		} `json:"kubernetesRead"`
		GitHubPullRequest struct {
			Repositories        []string `json:"repositories"`
			AllowedBaseBranches []string `json:"allowedBaseBranches"`
		} `json:"githubPullRequest"`
	} `json:"spec"`
}

type runtimeDocument struct {
	Spec struct {
		PodTemplate struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"podTemplate"`
	} `json:"spec"`
}

type podDocument struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ServiceAccountName           string `json:"serviceAccountName"`
		AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
		HostNetwork                  bool   `json:"hostNetwork"`
		HostPID                      bool   `json:"hostPID"`
		HostIPC                      bool   `json:"hostIPC"`
		SecurityContext              struct {
			SeccompProfile struct {
				Type string `json:"type"`
			} `json:"seccompProfile"`
		} `json:"securityContext"`
		Containers []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
			Env   []struct {
				Name      string          `json:"name"`
				Value     string          `json:"value"`
				ValueFrom json.RawMessage `json:"valueFrom"`
			} `json:"env"`
			SecurityContext struct {
				AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
				ReadOnlyRootFilesystem   *bool `json:"readOnlyRootFilesystem"`
				Privileged               *bool `json:"privileged"`
				SeccompProfile           struct {
					Type string `json:"type"`
				} `json:"seccompProfile"`
			} `json:"securityContext"`
		} `json:"containers"`
	} `json:"spec"`
}

type liveReport struct {
	Outcome              string            `json:"outcome"`
	RunName              string            `json:"runName"`
	RunUID               string            `json:"runUID,omitempty"`
	Repository           string            `json:"repository"`
	PRURL                string            `json:"prURL,omitempty"`
	PRCommit             string            `json:"prCommit,omitempty"`
	ObservedSandbox      bool              `json:"observedSandbox"`
	DenialProbesVerified bool              `json:"denialProbesVerified"`
	CleanupVerified      bool              `json:"cleanupVerified"`
	Commit               string            `json:"commit,omitempty"`
	WorkingTreeDirty     bool              `json:"workingTreeDirty"`
	ToolVersions         map[string]string `json:"toolVersions,omitempty"`
	ImageDigests         map[string]string `json:"imageDigests,omitempty"`
	AuditEvidence        []auditEvidence   `json:"auditEvidence,omitempty"`
	StartedAt            string            `json:"startedAt"`
	FinishedAt           string            `json:"finishedAt"`
	Error                string            `json:"error,omitempty"`
}

func TestLiveVerticalSlice(t *testing.T) {
	cfg, err := liveConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC()
	runName := liveRunName()
	report := liveReport{Outcome: "failed", RunName: runName, Repository: cfg.repository, StartedAt: started.Format(time.RFC3339)}
	writeReport := func() {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		if err := writeLiveReport(cfg.report, report); err != nil {
			t.Logf("could not write live report: %v", err)
		} else {
			t.Logf("redacted live report: %s", cfg.report)
		}
	}

	client := kubectlClient{config: cfg}
	if err := client.preflight(t); err != nil {
		report.Error = err.Error()
		writeReport()
		t.Fatal(err)
	}
	report.ToolVersions = collectToolVersions(cfg)
	report.Commit = repositoryCommit()
	report.WorkingTreeDirty = repositoryWorkingTreeDirty()

	tmpl, policy, err := client.validateInputs(t, cfg)
	if err != nil {
		report.Error = err.Error()
		writeReport()
		t.Fatal(err)
	}

	manifest, err := makeRunManifest(cfg, runName)
	if err != nil {
		report.Error = err.Error()
		writeReport()
		t.Fatal(err)
	}

	// Cleanup is intentionally scoped to this unique AgentRun. It never uses a
	// namespace-wide delete and never removes a provider PR automatically.
	var runUID string
	defer func() {
		cleanupErr := client.cleanup(t, cfg, runName, runUID, policy)
		report.CleanupVerified = cleanupErr == nil
		if cleanupErr != nil && report.Error == "" {
			report.Error = cleanupErr.Error()
		}
		if cleanupErr != nil {
			report.Outcome = "failed"
			t.Errorf("live cleanup failed: %v", cleanupErr)
		}
		writeReport()
	}()

	if err := client.apply(t, manifest); err != nil {
		report.Error = err.Error()
		t.Fatal(err)
	}

	run, observed, err := client.waitForRun(t, cfg, runName, cfg.timeout)
	report.ObservedSandbox = observed
	report.DenialProbesVerified = observed
	if run != nil {
		runUID = run.Metadata.UID
		report.RunUID = runUID
	}
	if err != nil {
		report.Error = err.Error()
		t.Fatal(err)
	}
	if !observed {
		err := errors.New("live run completed without observing its real sandbox Pod")
		report.Error = err.Error()
		t.Fatal(err)
	}
	if run.Status.Phase != "Succeeded" {
		err := fmt.Errorf("AgentRun %s completed in phase %q", runName, run.Status.Phase)
		report.Error = err.Error()
		t.Fatal(err)
	}
	pr, err := client.verifyPullRequest(t, cfg, run)
	if err != nil {
		report.Error = err.Error()
		t.Fatal(err)
	}
	report.PRURL, report.PRCommit = pr.URL, pr.HeadRefOID

	report.ImageDigests = client.collectImageDigests(t, cfg, tmpl)
	report.AuditEvidence = client.collectAuditEvidence(t, cfg, run.Metadata.UID)
	if err := validateDemoAudit(report.AuditEvidence, pr.URL); err != nil {
		report.Error = err.Error()
		t.Fatal(report.Error)
	}
	report.Outcome = "passed"
}

func liveConfigFromEnvironment() (liveConfig, error) {
	if os.Getenv("SPROOZI_LIVE_ACCEPT") != "1" {
		return liveConfig{}, errors.New("SPROOZI_LIVE_ACCEPT=1 is required; this test creates a real provider PR")
	}
	kubeconfig := os.Getenv("SPROOZI_KUBECONFIG")
	if kubeconfig == "" {
		return liveConfig{}, errors.New("SPROOZI_KUBECONFIG must name an explicit kubeconfig; refusing the default context")
	}
	info, err := os.Stat(kubeconfig)
	if err != nil || !info.Mode().IsRegular() {
		return liveConfig{}, fmt.Errorf("SPROOZI_KUBECONFIG is not a regular file: %q", kubeconfig)
	}
	repository := os.Getenv("SPROOZI_DEMO_REPO")
	if !githubRepository.MatchString(repository) {
		return liveConfig{}, fmt.Errorf("SPROOZI_DEMO_REPO must be owner/name, got %q", repository)
	}
	namespace := os.Getenv("SPROOZI_LIVE_NAMESPACE")
	template := os.Getenv("SPROOZI_LIVE_TEMPLATE")
	baseBranch := os.Getenv("SPROOZI_LIVE_BASE_BRANCH")
	task := os.Getenv("SPROOZI_LIVE_TASK")
	selector := os.Getenv("SPROOZI_CNI_SELECTOR")
	for key, value := range map[string]string{"SPROOZI_LIVE_NAMESPACE": namespace, "SPROOZI_LIVE_TEMPLATE": template, "SPROOZI_LIVE_BASE_BRANCH": baseBranch, "SPROOZI_LIVE_TASK": task, "SPROOZI_CNI_SELECTOR": selector} {
		if value == "" {
			return liveConfig{}, fmt.Errorf("%s is required for live acceptance", key)
		}
	}
	if !kubernetesName.MatchString(template) || len(template) > 253 {
		return liveConfig{}, fmt.Errorf("SPROOZI_LIVE_TEMPLATE is not a Kubernetes name: %q", template)
	}
	capabilities := splitList(os.Getenv("SPROOZI_LIVE_CAPABILITIES"))
	if len(capabilities) == 0 || !contains(capabilities, "github.pull_request") {
		return liveConfig{}, errors.New("SPROOZI_LIVE_CAPABILITIES must include github.pull_request")
	}
	seen := map[string]bool{}
	for _, capability := range capabilities {
		if capability == "" || strings.Contains(capability, "*") || seen[capability] {
			return liveConfig{}, fmt.Errorf("invalid or duplicate live capability %q", capability)
		}
		seen[capability] = true
	}
	timeout := 20 * time.Minute
	if raw := os.Getenv("SPROOZI_LIVE_TIMEOUT"); raw != "" {
		timeout, err = time.ParseDuration(raw)
		if err != nil || timeout <= 0 {
			return liveConfig{}, fmt.Errorf("invalid SPROOZI_LIVE_TIMEOUT %q", raw)
		}
	}
	report := os.Getenv("SPROOZI_LIVE_REPORT")
	if report == "" {
		report = filepath.Join(".local", "verification", "live-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	}
	return liveConfig{kubeconfig: kubeconfig, repository: repository, namespace: namespace, template: template, baseBranch: baseBranch, task: task, capabilities: capabilities, cniSelector: selector, timeout: timeout, report: report}, nil
}

type kubectlClient struct{ config liveConfig }

func (k kubectlClient) command(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	argv := append([]string{"--kubeconfig", k.config.kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", argv...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %s: %w", strings.Join(args, " "), redact(string(out)), err)
	}
	return out, nil
}

func (k kubectlClient) json(ctx context.Context, args ...string) ([]byte, error) {
	return k.command(ctx, nil, args...)
}

func (k kubectlClient) preflight(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	contextName, err := k.json(ctx, "config", "current-context")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(contextName)), "kind-") {
		return errors.New("live demo requires an explicit disposable Kind context")
	}
	cluster := strings.TrimPrefix(strings.TrimSpace(string(contextName)), "kind-")
	clusters, err := exec.CommandContext(ctx, "kind", "get", "clusters").Output()
	if err != nil || !contains(strings.Fields(string(clusters)), cluster) {
		return errors.New("live demo Kind cluster is not present locally")
	}
	if _, err := k.json(ctx, "version", "--request-timeout=15s"); err != nil {
		return fmt.Errorf("explicit kubeconfig cannot reach Kubernetes API: %w", err)
	}
	if _, err := k.json(ctx, "get", "namespace", k.config.namespace, "-o", "json"); err != nil {
		return fmt.Errorf("live namespace %q is unavailable: %w", k.config.namespace, err)
	}
	out, err := k.json(ctx, "get", "pods", "-n", "kube-system", "-l", k.config.cniSelector, "-o", "json")
	if err != nil {
		return fmt.Errorf("cannot inspect enforcing CNI selector: %w", err)
	}
	var list struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &list) != nil || len(list.Items) == 0 {
		return fmt.Errorf("CNI selector %q returned no Pods; refusing to claim network enforcement", k.config.cniSelector)
	}
	for _, pod := range list.Items {
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		if !ready {
			return fmt.Errorf("CNI selector %q has a non-ready Pod", k.config.cniSelector)
		}
	}
	return nil
}

func (k kubectlClient) validateInputs(t *testing.T, cfg liveConfig) (templateDocument, policyDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var tmpl templateDocument
	out, err := k.json(ctx, "get", "agenttemplate", cfg.template, "-n", cfg.namespace, "-o", "json")
	if err != nil {
		return tmpl, policyDocument{}, fmt.Errorf("get live AgentTemplate: %w", err)
	}
	if err := json.Unmarshal(out, &tmpl); err != nil {
		return tmpl, policyDocument{}, fmt.Errorf("decode live AgentTemplate: %w", err)
	}
	if tmpl.Spec.PolicyRef.Name == "" {
		return tmpl, policyDocument{}, errors.New("live AgentTemplate has no policyRef")
	}
	var policy policyDocument
	out, err = k.json(ctx, "get", "agentpolicy", tmpl.Spec.PolicyRef.Name, "-n", cfg.namespace, "-o", "json")
	if err != nil {
		return tmpl, policy, fmt.Errorf("get live AgentPolicy: %w", err)
	}
	if err := json.Unmarshal(out, &policy); err != nil {
		return tmpl, policy, fmt.Errorf("decode live AgentPolicy: %w", err)
	}
	if !containsFold(policy.Spec.GitHubPullRequest.Repositories, cfg.repository) || !contains(policy.Spec.GitHubPullRequest.AllowedBaseBranches, cfg.baseBranch) {
		return tmpl, policy, fmt.Errorf("live policy does not authorize repository %s and base %s", cfg.repository, cfg.baseBranch)
	}
	return tmpl, policy, nil
}

func (k kubectlClient) apply(t *testing.T, manifest []byte) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := k.command(ctx, manifest, "apply", "-f", "-")
	return err
}

func (k kubectlClient) waitForRun(t *testing.T, cfg liveConfig, name string, timeout time.Duration) (*runDocument, bool, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	observed := false
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := k.json(ctx, "get", "agentrun", name, "-n", cfg.namespace, "-o", "json")
		cancel()
		if err != nil {
			return nil, observed, err
		}
		var run runDocument
		if err := json.Unmarshal(out, &run); err != nil {
			return nil, observed, fmt.Errorf("decode AgentRun: %w", err)
		}
		if run.Metadata.UID == "" {
			return &run, observed, errors.New("API did not assign AgentRun UID")
		}
		if run.Status.Phase == "Running" && run.Status.Identity.SandboxName != "" && !observed {
			if err := k.observeSandbox(t, cfg, &run); err != nil {
				return &run, observed, err
			}
			observed = true
		}
		switch run.Status.Phase {
		case "Succeeded", "Failed", "TimedOut", "Cancelled":
			return &run, observed, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, observed, fmt.Errorf("AgentRun %s did not reach a terminal phase within %s", name, timeout)
}

func (k kubectlClient) observeSandbox(t *testing.T, cfg liveConfig, run *runDocument) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := k.json(ctx, "get", "pod", run.Status.Identity.SandboxName, "-n", agentsNamespace, "-o", "json")
	if err != nil {
		return fmt.Errorf("sandbox observation failed: %w", err)
	}
	var pod podDocument
	if err := json.Unmarshal(out, &pod); err != nil {
		return fmt.Errorf("decode sandbox: %w", err)
	}
	if pod.Metadata.Labels[runUIDLabel] != run.Metadata.UID || pod.Spec.ServiceAccountName != run.Status.Identity.ServiceAccountName || pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || pod.Spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		return errors.New("sandbox Pod did not satisfy immutable identity/network/security constraints")
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation == nil || *pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation || pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem == nil || !*pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem || (pod.Spec.Containers[0].SecurityContext.Privileged != nil && *pod.Spec.Containers[0].SecurityContext.Privileged) || pod.Spec.Containers[0].SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		return errors.New("sandbox container did not satisfy restricted security constraints")
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if (env.Name == "OPENAI_API_KEY" || env.Name == "GH_TOKEN") && env.Value == "sproozi-local-placeholder" && len(env.ValueFrom) == 0 {
			continue
		}
		if strings.Contains(strings.ToLower(env.Name), "key") || strings.Contains(strings.ToLower(env.Name), "token") || strings.Contains(strings.ToLower(env.Name), "secret") {
			return fmt.Errorf("sandbox contains forbidden credential environment variable %q", env.Name)
		}
	}
	if err := k.runNetworkDenialProbes(t, cfg, run, pod.Spec.Containers[0].Image); err != nil {
		return err
	}
	return nil
}

func (k kubectlClient) runNetworkDenialProbes(t *testing.T, cfg liveConfig, run *runDocument, image string) error {
	t.Helper()
	if image == "" {
		return errors.New("sandbox image is unavailable for denial probes")
	}
	for _, probe := range demoDenialProbes() {
		if err := k.runDenialProbe(t, cfg, run, image, probe.name, probe.audience, probe.command); err != nil {
			return err
		}
	}
	return nil
}

type denialProbe struct {
	name, audience, command string
}

func demoDenialProbes() []denialProbe {
	// Establish a working authenticated route before testing denials. An image
	// without curl, a broken proxy, bad trust or an inactive run must fail here.
	control := `request() { curl --silent --show-error --connect-timeout 5 --max-time 15 --noproxy '' --proxy https://sproozi-gateway.sproozi-system.svc:8443 --proxy-cacert /etc/sproozi/trust/ca-bundle.pem --cacert /etc/sproozi/trust/ca-bundle.pem --proxy-user "sproozi:$(cat /var/run/sproozi/token)" "$@"; }; code="$(request -o /dev/null -w '%{http_code}' https://kubernetes.default.svc/api/v1/namespaces/sproozi-demo/pods)"; test "$code" = 200; `
	return []denialProbe{
		{
			name:     "direct-network",
			audience: "sproozi-gateway",
			command:  control + `unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; rc=0; curl --silent --show-error --noproxy '*' --connect-timeout 5 --max-time 10 https://kubernetes.default.svc/version >/dev/null 2>&1 || rc=$?; printf 'direct-network curl exit=%s\n' "$rc"; test "$rc" = 28`,
		},
		{
			name:     "wrong-audience",
			audience: "sproozi-wrong-audience",
			command:  `code="$(curl --silent --show-error --connect-timeout 5 --max-time 10 --noproxy '' --proxy https://sproozi-gateway.sproozi-system.svc:8443 --proxy-cacert /etc/sproozi/trust/ca-bundle.pem --proxy-user "sproozi:$(cat /var/run/sproozi/token)" -o /dev/null -w '%{http_connect}' https://kubernetes.default.svc/api/v1/namespaces/sproozi-demo/pods 2>/dev/null || true)"; test "$code" = 401`,
		},
		{
			name:     "github-scope",
			audience: "sproozi-gateway",
			command:  control + `result="$(request -w ' HTTP_STATUS=%{http_code}' 'https://github.com/sproozi-denied/not-authorized.git/info/refs?service=git-upload-pack')"; case "$result" in *'repository not permitted by policy'*' HTTP_STATUS=403') ;; *) exit 42 ;; esac`,
		},
		{
			name: "kubernetes-secrets", audience: "sproozi-gateway",
			command: control + `result="$(request -w ' HTTP_STATUS=%{http_code}' https://kubernetes.default.svc/api/v1/namespaces/sproozi-demo/secrets)"; case "$result" in *'kubernetes request denied'*' HTTP_STATUS=403') ;; *) exit 42 ;; esac`,
		},
	}
}

func (k kubectlClient) runDenialProbe(t *testing.T, cfg liveConfig, run *runDocument, image, suffix, audience, command string) error {
	t.Helper()
	name := run.Metadata.Name + "-deny-" + suffix
	if len(name) > 63 {
		name = name[:63]
	}
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": agentsNamespace,
			"labels": map[string]string{"sproozi.com/agentrun": run.Metadata.Name, runUIDLabel: run.Metadata.UID, "sproozi.com/acceptance-probe": suffix},
		},
		"spec": map[string]any{
			"restartPolicy": "Never", "serviceAccountName": run.Status.Identity.ServiceAccountName, "automountServiceAccountToken": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "fsGroup": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []any{map[string]any{
				"name": "probe", "image": image, "command": []string{"/bin/sh", "-ceu", command},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "runAsNonRoot": true, "capabilities": map[string]any{"drop": []string{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "5m", "memory": "16Mi"}, "limits": map[string]string{"cpu": "100m", "memory": "64Mi"}},
				"volumeMounts":    []any{map[string]any{"name": "identity", "mountPath": "/var/run/sproozi", "readOnly": true}, map[string]any{"name": "trust", "mountPath": "/etc/sproozi/trust", "readOnly": true}},
			}},
			"volumes": []any{
				map[string]any{"name": "identity", "projected": map[string]any{"defaultMode": 256, "sources": []any{map[string]any{"serviceAccountToken": map[string]any{"audience": audience, "expirationSeconds": 600, "path": "token"}}}}},
				map[string]any{"name": "trust", "configMap": map[string]any{"name": "sproozi-sandbox-trust", "items": []any{map[string]any{"key": "ca-bundle.pem", "path": "ca-bundle.pem"}}}},
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer func() {
		_, _ = k.json(context.Background(), "delete", "pod", name, "-n", agentsNamespace, "--ignore-not-found", "--wait=false")
	}()
	if _, err := k.command(ctx, data, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create %s denial probe: %w", suffix, err)
	}
	if _, err := k.json(ctx, "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/"+name, "-n", agentsNamespace, "--timeout=60s"); err != nil {
		logs, _ := k.json(ctx, "logs", name, "-n", agentsNamespace)
		return fmt.Errorf("%s denial probe did not prove denial: %s: %w", suffix, redact(string(logs)), err)
	}
	return nil
}

func (k kubectlClient) verifyPullRequest(t *testing.T, cfg liveConfig, run *runDocument) (pullRequestDocument, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	head := "sproozi/" + run.Metadata.UID + "/fix-demo"
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", cfg.repository, "--state", "all", "--head", head,
		"--json", "number,url,headRefName,headRefOid,baseRefName,headRepository,files")
	out, err := cmd.Output()
	if err != nil {
		return pullRequestDocument{}, errors.New("could not read demo pull request with gh")
	}
	var prs []pullRequestDocument
	if err := json.Unmarshal(out, &prs); err != nil || len(prs) != 1 {
		return pullRequestDocument{}, errors.New("expected exactly one real pull request from the demo run branch")
	}
	if err := validateDemoPullRequest(prs[0], cfg.repository, cfg.baseBranch, run.Metadata.UID); err != nil {
		return pullRequestDocument{}, err
	}
	return prs[0], nil
}

func (k kubectlClient) collectImageDigests(t *testing.T, cfg liveConfig, tmpl templateDocument) map[string]string {
	t.Helper()
	images := make(map[string]string)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if tmpl.Spec.RuntimeRef.Name != "" {
		out, err := k.json(ctx, "get", "agentruntime", tmpl.Spec.RuntimeRef.Name, "-n", cfg.namespace, "-o", "json")
		if err == nil {
			var runtime runtimeDocument
			if json.Unmarshal(out, &runtime) == nil {
				for _, container := range runtime.Spec.PodTemplate.Spec.Containers {
					if strings.Contains(container.Image, "@sha256:") {
						images["agent/"+container.Name] = container.Image
					}
				}
			}
		}
	}
	for component, selector := range map[string]string{
		"controller": "control-plane=controller-manager",
		"gateway":    "app.kubernetes.io/component=shared-gateway",
	} {
		out, err := k.json(ctx, "get", "pods", "-n", cfg.namespace, "-l", selector, "-o", "json")
		if err != nil {
			continue
		}
		var list struct {
			Items []struct {
				Status struct {
					ContainerStatuses []struct {
						Name    string `json:"name"`
						ImageID string `json:"imageID"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(out, &list) != nil {
			continue
		}
		for _, item := range list.Items {
			for _, status := range item.Status.ContainerStatuses {
				if status.ImageID != "" {
					images[component+"/"+status.Name] = status.ImageID
				}
			}
		}
	}
	return images
}

func (k kubectlClient) collectAuditEvidence(t *testing.T, cfg liveConfig, runUID string) []auditEvidence {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := k.json(ctx, "logs", "deployment/sproozi-gateway", "-n", cfg.namespace)
	if err != nil {
		t.Logf("could not collect gateway audit evidence: %v", err)
		return nil
	}
	var evidence []auditEvidence
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		var event struct {
			RunUID string `json:"runUID"`
			auditEvidence
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.RunUID != runUID || event.Operation == "" {
			continue
		}
		evidence = append(evidence, event.auditEvidence)
	}
	return evidence
}

func (k kubectlClient) cleanup(t *testing.T, cfg liveConfig, runName, runUID string, policy policyDocument) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, _ = k.json(ctx, "delete", "agentrun", runName, "-n", cfg.namespace, "--ignore-not-found", "--wait=false")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := k.json(ctx, "get", "agentrun", runName, "-n", cfg.namespace, "-o", "name")
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "not found") {
				break
			}
			return fmt.Errorf("could not verify AgentRun deletion: %w", err)
		}
		_ = out
		time.Sleep(2 * time.Second)
	}
	if out, err := k.json(ctx, "get", "agentrun", runName, "-n", cfg.namespace, "-o", "name"); err == nil && strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("AgentRun %s was not deleted", runName)
	} else if err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
		return fmt.Errorf("could not verify AgentRun deletion: %w", err)
	}
	if runUID == "" {
		return errors.New("cannot verify cleanup without API-assigned run UID")
	}
	selector := runUIDLabel + "=" + runUID
	queries := [][]string{{"get", "pods,serviceaccounts,configmaps,networkpolicies", "-n", agentsNamespace, "-l", selector, "-o", "name"}, {"get", "roles,rolebindings,networkpolicies", "-n", cfg.namespace, "-l", selector, "-o", "name"}}
	for _, query := range queries {
		out, err := k.json(ctx, query...)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "" && !strings.Contains(strings.ToLower(string(out)), "no resources found") {
			return fmt.Errorf("run-owned resources remain after cleanup: %s", redact(string(out)))
		}
	}
	_ = policy // policy is retained only to keep cleanup scoped to validated input.
	return nil
}

func makeRunManifest(cfg liveConfig, name string) ([]byte, error) {
	manifest := map[string]any{"apiVersion": agentRunGroupVersion, "kind": "AgentRun", "metadata": map[string]any{"name": name, "namespace": cfg.namespace, "labels": map[string]string{"sproozi.com/acceptance": "live"}}, "spec": map[string]any{"templateRef": map[string]string{"name": cfg.template}, "task": cfg.task, "capabilities": cfg.capabilities, "eventContext": map[string]string{"trigger": "sproozi-live-acceptance", "namespace": cfg.namespace}}}
	return json.Marshal(manifest)
}

func liveRunName() string {
	return "sproozi-live-" + time.Now().UTC().Format("20060102t150405z") + "-" + strconv.Itoa(os.Getpid())
}
func splitList(raw string) []string {
	values := strings.Split(raw, ",")
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
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
	value = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`).ReplaceAllString(value, "<REDACTED>")
	if len(value) > 512 {
		return value[:512] + "..."
	}
	return value
}

func writeLiveReport(path string, report liveReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func collectToolVersions(cfg liveConfig) map[string]string {
	versions := make(map[string]string)
	commands := map[string][]string{
		"git":     {"git", "rev-parse", "HEAD"},
		"kubectl": {"kubectl", "version", "--client", "--output=json"},
		"kind":    {"kind", "version"},
		"docker":  {"docker", "version", "--format", "{{.Server.Version}}"},
		"gh":      {"gh", "version"},
	}
	for name, argv := range commands {
		cmd := exec.Command(argv[0], argv[1:]...)
		if name == "kubectl" {
			cmd.Args = append([]string{"kubectl", "--kubeconfig", cfg.kubeconfig}, argv[1:]...)
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			versions[name] = redact(string(out))
		}
	}
	return versions
}

func repositoryCommit() string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func repositoryWorkingTreeDirty() bool {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	return err != nil || len(bytes.TrimSpace(out)) != 0
}

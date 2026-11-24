package kubernetes_test

import (
	"slices"
	"strings"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

const (
	podOpenAIKey      = "OPENAI_API_KEY=" + testClientAuthPlaceholder
	podOpenAIBaseURL  = "OPENAI_BASE_URL=https://api.openai.com/v1"
	podGitHubToken    = "GH_TOKEN=" + testClientAuthPlaceholder
	testClaudeHarness = "claude-code"
	// secondWorkloadContainer is a second container the runtime names a
	// workload container.
	secondWorkloadContainer = "second"
)

// podCredentialEnvironment is what the controller gives an AgentRun Pod's
// workload containers for model and GitHub credentials, for each harness
// selector ("" is a runtime that names none), when the gateway holds the
// credential and when the harness signs in itself.
var podCredentialEnvironment = map[string]struct{ key, signIn []string }{
	"":                {key: []string{podOpenAIKey, podOpenAIBaseURL, podGitHubToken}, signIn: []string{podOpenAIBaseURL, podGitHubToken}},
	testCodexHarness:  {key: []string{podOpenAIKey, podOpenAIBaseURL, podGitHubToken}, signIn: []string{podOpenAIBaseURL, podGitHubToken}},
	testClaudeHarness: {key: []string{podOpenAIBaseURL, podGitHubToken}, signIn: []string{podOpenAIBaseURL, podGitHubToken}},
	"opencode":        {key: []string{podOpenAIBaseURL, podGitHubToken}, signIn: []string{podOpenAIBaseURL, podGitHubToken}},
	"copilot":         {key: []string{podOpenAIBaseURL, podGitHubToken}, signIn: []string{podOpenAIBaseURL, podGitHubToken}},
	"hermes":          {key: []string{podOpenAIBaseURL, podGitHubToken}, signIn: []string{podOpenAIBaseURL, podGitHubToken}},
}

func TestBuildPodSpecCredentialEnvironmentPerHarness(t *testing.T) {
	for _, selector := range append([]string{""}, harness.Selectors()...) {
		want, ok := podCredentialEnvironment[selector]
		if !ok {
			t.Errorf("harness %q has no row in podCredentialEnvironment", selector)
			continue
		}
		for _, signIn := range []bool{false, true} {
			rt := testRuntime()
			rt.Spec.ClientConfig.Harness = selector
			expected := want.key
			if signIn {
				rt.Spec.ClientConfig.SignIn = &sprooziv1alpha1.RuntimeSignIn{Claims: &sprooziv1alpha1.RuntimeLoginClaims{}}
				expected = want.signIn
			}
			// Two workload containers: each gets the same environment.
			second := *rt.Spec.PodTemplate.Spec.Containers[0].DeepCopy()
			second.Name = secondWorkloadContainer
			rt.Spec.PodTemplate.Spec.Containers = append(rt.Spec.PodTemplate.Spec.Containers, second)
			rt.Spec.WorkloadContainers = append(rt.Spec.WorkloadContainers, secondWorkloadContainer)
			run := testRunForSandbox("cccccccc-0000-0000-0000-000000000401")
			pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
			if len(pod.Spec.Containers) != 2 {
				t.Fatalf("Pod has %d containers, want both workload containers", len(pod.Spec.Containers))
			}
			for _, c := range pod.Spec.Containers {
				var got []string
				for _, env := range c.Env {
					if strings.Contains(env.Name, "KEY") || strings.Contains(env.Name, "TOKEN") ||
						strings.HasSuffix(env.Name, "_BASE_URL") {
						got = append(got, env.Name+"="+env.Value)
					}
				}
				if !slices.Equal(got, expected) {
					t.Errorf("harness %q, sign-in %v, container %s: credential environment %v, want %v",
						selector, signIn, c.Name, got, expected)
				}
			}
		}
	}
	for selector := range podCredentialEnvironment {
		if selector != "" && !slices.Contains(harness.Selectors(), selector) {
			t.Errorf("podCredentialEnvironment names %q, which is not a registered harness", selector)
		}
	}
}

package kubernetes_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentcontract"
	"github.com/andrewmccall/sproozi/internal/kubernetes"
)

func contractInputs() (*sprooziv1alpha1.AgentRun, *sprooziv1alpha1.AgentTemplate, *sprooziv1alpha1.AgentRuntime) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000200")
	run.Spec.Task = "Investigate the incident without changing production."
	run.Spec.EventContext = map[string]string{"alert": "api latency"}
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead}
	tmpl := &sprooziv1alpha1.AgentTemplate{Spec: sprooziv1alpha1.AgentTemplateSpec{Instructions: "Use only approved capabilities and record evidence."}}
	return run, tmpl, testRuntime()
}

func TestEnsureAgentContractCreatesImmutableSeparatedDocument(t *testing.T) {
	run, tmpl, rt := contractInputs()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if err := kubernetes.EnsureAgentContract(context.Background(), c, run, tmpl, rt); err != nil {
		t.Fatalf("EnsureAgentContract error: %v", err)
	}

	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}
	if err := c.Get(context.Background(), key, &configMap); err != nil {
		t.Fatalf("contract ConfigMap not found: %v", err)
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		t.Fatal("contract ConfigMap must be immutable")
	}
	input, err := agentcontract.DecodeInput([]byte(configMap.Data["input.json"]))
	if err != nil {
		t.Fatalf("stored contract was invalid: %v", err)
	}
	if input.Trusted.Instructions != tmpl.Spec.Instructions || input.Untrusted.Task != run.Spec.Task {
		t.Fatal("trusted instructions and untrusted task were not preserved in separate fields")
	}
	if input.Run.UID != string(run.UID) || configMap.Labels[kubernetes.RunUIDLabel] != string(run.UID) {
		t.Fatal("contract must bind the run UID in content and labels")
	}
	kubeconfigDocument := configMap.Data["kubeconfig"]
	if kubeconfigDocument == "" {
		t.Fatal("contract ConfigMap must contain a generated kubeconfig")
	}
	if strings.Contains(kubeconfigDocument, "client-key") || strings.Contains(kubeconfigDocument, "exec:") {
		t.Fatal("generated kubeconfig must not embed credentials or credential executors")
	}
	config, err := clientcmd.Load([]byte(kubeconfigDocument))
	if err != nil {
		t.Fatalf("generated kubeconfig is invalid: %v", err)
	}
	if config.Clusters[testKubeconfigContextName].Server != "https://kubernetes.default.svc" || config.Clusters[testKubeconfigContextName].CertificateAuthority != "/etc/sproozi/trust/ca-bundle.pem" {
		t.Fatalf("generated kubeconfig cluster = %#v", config.Clusters[testKubeconfigContextName])
	}
	if config.AuthInfos[testKubeconfigContextName].TokenFile != "" || config.AuthInfos[testKubeconfigContextName].Token != testClientAuthPlaceholder {
		t.Fatal("generated kubeconfig must contain only the harmless client placeholder")
	}
}

func TestEnsureAgentContractRejectsConflictingDocument(t *testing.T) {
	run, tmpl, rt := contractInputs()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: kubernetes.RunName(run.UID), Namespace: kubernetes.AgentsNamespace},
		Data:       map[string]string{"input.json": "{}"},
	}).Build()
	if err := kubernetes.EnsureAgentContract(context.Background(), c, run, tmpl, rt); err == nil {
		t.Fatal("expected conflicting ConfigMap to be rejected")
	}
}

func TestBuildPodSpecMountsContractReadOnly(t *testing.T) {
	run, _, rt := contractInputs()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
	var foundVolume, foundMount bool
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == testContractVolumeName && volume.ConfigMap != nil && volume.ConfigMap.Name == kubernetes.RunName(run.UID) {
			foundVolume = true
		}
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == testContractVolumeName && mount.MountPath == "/etc/sproozi/contract" && mount.ReadOnly {
			foundMount = true
		}
	}
	if !foundVolume || !foundMount {
		t.Fatal("Pod must mount its run contract as a read-only ConfigMap")
	}
}

package manifests_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// gatewayObjectReference matches the objects the gateway's startup errors name,
// such as "ConfigMap sproozi-system/sproozi-mcp-servers".
var gatewayObjectReference = regexp.MustCompile(`"(Secret|ConfigMap) ` + systemNamespace + `/([a-z0-9-]+)"`)

func TestGatewayStartupErrorsNameRenderedOrDocumentedObjects(t *testing.T) {
	root := repoRoot(t)
	source, err := os.ReadFile(filepath.Join(root, "cmd", "gateway", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	references := gatewayObjectReference.FindAllStringSubmatch(string(source), -1)
	if len(references) < 8 {
		t.Fatalf("expected the gateway to name its Secrets and ConfigMaps, found %v", references)
	}
	reference, err := os.ReadFile(filepath.Join(root, "docs", "reference", "required-secrets-and-config.md"))
	if err != nil {
		t.Fatal(err)
	}
	docs := renderDefault(t)
	gateway := renderedGatewayDeployment(t, docs)
	consumed := gatewayConsumedNames(t, docs, gateway)
	for _, match := range references {
		kind, name := match[1], match[2]
		if renderedObjectExists(docs, kind, name) {
			continue
		}
		// Objects the administrator creates are not rendered. They must keep
		// their unprefixed name in the gateway's references and be documented.
		if !slices.Contains(consumed[kind], name) {
			t.Errorf("gateway error names %s %s, but the rendered install neither ships it nor references it; "+
				"gateway references %v", kind, name, consumed[kind])
		}
		row := "| `" + name + "` | `" + systemNamespace + "` | `" + kind + "` |"
		if !strings.Contains(string(reference), row) {
			t.Errorf("gateway error names administrator-created %s %s, which required-secrets-and-config.md "+
				"does not list", kind, name)
		}
	}
}

func TestGatewayIsReadyOnlyAfterStartupOnPrivateHealthPort(t *testing.T) {
	docs := renderDefault(t)
	gateway := renderedGatewayDeployment(t, docs)
	container := gateway.Spec.Template.Spec.Containers[0]
	probe := container.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/readyz" {
		t.Fatalf("gateway needs an HTTP readiness probe on /readyz, got %+v", probe)
	}
	if probe.HTTPGet.Scheme != "" && probe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Fatalf("gateway readiness probe uses scheme %q", probe.HTTPGet.Scheme)
	}
	var healthPort int32
	for _, port := range container.Ports {
		if port.Name == probe.HTTPGet.Port.String() || port.ContainerPort == probe.HTTPGet.Port.IntVal {
			healthPort = port.ContainerPort
		}
	}
	if healthPort == 0 || healthPort == 8443 {
		t.Fatalf("readiness probe must use the separate health port, got %v on ports %+v",
			probe.HTTPGet.Port, container.Ports)
	}
	for _, service := range objectsOfKind(docs, "Service") {
		if metadataName(service) != sharedGatewayName {
			continue
		}
		var decoded corev1.Service
		decodeObject(t, service, &decoded)
		for _, port := range decoded.Spec.Ports {
			if port.TargetPort.IntVal == healthPort || port.TargetPort.StrVal == "health" {
				t.Fatalf("gateway Service exposes the health port: %+v", port)
			}
		}
	}
	// The default deny covers the health port; no policy that selects the
	// gateway may open it to other Pods. The kubelet probes from the node.
	gatewayLabels := labels.Set(gateway.Spec.Template.Labels)
	for _, doc := range objectsOfKind(docs, "NetworkPolicy") {
		var policy networkingv1.NetworkPolicy
		decodeObject(t, doc, &policy)
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		if !selector.Matches(gatewayLabels) {
			continue
		}
		for _, rule := range policy.Spec.Ingress {
			if len(rule.Ports) == 0 {
				t.Fatalf("NetworkPolicy %s opens every gateway port", policy.Name)
			}
			for _, port := range rule.Ports {
				if port.Port != nil && (port.Port.IntVal == healthPort || port.Port.StrVal == "health") {
					t.Fatalf("NetworkPolicy %s opens the gateway health port", policy.Name)
				}
			}
		}
	}
}

func renderedGatewayDeployment(t *testing.T, docs []map[string]any) appsv1.Deployment {
	t.Helper()
	for _, doc := range objectsOfKind(docs, "Deployment") {
		if metadataName(doc) == sharedGatewayName {
			var deployment appsv1.Deployment
			decodeObject(t, doc, &deployment)
			return deployment
		}
	}
	t.Fatalf("rendered default has no %s Deployment", sharedGatewayName)
	return appsv1.Deployment{}
}

func renderedObjectExists(docs []map[string]any, kind, name string) bool {
	for _, doc := range objectsOfKind(docs, kind) {
		if metadataName(doc) == name && metadataNamespace(doc) == systemNamespace {
			return true
		}
	}
	return false
}

// gatewayConsumedNames lists the Secrets and ConfigMaps the rendered gateway
// reads through its Pod spec or its namespaced RBAC.
func gatewayConsumedNames(t *testing.T, docs []map[string]any, gateway appsv1.Deployment) map[string][]string {
	t.Helper()
	names := map[string][]string{}
	spec := gateway.Spec.Template.Spec
	for _, volume := range spec.Volumes {
		if volume.Secret != nil {
			names["Secret"] = append(names["Secret"], volume.Secret.SecretName)
		}
		if volume.ConfigMap != nil {
			names["ConfigMap"] = append(names["ConfigMap"], volume.ConfigMap.Name)
		}
	}
	for _, container := range spec.Containers {
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				names["Secret"] = append(names["Secret"], ref.Name)
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				names["ConfigMap"] = append(names["ConfigMap"], ref.Name)
			}
		}
	}
	for _, doc := range objectsOfKind(docs, "Role") {
		if !strings.HasPrefix(metadataName(doc), "sproozi-shared-gateway") {
			continue
		}
		var role rbacv1.Role
		decodeObject(t, doc, &role)
		for _, rule := range role.Rules {
			for _, resource := range rule.Resources {
				switch resource {
				case string(corev1.ResourceSecrets):
					names["Secret"] = append(names["Secret"], rule.ResourceNames...)
				case string(corev1.ResourceConfigMaps):
					names["ConfigMap"] = append(names["ConfigMap"], rule.ResourceNames...)
				}
			}
		}
	}
	return names
}

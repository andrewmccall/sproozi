package manifests_test

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The optional certificate controller holds only what its lifecycle needs:
// get/update on its two precreated Secrets by name, and creation of public
// trust ConfigMaps in the agents namespace. It has no cluster-wide role.
func TestCertificateControllerRBACStaysNarrow(t *testing.T) {
	docs := renderKustomization(t, "config/certificates")
	if len(objectsOfKind(docs, "ClusterRole"))+len(objectsOfKind(docs, "ClusterRoleBinding")) != 0 {
		t.Fatal("certificate controller must not have cluster-wide RBAC")
	}
	secrets, configMaps := string(corev1.ResourceSecrets), string(corev1.ResourceConfigMaps)
	writes := strings.Fields("update patch delete deletecollection list watch")
	roles := objectsOfKind(docs, "Role")
	if len(roles) != 2 {
		t.Fatalf("expected two Roles, got %d", len(roles))
	}
	for _, role := range roles {
		namespace := role["metadata"].(map[string]any)["namespace"]
		for _, rule := range decodeRules(t, role) {
			switch namespace {
			case "sproozi-system":
				if !slices.Equal(rule.Resources, []string{secrets}) || !slices.Equal(rule.Verbs, []string{verbGet, writes[0]}) ||
					!slices.Equal(rule.ResourceNames, []string{"sproozi-certificate-authority", "sproozi-gateway-tls"}) {
					t.Fatalf("Secret access must be get/update by name only: %+v", rule)
				}
			case "sproozi-agents":
				if !slices.Equal(rule.Resources, []string{configMaps}) ||
					slices.ContainsFunc(rule.Verbs, func(v string) bool { return slices.Contains(writes, v) }) {
					t.Fatalf("trust access must create immutable ConfigMaps only: %+v", rule)
				}
			default:
				t.Fatalf("unexpected Role namespace %v", namespace)
			}
			for _, group := range rule.APIGroups {
				if group != "" {
					t.Fatalf("certificate controller must not reach API group %q, such as Sproozi policies", group)
				}
			}
		}
	}
	for _, secret := range objectsOfKind(docs, "Secret") {
		if _, ok := secret["data"]; ok {
			t.Fatalf("precreated Secret %s must carry no data so reapplying never resets it", metadataName(secret))
		}
	}
}

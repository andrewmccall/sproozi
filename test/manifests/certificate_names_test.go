package manifests_test

import (
	"path"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/andrewmccall/sproozi/internal/certificates"
)

// The gateway and the optional certificate controller share names that are Go
// constants on one side and manifest text on the other: the precreated
// Secrets, the gateway's own name and the files its serving Secret mounts.
// They must not drift.
func TestGatewayAndCertificateControllerAgreeOnNames(t *testing.T) {
	controller := renderKustomization(t, "config/certificates")
	shipped := objectsOfKind(controller, "Secret")
	secrets := make([]string, 0, len(shipped))
	for _, secret := range shipped {
		secrets = append(secrets, metadataName(secret))
	}
	slices.Sort(secrets)
	if want := []string{certificates.AuthoritySecretName, certificates.ServingSecretName}; !slices.Equal(secrets, want) {
		t.Errorf("config/certificates ships Secrets %v, want %v", secrets, want)
	}
	var args []string
	for _, doc := range objectsOfKind(controller, "Deployment") {
		var deployment appsv1.Deployment
		decodeObject(t, doc, &deployment)
		for _, container := range deployment.Spec.Template.Spec.Containers {
			args = append(args, container.Args...)
		}
	}
	for _, host := range []string{certificates.GatewayHost, certificates.GatewayHost + ".cluster.local"} {
		if !slices.Contains(args, "--dns="+host) {
			t.Errorf("certificate controller does not cover %s: args %v", host, args)
		}
	}

	for name, docs := range map[string][]map[string]any{"config/default": renderDefault(t),
		"config/install": renderInstall(t)} {
		spec := renderedGatewayDeployment(t, docs).Spec.Template.Spec
		env := map[string]string{}
		for _, variable := range spec.Containers[0].Env {
			env[variable.Name] = variable.Value
		}
		if got := env["SPROOZI_GATEWAY_HOST"]; got != certificates.GatewayHost {
			t.Errorf("%s: SPROOZI_GATEWAY_HOST = %q, want %q", name, got, certificates.GatewayHost)
		}
		mount := ""
		for _, volume := range spec.Volumes {
			if volume.Secret == nil || volume.Secret.SecretName != certificates.ServingSecretName {
				continue
			}
			for _, candidate := range spec.Containers[0].VolumeMounts {
				if candidate.Name == volume.Name {
					mount = candidate.MountPath
				}
			}
		}
		if mount == "" {
			t.Errorf("%s: gateway does not mount Secret %s", name, certificates.ServingSecretName)
		}
		if want := path.Join(mount, corev1.TLSCertKey); env["SPROOZI_GATEWAY_CERT_FILE"] != want {
			t.Errorf("%s: SPROOZI_GATEWAY_CERT_FILE = %q, want %q", name, env["SPROOZI_GATEWAY_CERT_FILE"], want)
		}
		if want := path.Join(mount, corev1.TLSPrivateKeyKey); env["SPROOZI_GATEWAY_KEY_FILE"] != want {
			t.Errorf("%s: SPROOZI_GATEWAY_KEY_FILE = %q, want %q", name, env["SPROOZI_GATEWAY_KEY_FILE"], want)
		}
		// The gateway reads certificates.CABundleKey from the directory of
		// its certificate file, so the Secret must mount whole, not by item.
		for _, volume := range spec.Volumes {
			if volume.Secret != nil && volume.Secret.SecretName == certificates.ServingSecretName &&
				len(volume.Secret.Items) > 0 {
				t.Errorf("%s: serving Secret is mounted by item, so %s would not reach the gateway",
					name, certificates.CABundleKey)
			}
		}
	}
}

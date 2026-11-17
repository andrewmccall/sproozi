package kubernetes

import (
	"context"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// gatewayTokenDirectory holds only the projected gateway token, at
// gatewayTokenPath.
const gatewayTokenDirectory = "/var/run/sproozi/tokens/gateway"

// gatewayTokenMount mounts the projected gateway token's directory. The
// kubelet refreshes a projected token by writing a new timestamped directory
// and swapping the volume's ..data symlink; only a directory mount sees the
// swap. A file mounted through subPath keeps the token the Pod started with.
func gatewayTokenMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: gatewayTokenVolumeName, MountPath: gatewayTokenDirectory, ReadOnly: true}
}

// earlierGatewayTokenMount is the subPath mount controllers before the
// directory mount built. Its token is never refreshed, so a Pod that has it
// loses the gateway when the token it started with expires.
func earlierGatewayTokenMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: gatewayTokenVolumeName, MountPath: gatewayTokenPath, SubPath: "token", ReadOnly: true}
}

// acceptEarlierGatewayTokenMount lets a Pod an earlier controller created
// match the current build. Where existing has exactly the earlier gateway
// token mount and wanted has exactly the current one at the same container
// and position, it puts the current mount in existing. existing must be a
// copy. Every other difference, in that mount or elsewhere, is left for the
// comparison to refuse. It reports whether it replaced a mount.
func acceptEarlierGatewayTokenMount(existing, wanted *corev1.PodSpec) bool {
	if len(existing.Containers) != len(wanted.Containers) {
		return false
	}
	earlier, current := earlierGatewayTokenMount(), gatewayTokenMount()
	replaced := false
	for i := range existing.Containers {
		e, w := existing.Containers[i].VolumeMounts, wanted.Containers[i].VolumeMounts
		if existing.Containers[i].Name != wanted.Containers[i].Name || len(e) != len(w) {
			continue
		}
		for j := range e {
			if reflect.DeepEqual(e[j], earlier) && reflect.DeepEqual(w[j], current) {
				e[j] = current
				replaced = true
			}
		}
	}
	return replaced
}

// validateExisting checks an existing sandbox with validateExistingSandbox
// and reports an adopted Pod that keeps the gateway token it started with.
func (p *PodSandboxClient) validateExisting(ctx context.Context, existing, wanted *corev1.Pod) error {
	earlier, err := validateExistingSandbox(existing, wanted)
	if err == nil && earlier {
		p.reportUnrefreshedToken(ctx, existing)
	}
	return err
}

// reportUnrefreshedToken logs, once per Pod and controller, that an adopted
// sandbox keeps the gateway token it started with.
func (p *PodSandboxClient) reportUnrefreshedToken(ctx context.Context, pod *corev1.Pod) {
	name := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if _, seen := p.unrefreshedTokens.LoadOrStore(name.String()+"/"+string(pod.UID), struct{}{}); seen {
		return
	}
	log.FromContext(ctx).Info("Adopted sandbox Pod whose gateway token is never refreshed",
		"pod", name, "created", pod.CreationTimestamp,
		"gatewayTokenExpiresAfter", (time.Duration(gatewayTokenExpirationSeconds) * time.Second).String())
}

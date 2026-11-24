package kubernetes

import (
	corev1 "k8s.io/api/core/v1"
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

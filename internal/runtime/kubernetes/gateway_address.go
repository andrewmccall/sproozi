package kubernetes

import (
	"context"
	"fmt"
	"net"
	"net/url"

	corev1 "k8s.io/api/core/v1"
)

// sandboxNameserver is the only resolver a sandbox is given. Nothing listens
// on it, so every lookup fails at once inside the Pod and no query reaches a
// cluster resolver that could forward it.
const sandboxNameserver = "127.0.0.1"

// GatewayResolver returns the address a sandbox must use for the gateway host.
// The controller resolves it, so the sandbox needs no DNS.
type GatewayResolver func(ctx context.Context, host string) (string, error)

// LookupGateway resolves host with the controller's own resolver and prefers
// an IPv4 address.
func LookupGateway(ctx context.Context, host string) (string, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			return addr.IP.String(), nil
		}
	}
	if len(addrs) > 0 {
		return addrs[0].IP.String(), nil
	}
	return "", fmt.Errorf("no address for %s", host)
}

// PinGateway removes DNS from the sandbox and maps the gateway host to a fixed
// address in its hosts file. Every outside request goes through the gateway,
// which the sandbox reaches by that address; a resolver would be a channel
// past the gateway's policy, audit and accounting.
func PinGateway(ctx context.Context, pod *corev1.Pod, endpoint string, resolve GatewayResolver) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("%w: gatewayEndpoint %q has no host", ErrInvalidRuntime, endpoint)
	}
	host := parsed.Hostname()
	pod.Spec.DNSPolicy = corev1.DNSNone
	pod.Spec.DNSConfig = &corev1.PodDNSConfig{Nameservers: []string{sandboxNameserver}}
	pod.Spec.HostAliases = nil
	if net.ParseIP(host) != nil {
		return nil
	}
	address, err := resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve gateway host %s: %w", host, err)
	}
	if net.ParseIP(address) == nil {
		return fmt.Errorf("resolve gateway host %s: %q is not an IP address", host, address)
	}
	pod.Spec.HostAliases = []corev1.HostAlias{{IP: address, Hostnames: []string{host}}}
	return nil
}

package manifests_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The gateway dials providers by name with Go's resolver. A lost DNS query
// costs one resolver timeout, 5 s by Kubernetes' default, so the gateway
// sets 1 s with 3 attempts. It must not change which names resolve: no
// ndots, search list, nameservers or DNS policy of its own
// (cmd/gateway/upstream_dns_test.go measures each setting).
func TestGatewayResolverBoundsALostDNSDatagram(t *testing.T) {
	for name, docs := range map[string][]map[string]any{"config/default": renderDefault(t),
		"config/install": renderInstall(t)} {
		spec := renderedGatewayDeployment(t, docs).Spec.Template.Spec
		if spec.DNSPolicy != "" && spec.DNSPolicy != corev1.DNSClusterFirst {
			t.Errorf("%s: gateway dnsPolicy %q, want the cluster's", name, spec.DNSPolicy)
		}
		config := spec.DNSConfig
		if config == nil {
			t.Fatalf("%s: gateway has no dnsConfig; a lost DNS query would stall a model call for 5 s", name)
		}
		if len(config.Nameservers) > 0 || len(config.Searches) > 0 {
			t.Errorf("%s: gateway dnsConfig changes nameservers %v or searches %v", name, config.Nameservers,
				config.Searches)
		}
		options := map[string]string{}
		for _, option := range config.Options {
			if option.Value == nil {
				t.Errorf("%s: gateway resolver option %s has no value", name, option.Name)
				continue
			}
			options[option.Name] = *option.Value
		}
		want := map[string]string{"timeout": "1", "attempts": "3"}
		if len(options) != len(want) || options["timeout"] != want["timeout"] ||
			options["attempts"] != want["attempts"] {
			t.Errorf("%s: gateway resolver options %v, want exactly %v", name, options, want)
		}
	}
}

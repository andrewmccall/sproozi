package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestControllerReadsConfigMapsWithoutClusterWideInformer(t *testing.T) {
	options := controllerClientOptions()
	if options.Cache != nil {
		for _, object := range options.Cache.DisableFor {
			if _, ok := object.(*corev1.ConfigMap); ok {
				return
			}
		}
	}
	t.Fatal("ConfigMap reads would start a cluster-wide informer despite namespace-only get/create/delete RBAC")
}

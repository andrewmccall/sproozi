//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/andrewmccall/sproozi/test/utils"
)

// managerImage is the manager image to be built and loaded for testing.
// hack/verify/kind.sh names it with SPROOZI_E2E_MANAGER_IMAGE when it builds
// the image itself.
var managerImage = envOr("SPROOZI_E2E_MANAGER_IMAGE", "example.com/sproozi:v0.0.1")

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting sproozi e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	configureKubectlKubeRC()
	// hack/verify/kind.sh builds the images while the cluster starts, loads
	// them and sets SPROOZI_E2E_IMAGES_LOADED=1. Other callers build here.
	if os.Getenv("SPROOZI_E2E_IMAGES_LOADED") == "1" {
		_, _ = fmt.Fprintf(GinkgoWriter, "Using images built and loaded by hack/verify/kind.sh\n")
		return
	}

	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	if os.Getenv("SPROOZI_E2E_ORCHESTRATION") != "0" {
		By("building and loading default native orchestration acceptance clients")
		cmd = exec.Command("python3", "hack/verify/orchestration-kind/build.py")
		_, err = utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to provision native acceptance images")
	}
})

// disposableCluster reports whether the caller deletes the cluster as soon as
// the suite ends. hack/verify/kind.sh sets SPROOZI_E2E_DISPOSABLE_CLUSTER=1, so
// teardown that only removes resources is skipped. make test-e2e-existing runs
// against a caller-owned cluster and still cleans up.
func disposableCluster() bool {
	return os.Getenv("SPROOZI_E2E_DISPOSABLE_CLUSTER") == "1"
}

// Disable kubectl kuberc by default for test isolation.
// This prevents local kubectl configurations from affecting test behavior.
// To enable kuberc, set: KUBECTL_KUBERC=true
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}

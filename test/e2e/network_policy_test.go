//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/andrewmccall/sproozi/test/utils"
)

// This checks standard Kubernetes NetworkPolicy, not a provider's CRDs or API.
// A Ready CNI Pod alone is not evidence of packet filtering.
var _ = Describe("NetworkPolicy enforcement", func() {
	It("blocks an established destination and restores access after policy removal", func() {
		image := os.Getenv("SPROOZI_E2E_PROBE_IMAGE")
		if image == "" {
			image = "curlimages/curl@sha256:94e9e444bcba979c2ea12e27ae39bee4cd10bc7041a472c4727a558e213744e6"
		}
		Expect(image).To(ContainSubstring("@sha256:"), "Provide a digest-pinned curl probe image")
		const probeName = "network-policy-probe"
		const probeNamespace = "sproozi-network-test"
		_, err := utils.Run(exec.Command("kubectl", "create", "namespace", probeNamespace))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, err := utils.Run(exec.Command("kubectl", "delete", "namespace", probeNamespace, "--wait=true"))
			Expect(err).NotTo(HaveOccurred())
		})

		no, yes, uid := false, true, int64(1000)
		pod := corev1.Pod{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: probeName, Namespace: probeNamespace,
				Labels: map[string]string{"app": probeName}},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &no,
				RestartPolicy:                corev1.RestartPolicyNever,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: &yes, RunAsUser: &uid,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name: "probe", Image: image, Command: []string{"/bin/sh", "-c", "sleep 900"},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
				}},
			},
		}
		applyNetworkFixture(pod)
		_, err = utils.Run(exec.Command("kubectl", "wait", "pod/"+probeName, "-n", probeNamespace,
			"--for=condition=Ready", "--timeout=180s"))
		Expect(err).NotTo(HaveOccurred())
		apiIP, err := utils.Run(exec.Command("kubectl", "get", "service", "kubernetes", "-n", "default",
			"-o", "jsonpath={.spec.clusterIP}"))
		Expect(err).NotTo(HaveOccurred())
		url := fmt.Sprintf("https://%s/version", strings.TrimSpace(apiIP))
		curl := []string{"curl", "--noproxy", "*", "--silent", "--show-error", "--insecure",
			"--connect-timeout", "3", "--max-time", "5", "-o", "/dev/null", "-w", "%{http_code}", url}
		request := func() (string, error) {
			args := append([]string{"exec", probeName, "-n", probeNamespace, "--"}, curl...)
			out, err := utils.Run(exec.Command("kubectl", args...))
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Probe HTTP status: %s\n", strings.TrimSpace(out))
			}
			return out, err
		}
		By("proving that the image, destination and route work before filtering")
		Eventually(request, 30*time.Second, time.Second).Should(Equal("200"))
		applyNetworkFixture(networkingv1.NetworkPolicy{
			TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
			ObjectMeta: metav1.ObjectMeta{Name: probeName, Namespace: probeNamespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": probeName}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		})
		By("requiring a connection timeout rather than a DNS, binary or HTTP failure")
		Eventually(func() error {
			args := []string{"exec", probeName, "-n", probeNamespace, "--", "/bin/sh", "-c",
				`rc=0
"$@" >/dev/null 2>&1 || rc=$?
printf 'curl exit code: %s\n' "$rc"
test "$rc" = 28`, "probe"}
			out, err := utils.Run(exec.Command("kubectl", append(args, curl...)...))
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Blocked probe %s", out)
			}
			return err
		}, 30*time.Second, time.Second).Should(Succeed())
		_, err = utils.Run(exec.Command("kubectl", "delete", "networkpolicy", probeName, "-n", probeNamespace))
		Expect(err).NotTo(HaveOccurred())
		By("proving that removing only the policy restores the same connection")
		Eventually(request, 30*time.Second, time.Second).Should(Equal("200"))
	})
})

func applyNetworkFixture(object any) {
	data, err := json.Marshal(object)
	Expect(err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(data)
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

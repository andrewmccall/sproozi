package certificates

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/pki"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var updateOutcomes = flag.Bool("update-outcomes", false, "rewrite testdata/reconcile-outcomes.txt from the current code")

const outcomesFile = "testdata/reconcile-outcomes.txt"

// Names of the built authority Secrets the tests share.
const (
	stateSingle   = "single"
	statePrepared = "prepared"
	stateSwitched = "switched"
)

// secretState is an authority Secret the recorded table starts from: one the
// controller wrote, or one a hand edit, a truncated restore or an
// interrupted write could leave.
type secretState struct {
	name  string
	build func(t *testing.T, c *Controller, client *fake.Clientset)
}

func reconcileOK(t *testing.T, c *Controller) {
	t.Helper()
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func step(t *testing.T, c *Controller, client *fake.Clientset, requests ...string) {
	t.Helper()
	reconcileOK(t, c)
	for _, r := range requests {
		request(t, client, r)
		reconcileOK(t, c)
	}
}

func authorityData(t *testing.T, client *fake.Clientset) map[string][]byte {
	t.Helper()
	s, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s.Data
}

func setAuthorityData(t *testing.T, client *fake.Clientset, edit func(map[string][]byte)) {
	t.Helper()
	secrets := client.CoreV1().Secrets(systemNamespace)
	s, err := secrets.Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	edit(s.Data)
	if _, err = secrets.Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func editStoredState(t *testing.T, client *fake.Clientset, edit func(st *pki.State)) {
	t.Helper()
	setAuthorityData(t, client, func(data map[string][]byte) {
		var st pki.State
		if err := json.Unmarshal(data["state.json"], &st); err != nil {
			t.Fatal(err)
		}
		edit(&st)
		raw, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		data["state.json"] = append(raw, '\n')
	})
}

func secretStates() []secretState {
	return []secretState{
		{stateSingle, func(t *testing.T, c *Controller, client *fake.Clientset) { step(t, c, client) }},
		{statePrepared, func(t *testing.T, c *Controller, client *fake.Clientset) { step(t, c, client, requestPrepare) }},
		{stateSwitched, func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare, requestSwitch)
		}},
		{"retired", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare, requestSwitch, requestRetire)
		}},
		// RetireRotation committed in scratch space, the key's removal failed,
		// and the controller then committed the scratch space to the Secret.
		{"retired-key-left", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare, requestSwitch)
			key := authorityData(t, client)["ca-1.key"]
			request(t, client, requestRetire)
			reconcileOK(t, c)
			setAuthorityData(t, client, func(data map[string][]byte) { data["ca-1.key"] = key })
		}},
		{"replaced-key-left", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client)
			key := authorityData(t, client)["ca-1.key"]
			request(t, client, requestReplace)
			reconcileOK(t, c)
			setAuthorityData(t, client, func(data map[string][]byte) { data["ca-1.key"] = key })
		}},
		{"truncated", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client)
			setAuthorityData(t, client, func(data map[string][]byte) {
				data["state.json"] = data["state.json"][:len(data["state.json"])/2]
			})
		}},
		{"stage-unknown", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare)
			editStoredState(t, client, func(st *pki.State) { st.Rotation.Stage = "bogus" })
		}},
		{"rotation-dropped", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare)
			editStoredState(t, client, func(st *pki.State) { st.Rotation = nil })
		}},
		{"serving-not-trusted", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare)
			editStoredState(t, client, func(st *pki.State) { st.Trusted = []int{2} })
		}},
		{"prepared-serving-new", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare)
			editStoredState(t, client, func(st *pki.State) { st.Serving = 2 })
		}},
		{"prepared-trusted-one", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare)
			editStoredState(t, client, func(st *pki.State) { st.Trusted = []int{1} })
		}},
		{"generation-not-retired-untrusted", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client, requestPrepare)
			editStoredState(t, client, func(st *pki.State) { st.Trusted, st.Rotation = []int{1}, nil })
		}},
		{"ca-pair-other", func(t *testing.T, c *Controller, client *fake.Clientset) {
			step(t, c, client)
			ca, err := pki.NewCA("hand-made", time.Now(), pki.DefaultPolicy.CAValidity)
			if err != nil {
				t.Fatal(err)
			}
			setAuthorityData(t, client, func(data map[string][]byte) {
				data["ca-1.pem"], data["ca-1.key"] = ca.Certificate, ca.Key
			})
		}},
	}
}

// cloneCluster copies the Secrets and trust ConfigMaps into a new fake
// client, so each request starts from the same built state.
func cloneCluster(t *testing.T, from *fake.Clientset) *fake.Clientset {
	t.Helper()
	ctx := context.Background()
	secrets, err := from.CoreV1().Secrets(systemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cms, err := from.CoreV1().ConfigMaps("sproozi-agents").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientset()
	for i := range secrets.Items {
		s := secrets.Items[i].DeepCopy()
		s.ResourceVersion = ""
		if _, err = client.CoreV1().Secrets(systemNamespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range cms.Items {
		cm := cms.Items[i].DeepCopy()
		cm.ResourceVersion = ""
		if _, err = client.CoreV1().ConfigMaps("sproozi-agents").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return client
}

var (
	controllerLeafHex = regexp.MustCompile(`leaf-[0-9a-f]{16}`)
	scratchPath       = regexp.MustCompile(`/[^ ]*/store-[0-9a-f]+`)
)

func normalize(msg string) string {
	return scratchPath.ReplaceAllString(controllerLeafHex.ReplaceAllString(msg, "leaf-X"), "$$SCRATCH")
}

// describeCluster summarises what the gateway, the operator and the
// sandboxes can see: the authority Secret's CA keys and stage, the status
// annotations, whether the serving Secret changed and which trust
// ConfigMaps exist.
func describeCluster(t *testing.T, client *fake.Clientset, servingBefore []byte) string {
	t.Helper()
	s, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-certificate-authority", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	leaves := 0
	for k := range s.Data {
		switch {
		case strings.HasPrefix(k, "ca-"):
			keys = append(keys, k)
		case strings.HasPrefix(k, "leaf-"):
			leaves++
		}
	}
	slices.Sort(keys)
	stage := "unreadable"
	var st pki.State
	if json.Unmarshal(s.Data["state.json"], &st) == nil {
		stage = noRotation
		if st.Rotation != nil {
			stage = fmt.Sprintf("%s:%d>%d", st.Rotation.Stage, st.Rotation.From, st.Rotation.To)
		}
		stage += fmt.Sprintf(" serving=%d trusted=%v", st.Serving, st.Trusted)
	}
	serving, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-gateway-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	servingChange := "kept"
	if !bytes.Equal(serving.Data[corev1.TLSCertKey], servingBefore) {
		servingChange = "changed"
	}
	cms, err := client.CoreV1().ConfigMaps("sproozi-agents").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	trust := make([]string, 0, len(cms.Items))
	for _, cm := range cms.Items {
		trust = append(trust, cm.Name)
	}
	slices.Sort(trust)
	a := s.Annotations
	return fmt.Sprintf("state=%s ca=%v leafFiles=%d | request=%q stage-annotation=%q trust-annotation=%q last-error=%q | serving=%s | trust=%v",
		stage, keys, leaves, a[RotationRequest], a[RotationStage], a[TrustAnnotation],
		normalize(a[LastError]), servingChange, trust)
}

// Every rotation request, from every authority Secret in secretStates, has
// the outcome recorded in testdata/reconcile-outcomes.txt. The file was
// first generated from the controller before the store's rotation record
// adopted ADR 0058's pattern; each later difference is a deliberate change
// in its own commit.
func TestReconcileOutcomesMatchTheRecordedTable(t *testing.T) {
	lines := make([]string, 0, 128)
	for _, state := range secretStates() {
		c, built := setup(t)
		state.build(t, c, built)
		for _, req := range []string{"", requestRenew, requestPrepare, requestSwitch, requestRollback, requestRetire, requestReplace} {
			client := cloneCluster(t, built)
			if req != "" {
				request(t, client, req)
			}
			before, err := client.CoreV1().Secrets(systemNamespace).Get(context.Background(), "sproozi-gateway-tls", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// A restarted controller with fresh scratch space.
			_, err = controller(t, client).Reconcile(context.Background())
			result := "ok"
			if err != nil {
				result = "error: " + normalize(err.Error())
			}
			lines = append(lines, fmt.Sprintf("%s | request %q -> %s | %s", state.name, req, result,
				describeCluster(t, client, before.Data[corev1.TLSCertKey])))
		}
	}
	got := strings.Join(lines, "\n") + "\n"
	if *updateOutcomes {
		if err := os.MkdirAll(filepath.Dir(outcomesFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outcomesFile, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(outcomesFile)
	if err != nil {
		t.Fatal(err)
	}
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d outcomes, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("outcome changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}

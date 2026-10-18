package envtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// The package shares one API server. Validation tests run in parallel, each in
// its own namespace; controller tests share one manager and run serially.
var (
	testConfig *rest.Config
	testScheme = k8sruntime.NewScheme()
	testClient client.Client
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if dir, ok := findLocalKubebuilderAssets(); ok {
			_ = os.Setenv("KUBEBUILDER_ASSETS", dir)
		}
	}
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "clientgoscheme.AddToScheme() error = %v\n", err)
		return 1
	}
	if err := sprooziv1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "AddToScheme() error = %v\n", err)
		return 1
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	var err error
	if testConfig, err = env.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Start() error = %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "Stop() error = %v\n", err)
		}
	}()
	if testClient, err = client.New(testConfig, client.Options{Scheme: testScheme}); err != nil {
		fmt.Fprintf(os.Stderr, "New() error = %v\n", err)
		return 1
	}
	defer stopAgentRunController()
	return m.Run()
}

// newNamespace creates a namespace private to the calling test, so tests that
// share the API server never see each other's objects.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "envtest-"}}
	if err := testClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("Create namespace error = %v", err)
	}
	return ns.Name
}

func findLocalKubebuilderAssets() (string, bool) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	entries, err := filepath.Glob(filepath.Join(root, "bin", "k8s", "*"))
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if hasRequiredEnvtestBinaries(entry) {
			return entry, true
		}
	}
	return "", false
}

func hasRequiredEnvtestBinaries(dir string) bool {
	for _, name := range []string{"etcd", "kube-apiserver", "kubectl"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

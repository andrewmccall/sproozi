package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const twoContexts = `apiVersion: v1
kind: Config
current-context: admin
clusters:
- name: kind
  cluster: {server: "https://127.0.0.1:6443"}
users:
- name: cli
  user: {token: "fixture-token-0123456789abcdef0123456789"}
contexts:
- name: admin
  context: {cluster: kind, user: cli}
- name: sproozi
  context: {cluster: kind, user: cli, namespace: sproozi-system}
`

const fileOnly = `apiVersion: v1
kind: Config
current-context: kind-qa
clusters:
- name: kind
  cluster: {server: "https://127.0.0.1:6443"}
users:
- name: cli
  user: {token: "fixture-token-0123456789abcdef0123456789"}
contexts:
- name: kind-qa
  context: {cluster: kind, user: cli, namespace: team-a}
`

func writeConfig(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSelectResolvesContextsFilesAndNamespaces(t *testing.T) {
	dir := t.TempDir()
	defaults := writeConfig(t, dir, "config", twoContexts)
	file := writeConfig(t, dir, "qa.kubeconfig", fileOnly)
	t.Setenv("KUBECONFIG", defaults)
	t.Chdir(dir)
	for _, tc := range []struct {
		name, context, namespace string
		want                     Selection
	}{
		{"default sproozi context", "", "", Selection{[]string{defaults}, DefaultContext, "sproozi-system"}},
		{"namespace overrides context", "", "team-b", Selection{[]string{defaults}, DefaultContext, "team-b"}},
		{"named context without namespace", adminContext, "", Selection{[]string{defaults}, adminContext, DefaultNamespace}},
		{"file uses its current context", file, "", Selection{[]string{file}, "kind-qa", "team-a"}},
		{"relative file becomes absolute", "qa.kubeconfig", "ns", Selection{[]string{file}, "kind-qa", "ns"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(tc.context, tc.namespace)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got.Kubeconfig, ",") != strings.Join(tc.want.Kubeconfig, ",") || got.Context != tc.want.Context || got.Namespace != tc.want.Namespace {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if err = got.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSelectPrefersAContextOverAFileOfTheSameName(t *testing.T) {
	dir := t.TempDir()
	defaults := writeConfig(t, dir, "config", twoContexts)
	writeConfig(t, dir, "admin", fileOnly)
	t.Setenv("KUBECONFIG", defaults)
	t.Chdir(dir)
	got, err := Select("admin", "")
	if err != nil || got.Context != "admin" || got.Kubeconfig[0] != defaults {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestSelectExplainsEachFailure(t *testing.T) {
	dir := t.TempDir()
	defaults := writeConfig(t, dir, "config", twoContexts)
	noSproozi := writeConfig(t, dir, "plain", fileOnly)
	noCurrent := writeConfig(t, dir, "nocurrent", strings.Replace(fileOnly, "current-context: kind-qa\n", "", 1))
	broken := writeConfig(t, dir, "broken", "not: [valid")
	badNamespace := writeConfig(t, dir, "badns", strings.Replace(fileOnly, "namespace: team-a", "namespace: Team_A", 1))
	t.Chdir(dir)
	for _, tc := range []struct {
		name, kubeconfig, context, namespace string
		says                                 []string
	}{
		{"no sproozi context", noSproozi, "", "", []string{`no context named "sproozi"`, "docs/reference/remote-executors.md", "--context"}},
		{"neither context nor file", defaults, "prod", "", []string{`--context "prod" is neither a context`, "admin, sproozi", "kubeconfig file"}},
		{"file without current context", defaults, noCurrent, "", []string{"read as the kubeconfig file", "sets no current context", "use-context"}},
		{"file that is not a kubeconfig", defaults, broken, "", []string{"read as a kubeconfig file", "not a valid kubeconfig"}},
		{"invalid namespace flag", defaults, "", "Bad_NS", []string{`--namespace "Bad_NS"`}},
		{"invalid context namespace", defaults, badNamespace, "", []string{`namespace "Team_A"`, "pass --namespace"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tc.kubeconfig)
			_, err := Select(tc.context, tc.namespace)
			if err == nil {
				t.Fatal("selection succeeded")
			}
			for _, s := range tc.says {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not say %q", err, s)
				}
			}
		})
	}
}

func TestRecordedSelectionMustBeAbsoluteAndComplete(t *testing.T) {
	err := Selection{Kubeconfig: []string{"relative"}, Namespace: "Bad"}.Validate()
	for _, s := range []string{"not an absolute path", "context:", "namespace:"} {
		if err == nil || !strings.Contains(err.Error(), s) {
			t.Errorf("error %v does not say %q", err, s)
		}
	}
}

func TestRecordedContextThatDisappearsNamesTheFix(t *testing.T) {
	dir := t.TempDir()
	file := writeConfig(t, dir, "qa", fileOnly)
	_, err := Selection{Kubeconfig: []string{file}, Context: "gone", Namespace: "ns"}.restConfig()
	if err == nil || !strings.Contains(err.Error(), `context "gone" is no longer`) || !strings.Contains(err.Error(), "--context") {
		t.Fatalf("got %v", err)
	}
}

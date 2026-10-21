// Package remote reaches a remote executor through a kubeconfig context. The
// developer's own Kubernetes identity finds the executor's Service in a
// namespace, reads its access Secret and opens a port-forward tunnel to its
// Pod; the profile protocol then runs over that tunnel with the same TLS and
// home principal token as before. See ADR 0034.
package remote

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// DefaultNamespace is used, as kubectl does, when neither --namespace nor the
// context names one.
const DefaultNamespace = "default"

// Selection names a remote executor the way this developer reaches it: a
// kubeconfig context and a namespace. Home keeps it after the CLI exits.
// Kubeconfig holds absolute files in precedence order and Context is pinned,
// so neither home's own environment nor a later "kubectl config use-context"
// changes which cluster a recorded executor is in.
type Selection struct {
	Kubeconfig []string `json:"kubeconfig"`
	Context    string   `json:"context"`
	Namespace  string   `json:"namespace"`
}

// DefaultContext is the kubeconfig context used when --context is omitted. It
// signs in as the ServiceAccount CLIServiceAccount in the executor's namespace;
// docs/reference/remote-executors.md shows how to create it.
const DefaultContext = "sproozi"

// Select resolves --context and --namespace in the caller's environment, without
// contacting any cluster.
//
// An empty context selects the context named "sproozi" in the default
// kubeconfig (KUBECONFIG, else ~/.kube/config). Otherwise the value is a context
// name when the default kubeconfig defines a context with exactly that name,
// and is otherwise a kubeconfig file path when it names an existing file, whose
// current context is used. A context name wins over a file of the same name, so
// a stray file in the working directory never hides a context. The namespace is
// --namespace, else the context's namespace, else "default".
func Select(context, namespace string) (Selection, error) {
	if namespace != "" {
		if problem := namespaceProblem(namespace); problem != "" {
			return Selection{}, fmt.Errorf("--namespace %q %s", namespace, problem)
		}
	}
	files := defaultFiles()
	loaded, loadErr := load(files)
	if context == "" {
		if loadErr != nil {
			return Selection{}, fmt.Errorf("cannot read the kubeconfig %s: %w; fix the file, set KUBECONFIG, or pass --context FILE",
				describeFiles(files), loadErr)
		}
		if !slices.ContainsFunc(files, exists) {
			return Selection{}, fmt.Errorf("there is no kubeconfig file at %s, so the default context %q does not "+
				"exist; create it for the ServiceAccount %s as docs/reference/remote-executors.md shows, set KUBECONFIG, "+
				"or pass --context FILE", describeFiles(files), DefaultContext, CLIServiceAccount)
		}
		if loaded.Contexts[DefaultContext] == nil {
			return Selection{}, fmt.Errorf("the kubeconfig %s has no context named %q, which remote commands use by default; "+
				"create it for the ServiceAccount %s as docs/reference/remote-executors.md shows, or pass --context NAME or --context FILE",
				describeFiles(files), DefaultContext, CLIServiceAccount)
		}
		return finish(files, loaded, DefaultContext, namespace, fmt.Sprintf("the default context %q in %s", DefaultContext, describeFiles(files)))
	}
	if loadErr == nil && loaded.Contexts[context] != nil {
		return finish(files, loaded, context, namespace, fmt.Sprintf("--context %q, a context in %s", context, describeFiles(files)))
	}
	if info, err := os.Stat(context); err == nil && info.Mode().IsRegular() {
		return selectFile(context, namespace)
	}
	known := "it defines no contexts"
	if loadErr != nil {
		known = "it could not be read: " + loadErr.Error()
	} else if len(loaded.Contexts) > 0 {
		known = "its contexts are " + strings.Join(slices.Sorted(maps.Keys(loaded.Contexts)), ", ")
	}
	return Selection{}, fmt.Errorf("--context %q is neither a context in the kubeconfig %s (%s) nor an existing kubeconfig file; "+
		"pass a context name from that list or a path to a kubeconfig file", context, describeFiles(files), known)
}

// selectFile reads one kubeconfig file named by --context and uses its current context.
func selectFile(path, namespace string) (Selection, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Selection{}, fmt.Errorf("--context %q names a kubeconfig file, but its absolute path is unavailable: %w", path, err)
	}
	config, err := clientcmd.LoadFromFile(absolute)
	if err != nil {
		return Selection{}, fmt.Errorf("--context %q was read as a kubeconfig file, because no context has that name, "+
			"but the file is not a valid kubeconfig: %w", path, err)
	}
	if config.CurrentContext == "" {
		return Selection{}, fmt.Errorf("--context %q was read as the kubeconfig file %s, which sets no current context; "+
			`run "kubectl --kubeconfig %s config use-context NAME"`, path, absolute, absolute)
	}
	return finish([]string{absolute}, config, config.CurrentContext, namespace,
		fmt.Sprintf("--context %q, read as the kubeconfig file %s, whose current context is %q", path, absolute, config.CurrentContext))
}

// finish pins the context and namespace. how says which reading of --context
// was used, for errors about the context itself.
func finish(files []string, config *clientcmdapi.Config, name, namespace, how string) (Selection, error) {
	context := config.Contexts[name]
	if context == nil {
		return Selection{}, fmt.Errorf("%s: the context %q is not defined in that kubeconfig; "+
			`run "kubectl config get-contexts" and choose one that is`, how, name)
	}
	if namespace == "" {
		namespace = context.Namespace
		if problem := namespaceProblem(namespace); namespace != "" && problem != "" {
			return Selection{}, fmt.Errorf("%s: the context's namespace %q %s; pass --namespace", how, namespace, problem)
		}
	}
	if namespace == "" {
		namespace = DefaultNamespace
	}
	return Selection{Kubeconfig: files, Context: name, Namespace: namespace}, nil
}

// Validate checks a recorded selection without reading any file.
func (s Selection) Validate() error {
	var problems []error
	if len(s.Kubeconfig) == 0 {
		problems = append(problems, errors.New("kubeconfig: at least one absolute kubeconfig path required"))
	}
	for _, file := range s.Kubeconfig {
		if !filepath.IsAbs(file) {
			problems = append(problems, fmt.Errorf("kubeconfig: %q is not an absolute path", file))
		}
	}
	if s.Context == "" {
		problems = append(problems, errors.New("context: a kubeconfig context name required"))
	}
	if problem := namespaceProblem(s.Namespace); problem != "" {
		problems = append(problems, fmt.Errorf("namespace: %q %s", s.Namespace, problem))
	}
	return errors.Join(problems...)
}

// String names the selection for output and errors.
func (s Selection) String() string {
	return fmt.Sprintf("context %q in %s, namespace %q", s.Context, describeFiles(s.Kubeconfig), s.Namespace)
}

// restConfig loads the recorded kubeconfig files again, so a refreshed
// credential in them is used, and pins the recorded context. Exec plugins and
// auth providers run through client-go, which refreshes their credentials.
func (s Selection) restConfig() (*rest.Config, error) {
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: s.Kubeconfig}
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("cannot read the kubeconfig %s: %w", describeFiles(s.Kubeconfig), err)
	}
	if raw.Contexts[s.Context] == nil {
		return nil, fmt.Errorf("the context %q is no longer in the kubeconfig %s; it was renamed or removed. "+
			"Restore it, or select the executor again with --context", s.Context, describeFiles(s.Kubeconfig))
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, s.Context, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("the context %q in %s cannot be used: %w", s.Context, describeFiles(s.Kubeconfig), err)
	}
	return config, nil
}

// defaultFiles is the caller's kubeconfig precedence as absolute paths.
func defaultFiles() []string {
	precedence := clientcmd.NewDefaultClientConfigLoadingRules().GetLoadingPrecedence()
	files := make([]string, 0, len(precedence))
	for _, file := range precedence {
		if file == "" {
			continue
		}
		if absolute, err := filepath.Abs(file); err == nil {
			file = absolute
		}
		files = append(files, file)
	}
	return files
}

// load merges kubeconfig files as kubectl does; missing files are skipped.
func load(files []string) (*clientcmdapi.Config, error) {
	return (&clientcmd.ClientConfigLoadingRules{Precedence: files}).Load()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func describeFiles(files []string) string {
	if len(files) == 0 {
		return "(no file; set KUBECONFIG)"
	}
	return strings.Join(files, string(os.PathListSeparator))
}

func namespaceProblem(namespace string) string {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return "is not a Kubernetes namespace name; use up to 63 lowercase letters, digits and hyphens"
	}
	return ""
}

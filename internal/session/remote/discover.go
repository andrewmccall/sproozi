package remote

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// The executor's published shape in its namespace, and the CLI's identity
// there. The administrator labels the executor's Service and publishes the CA
// that issued its serving certificate; the CLI signs in as a ServiceAccount
// whose Role grants exactly what reaching the executor needs.
const (
	// ExecutorLabel marks the one Service in a namespace that serves a remote
	// executor, with the value "true". Its port named PortName carries HTTPS.
	ExecutorLabel = "sproozi.com/remote-executor"
	PortName      = "https"
	// TrustConfigMap holds, under CAKey, the CA that issued the executor's
	// serving certificate. The certificate covers SERVICE.NAMESPACE.svc.
	TrustConfigMap = "sproozi-remote-executor"
	CAKey          = "ca.crt"
	// CLIServiceAccount and CLIRole are the CLI's identity in the executor's
	// namespace and the Role bound to it (config/cli-access).
	CLIServiceAccount = "sproozi-cli"
	CLIRole           = "sproozi-cli"
	// The executor authorizes the home principal with a SubjectAccessReview for
	// this verb on this resource, in its namespace, named by its placement name.
	// No such API resource exists; RBAC rules name it all the same.
	PermissionGroup    = "sproozi.com"
	PermissionResource = "remoteexecutors"
	PermissionVerb     = "use"
)

// Executor is a remote executor home has reached: how to reach it, and what it
// stated about itself. Home records it so it can reach the executor again after
// the CLI exits. Name is the executor's own placement name, which launches carry
// and session rows show; a context is only how this developer reaches it.
type Executor struct {
	Selection
	Name               string             `json:"name"`
	Harness            harness.NativeKind `json:"harness"`
	MaxCheckpointBytes int64              `json:"maxCheckpointBytes"`
}

// Discover finds the executor that sel selects, authenticates to it and asks it
// to describe itself. Every failure names the fix.
func Discover(ctx context.Context, sel Selection) (Executor, error) {
	if err := sel.Validate(); err != nil {
		return Executor{}, err
	}
	c, err := openCluster(sel)
	if err != nil {
		return Executor{}, err
	}
	return c.discover(ctx)
}

// CheckpointProblem explains a checkpoint limit that differs from home's own,
// or returns nil. Moves go both ways, so the two limits must be equal.
func (e Executor) CheckpointProblem(local int64) error {
	if local == e.MaxCheckpointBytes {
		return nil
	}
	return fmt.Errorf("the remote executor %q accepts checkpoints up to %d bytes, but local.maxCheckpointBytes is %d; "+
		"they must be equal so a session can move both ways. Change local.maxCheckpointBytes, or ask the administrator "+
		"to change the executor's maxCheckpointBytes", e.Name, e.MaxCheckpointBytes, local)
}

// cluster is one connection attempt's view of the executor's namespace.
type cluster struct {
	sel    Selection
	kube   kubernetes.Interface
	token  string
	tunnel func(ctx context.Context, pod string, port int32) (net.Conn, error)
}

func openCluster(sel Selection) (*cluster, error) {
	config, err := sel.restConfig()
	if err != nil {
		return nil, err
	}
	token, err := bearerToken(sel, config)
	if err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be used: %w", sel, err)
	}
	return &cluster{sel: sel, kube: kube, token: token, tunnel: portForwarder(config, kube, sel.Namespace)}, nil
}

func (c *cluster) discover(ctx context.Context) (Executor, error) {
	connection, err := c.connection(ctx, 1)
	if err != nil {
		return Executor{}, err
	}
	client, err := profile.Connect(connection)
	if err != nil {
		return Executor{}, err
	}
	defer client.Close()
	d, err := client.Describe(ctx)
	if err != nil {
		return Executor{}, c.describeProblem(err)
	}
	return Executor{Selection: c.sel, Name: d.Placement, Harness: d.Harness, MaxCheckpointBytes: d.MaxCheckpointBytes}, nil
}

// connection finds the executor and its access material and returns a route
// whose every dial opens a fresh tunnel to a ready executor Pod.
func (c *cluster) connection(ctx context.Context, maxCheckpoint int64) (profile.Connection, error) {
	service, err := c.service(ctx)
	if err != nil {
		return profile.Connection{}, err
	}
	if err = c.mayForward(ctx); err != nil {
		return profile.Connection{}, err
	}
	roots, err := c.trust(ctx)
	if err != nil {
		return profile.Connection{}, err
	}
	port := servicePort(service)
	host := service.Name + "." + c.sel.Namespace + ".svc"
	return profile.Connection{
		Origin: "https://" + net.JoinHostPort(host, strconv.Itoa(int(port))), Token: c.token, Roots: roots,
		MaxCheckpointBytes: maxCheckpoint,
		Dial:               func(ctx context.Context) (net.Conn, error) { return c.dial(ctx, service.Name) },
	}, nil
}

// service finds the single Service labelled as the namespace's remote executor.
func (c *cluster) service(ctx context.Context) (*corev1.Service, error) {
	list, err := c.kube.CoreV1().Services(c.sel.Namespace).List(ctx, metav1.ListOptions{LabelSelector: ExecutorLabel + "=true"})
	if err != nil {
		return nil, c.apiProblem("list Services", err)
	}
	switch len(list.Items) {
	case 0:
		return nil, fmt.Errorf("no remote executor in namespace %q (%s): no Service there is labelled %s=true. "+
			"Pass --namespace with the executor's namespace, or ask the administrator to install the executor there "+
			"(cmd/session-profile/README.md)", c.sel.Namespace, c.sel, ExecutorLabel)
	case 1:
	default:
		names := make([]string, 0, len(list.Items))
		for _, s := range list.Items {
			names = append(names, s.Name)
		}
		slices.Sort(names)
		return nil, fmt.Errorf("namespace %q (%s) has %d Services labelled %s=true (%v); a namespace holds one "+
			"remote executor, so ask the administrator to remove the label from all but one", c.sel.Namespace, c.sel,
			len(names), ExecutorLabel, names)
	}
	service := &list.Items[0]
	if servicePort(service) == 0 {
		return nil, fmt.Errorf("the remote executor Service %s/%s has no port named %q; ask the administrator to name "+
			"its HTTPS port %q", c.sel.Namespace, service.Name, PortName, PortName)
	}
	return service, nil
}

func servicePort(s *corev1.Service) int32 {
	for _, p := range s.Spec.Ports {
		if p.Name == PortName {
			return p.Port
		}
	}
	return 0
}

// mayForward asks the API server whether this identity may open a port-forward
// in the namespace, so a denial is reported before any tunnel fails obscurely.
func (c *cluster) mayForward(ctx context.Context) error {
	review, err := c.kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: c.sel.Namespace, Verb: "create", Resource: "pods", Subresource: "portforward",
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		return c.apiProblem("check port-forward access", err)
	}
	if !review.Status.Allowed {
		return fmt.Errorf("your Kubernetes user (%s) may not port-forward to Pods in namespace %q, which reaching the "+
			"remote executor needs. Sign in as the ServiceAccount %s, whose Role %s grants it (docs/reference/remote-executors.md)",
			c.sel, c.sel.Namespace, CLIServiceAccount, CLIRole)
	}
	return nil
}

// trust reads the CA that issued the executor's serving certificate.
func (c *cluster) trust(ctx context.Context) (*x509.CertPool, error) {
	published, err := c.kube.CoreV1().ConfigMaps(c.sel.Namespace).Get(ctx, TrustConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("the remote executor in namespace %q publishes no CA: the ConfigMap %q is missing (%s); "+
			"ask the administrator to create it with the key %q (docs/reference/remote-executors.md)",
			c.sel.Namespace, TrustConfigMap, c.sel, CAKey)
	}
	if err != nil {
		return nil, c.apiProblem("read the ConfigMap "+TrustConfigMap, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(published.Data[CAKey])) {
		return nil, fmt.Errorf("the ConfigMap %s/%s has no PEM certificate under %q; ask the administrator to add the CA "+
			"that issued the executor's serving certificate", c.sel.Namespace, TrustConfigMap, CAKey)
	}
	return roots, nil
}

// dial opens a tunnel to a ready Pod behind the executor's Service. It looks up
// the endpoints on every dial, so a restarted executor Pod is found again.
func (c *cluster) dial(ctx context.Context, service string) (net.Conn, error) {
	slicesList, err := c.kube.DiscoveryV1().EndpointSlices(c.sel.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + service})
	if err != nil {
		return nil, c.apiProblem("list the executor's EndpointSlices", err)
	}
	pod, port := readyEndpoint(slicesList.Items)
	if pod == "" {
		return nil, fmt.Errorf("the remote executor Service %s/%s has no ready Pod; check it with "+
			`"kubectl --context %s -n %s get pods" and ask the administrator if it is not running`,
			c.sel.Namespace, service, c.sel.Context, c.sel.Namespace)
	}
	conn, err := c.tunnel(ctx, pod, port)
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, c.apiProblem("port-forward to the executor Pod", err)
		}
		return nil, fmt.Errorf("cannot open a port-forward to the remote executor Pod %s/%s (%s): %w",
			c.sel.Namespace, pod, c.sel, err)
	}
	return conn, nil
}

// readyEndpoint picks the first ready Pod, by name, and its HTTPS target port.
func readyEndpoint(items []discoveryv1.EndpointSlice) (string, int32) {
	type candidate struct {
		pod  string
		port int32
	}
	var ready []candidate
	for _, slice := range items {
		var port int32
		for _, p := range slice.Ports {
			if p.Name != nil && *p.Name == PortName && p.Port != nil {
				port = *p.Port
			}
		}
		if port == 0 {
			continue
		}
		for _, e := range slice.Endpoints {
			if e.TargetRef == nil || e.TargetRef.Kind != "Pod" || e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			ready = append(ready, candidate{e.TargetRef.Name, port})
		}
	}
	if len(ready) == 0 {
		return "", 0
	}
	slices.SortFunc(ready, func(a, b candidate) int {
		switch {
		case a.pod < b.pod:
			return -1
		case a.pod > b.pod:
			return 1
		}
		return 0
	})
	return ready[0].pod, ready[0].port
}

// apiProblem turns a Kubernetes API failure into guidance: RBAC denials name
// the ClusterRole to bind, and an unreachable cluster names the context.
func (c *cluster) apiProblem(action string, err error) error {
	switch {
	case apierrors.IsForbidden(err):
		return fmt.Errorf("your Kubernetes user (%s) may not %s in namespace %q: %w. If the executor is in another "+
			"namespace, pass --namespace; otherwise sign in as the ServiceAccount %s, whose Role %s grants it "+
			"(docs/reference/remote-executors.md)", c.sel, action, c.sel.Namespace, err, CLIServiceAccount, CLIRole)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("the cluster rejected the credentials of %s: %w. Sign in again, or refresh the kubeconfig, "+
			"then retry", c.sel, err)
	}
	return fmt.Errorf("cannot %s in namespace %q (%s): %w; check that the cluster is reachable "+
		`with "kubectl --context %s -n %s get services"`, action, c.sel.Namespace, c.sel, err, c.sel.Context, c.sel.Namespace)
}

// describeProblem explains a failed first request to the executor.
func (c *cluster) describeProblem(err error) error {
	var remote *profile.RemoteError
	if errors.As(err, &remote) {
		switch {
		case remote.Status == http.StatusUnauthorized:
			return fmt.Errorf("the remote executor in namespace %q could not authenticate the token of %s; the token "+
				"may have expired or been revoked. Mint a new ServiceAccount token into the context "+
				"(docs/reference/remote-executors.md)", c.sel.Namespace, c.sel)
		case remote.Status == http.StatusForbidden:
			return fmt.Errorf("the remote executor in namespace %q refused your Kubernetes user (%s): it needs %q on %s.%s "+
				"in that namespace, which the Role %s grants to the ServiceAccount %s", c.sel.Namespace, c.sel,
				PermissionVerb, PermissionResource, PermissionGroup, CLIRole, CLIServiceAccount)
		case remote.Status == http.StatusNotFound:
			return fmt.Errorf("the remote executor in namespace %q does not support describe; it is older than this "+
				"client, so ask the administrator to upgrade it", c.sel.Namespace)
		case remote.Transport != nil:
			var unknownAuthority x509.UnknownAuthorityError
			var hostname x509.HostnameError
			if errors.As(remote.Transport, &unknownAuthority) || errors.As(remote.Transport, &hostname) {
				return fmt.Errorf("the remote executor's certificate is not trusted: %w. Its serving certificate must be "+
					"issued by the %q CA in the ConfigMap %s/%s and cover SERVICE.%s.svc",
					remote.Transport, CAKey, c.sel.Namespace, TrustConfigMap, c.sel.Namespace)
			}
			return fmt.Errorf("cannot reach the remote executor in namespace %q (%s): %w", c.sel.Namespace, c.sel, remote.Transport)
		}
	}
	return fmt.Errorf("the remote executor in namespace %q (%s) did not describe itself: %w", c.sel.Namespace, c.sel, err)
}

package kubernetes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const Authority = "kubernetes.default.svc:443"

// HandlerConfig controls the trusted upstream and bounded response handling.
type HandlerConfig struct {
	Upstream         *url.URL
	Client           *http.Client
	MaxResponseBytes int64
	MaxWatchSeconds  int
	AuditLogger      audit.Writer
}

type Handler struct {
	Config HandlerConfig
	mcp    http.Handler
}

// NewHandler constructs a Kubernetes semantic capability handler.
func NewHandler(config HandlerConfig) *Handler {
	h := &Handler{Config: config}
	h.mcp = h.newMCPHandler()
	return h
}

// ServeHTTP exposes the handler to the shared gateway.
// It deliberately requires identity in context; Kubernetes never has a
// destination-specific bearer-token authentication path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	identity, ok := gateway.IdentityFromContext(req.Context())
	if !ok {
		http.Error(w, "gateway identity required", http.StatusUnauthorized)
		return
	}
	if req.URL.Path == "/mcp" && h.mcp != nil {
		h.serveMCP(w, req, identity)
		return
	}
	response, err := h.authorizeAndForward(req.Context(), identity, req)
	if err != nil {
		http.Error(w, "kubernetes request denied", http.StatusForbidden)
		return
	}
	defer func() { _ = response.Body.Close() }()
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(flushingWriter{w}, response.Body)
}

func (h *Handler) authorizeAndForward(ctx context.Context, identity *gateway.RunIdentity, req *http.Request) (response *http.Response, requestErr error) {
	if identity == nil || identity.Run == nil || identity.Policy == nil {
		return nil, fmt.Errorf("kubernetes: missing run identity")
	}
	event := identity.AuditEvent("kubernetes.read")
	event.Capability = string(sprooziv1alpha1.CapabilityKubernetesRead)
	event.SemanticLevel, event.Service = "semantic", "kubernetes"
	defer func() {
		if h.Config.AuditLogger == nil {
			return
		}
		if requestErr != nil && event.DenyReason == "" {
			// Arbitrary upstream errors can contain URLs or data.
			event.DenyReason = "kubernetes request denied or failed"
		} else if requestErr == nil && response != nil {
			event.Allowed = true
			event.UpstreamStatus = response.StatusCode
		}
		h.Config.AuditLogger.Log(event)
	}()
	op, err := ParseOperation(req)
	if err != nil {
		return nil, err
	}
	event.Operation = "kubernetes." + op.Verb
	event.Resource = req.URL.Path
	if !allowed(identity, op) {
		event.DenyReason = "operation not permitted by policy"
		return nil, fmt.Errorf("kubernetes: operation denied")
	}
	if h.Config.Upstream == nil {
		return nil, fmt.Errorf("kubernetes: upstream is not configured")
	}
	base := *h.Config.Upstream
	base.Path = strings.TrimRight(base.Path, "/") + req.URL.Path
	base.RawQuery = req.URL.RawQuery
	forward, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), req.Body)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: build request: %w", err)
	}
	for k, values := range req.Header {
		for _, value := range values {
			if !strings.EqualFold(k, "Authorization") && !strings.EqualFold(k, "Proxy-Authorization") && !strings.EqualFold(k, "Impersonate-User") && !strings.HasPrefix(strings.ToLower(k), "impersonate-extra-") {
				forward.Header.Add(k, value)
			}
		}
	}
	// Kubernetes authentication belongs exclusively to the gateway's own
	// in-cluster rest.Config transport. Never derive or forward a workload
	// credential from the authenticated run identity.
	client := h.Config.Client
	if client == nil {
		client = http.DefaultClient
	}
	callCtx := forward.Context()
	cancel := func() {}
	if op.Watch {
		max := h.Config.MaxWatchSeconds
		if max == 0 {
			max = 300
		}
		if v := req.URL.Query().Get("timeoutSeconds"); v != "" {
			n, _ := strconv.Atoi(v)
			if n < max {
				max = n
			}
		}
		callCtx, cancel = context.WithTimeout(callCtx, time.Duration(max)*time.Second)
		// The body owns cancellation after headers have been returned.
		forward = forward.WithContext(callCtx)
	}
	if err := budget.FromContext(ctx).SpendRequest(); err != nil {
		cancel()
		return nil, err
	}
	resp, err := client.Do(forward)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &responseBody{ReadCloser: resp.Body, cancel: cancel}
	if h.Config.MaxResponseBytes > 0 && resp.Body != nil {
		resp.Body = &boundedBody{ReadCloser: resp.Body, remaining: h.Config.MaxResponseBytes}
	}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, fmt.Errorf("kubernetes: response exceeds limit")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func allowed(id *gateway.RunIdentity, op Operation) bool {
	if !slices.Contains(id.Run.Spec.Capabilities, sprooziv1alpha1.CapabilityKubernetesRead) || !slices.Contains(id.Policy.Spec.AllowedCapabilities, sprooziv1alpha1.CapabilityKubernetesRead) {
		return false
	}
	if op.Discovery {
		return (op.APIGroup == "" || op.APIGroup == appsAPIGroup) && (op.Version == "" || op.Version == "v1")
	}
	if op.APIGroup != "" && op.APIGroup != appsAPIGroup || op.Version == "" {
		return false
	}
	if op.Namespace == "" || !containsString(id.Policy.Spec.KubernetesRead.Namespaces, op.Namespace) {
		return false
	}
	resource := sprooziv1alpha1.KubernetesReadResource(op.Resource)
	if op.Subresource != "" {
		resource = sprooziv1alpha1.KubernetesReadResource(op.Resource + "/" + op.Subresource)
	}
	return slices.Contains(id.Policy.Spec.KubernetesRead.Resources, resource)
}
func containsString(xs []string, x string) bool {
	return slices.Contains(xs, x)
}

// Closing or exhausting a response releases its watch deadline.
type responseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *responseBody) Close() error { b.cancel(); return b.ReadCloser.Close() }
func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cancel()
	}
	return n, err
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

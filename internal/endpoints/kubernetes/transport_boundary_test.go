package kubernetes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestKubernetesHandlerRefusesRedirectsAndImpersonation(t *testing.T) {
	upstream, _ := url.Parse("https://kubernetes.test")
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		for key := range req.Header {
			if strings.HasPrefix(strings.ToLower(key), "impersonate-") {
				t.Errorf("forwarded header %s", key)
			}
		}
		if req.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", req.Header.Get("Accept"))
		}
		return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://redirect.test/next"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: req}, nil
	})}
	handler := NewHandler(HandlerConfig{Upstream: upstream, Client: client})
	request := httptest.NewRequest(http.MethodGet, demoPodsPath, nil)
	for _, key := range []string{"Impersonate-User", "Impersonate-Group", "Impersonate-Uid", "Impersonate-Extra-Tenant"} {
		request.Header.Set(key, "untrusted")
	}
	request.Header.Set("Accept", "application/json")
	got, err := handler.authorizeAndForward(context.Background(), identity(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Body.Close() }()
	if got.StatusCode != http.StatusTemporaryRedirect || calls != 1 {
		t.Fatalf("status=%d calls=%d", got.StatusCode, calls)
	}
	if client.CheckRedirect != nil {
		t.Fatal("handler mutated caller-owned client")
	}
}

func TestKubernetesScopeBindsGroupVersionAndResource(t *testing.T) {
	upstream, _ := url.Parse("https://kubernetes.test")
	calls := 0
	handler := NewHandler(HandlerConfig{Upstream: upstream, Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { calls++; return response(), nil })}})
	grant := identity()
	grant.Policy.Spec.KubernetesRead.Resources = append(grant.Policy.Spec.KubernetesRead.Resources, sprooziv1alpha1.KubernetesReadDeployments)
	for _, tc := range []struct {
		path    string
		allowed bool
	}{
		{"/api/v1/namespaces/demo/pods", true},
		{"/api/v1/namespaces/demo/pods/p1/log", true},
		{"/apis/apps/v1/namespaces/demo/deployments", true},
		{"/apis/apps/v1/namespaces/demo/pods", false},
		{"/api/v1/namespaces/demo/deployments", false},
		{"/api/v2/namespaces/demo/pods", false},
		{"/apis/apps/v1beta1/namespaces/demo/deployments", false},
	} {
		got, err := handler.authorizeAndForward(context.Background(), grant, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if (err == nil) != tc.allowed {
			t.Errorf("%s: response=%v error=%v", tc.path, got, err)
		}
		if got != nil {
			_ = got.Body.Close()
		}
	}
	if calls != 3 {
		t.Fatalf("upstream calls=%d want 3", calls)
	}
}

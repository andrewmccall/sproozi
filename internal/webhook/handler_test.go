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

package webhook_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/webhook"
)

const testAlertBody = `{"id":"evt-123","eventContext":{"alertname":"CrashLoopBackOff"}}`

const (
	testTemplateName = "sre-remediation"
)

const (
	webhookTestNamespace = "default"
	webhookReceiver      = "alertmanager"
)

type captureWebhookLogger struct {
	events []audit.WebhookEvent
}

func (c *captureWebhookLogger) LogWebhook(e audit.WebhookEvent) {
	c.events = append(c.events, e)
}

func webhookScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sprooziv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme sproozi: %v", err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme core: %v", err)
	}
	return scheme
}

func webhookPolicy() *sprooziv1alpha1.AgentPolicy {
	return &sprooziv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sre",
			Namespace: webhookTestNamespace,
		},
		Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{
				sprooziv1alpha1.CapabilityKubernetesRead,
				sprooziv1alpha1.CapabilityGitHubPullRequest,
			},
		},
	}
}

func webhookTemplate() *sprooziv1alpha1.AgentTemplate {
	return &sprooziv1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testTemplateName,
			Namespace: webhookTestNamespace,
			Labels: map[string]string{
				webhook.ReceiverLabel: webhookReceiver,
			},
			Annotations: map[string]string{
				webhook.SecretNameAnnotation: "alertmanager-secret",
			},
		},
		Spec: sprooziv1alpha1.AgentTemplateSpec{
			RuntimeRef:     sprooziv1alpha1.AgentRuntimeReference{Name: "codex"},
			PolicyRef:      sprooziv1alpha1.AgentPolicyReference{Name: "sre"},
			Instructions:   "Investigate and propose a pull request.",
			EgressProfiles: []string{"registry"},
		},
	}
}

func webhookSecret(key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alertmanager-secret",
			Namespace: webhookTestNamespace,
		},
		Data: map[string][]byte{
			"hmac-key": key,
		},
	}
}

func webhookClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(webhookScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()
}

func listRuns(t *testing.T, c client.Client) []sprooziv1alpha1.AgentRun {
	t.Helper()
	var runs sprooziv1alpha1.AgentRunList
	if err := c.List(t.Context(), &runs); err != nil {
		t.Fatalf("List AgentRuns: %v", err)
	}
	return runs.Items
}

func TestHandler_CreatesOneRunWithTemplateFixedFields(t *testing.T) {
	key := []byte("webhook-secret")
	body := `{"id":"evt-123","eventContext":{"alertname":"CrashLoopBackOff","namespace":"sproozi-demo"},"capabilities":["network.egress"],"templateRef":{"name":"evil"},"policyRef":{"name":"evil"},"runtimeRef":{"name":"evil"},"maxExecutionDuration":"24h"}`
	ts := fmt.Sprintf("%d", time.Now().Unix())

	c := webhookClient(t,
		webhookPolicy(),
		webhookTemplate(),
		webhookSecret(key),
	)
	logger := &captureWebhookLogger{}
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: logger,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), key))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rr.Code, rr.Body.String())
	}

	runs := listRuns(t, c)
	if len(runs) != 1 {
		t.Fatalf("expected exactly one AgentRun, got %d", len(runs))
	}

	run := runs[0]
	if run.Spec.TemplateRef.Name != testTemplateName {
		t.Fatalf("expected templateRef.name to stay fixed, got %q", run.Spec.TemplateRef.Name)
	}
	if run.Spec.Task != webhook.RunTask {
		t.Fatalf("expected fixed webhook task %q, got %q", webhook.RunTask, run.Spec.Task)
	}
	if got, want := run.Spec.Capabilities, webhookPolicy().Spec.AllowedCapabilities; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("expected policy capabilities %v, got %v", want, got)
	}
	if len(run.Spec.EventContext) != 2 || run.Spec.EventContext["alertname"] != "CrashLoopBackOff" || run.Spec.EventContext["namespace"] != "sproozi-demo" {
		t.Fatalf("unexpected eventContext: %#v", run.Spec.EventContext)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response JSON: %v", err)
	}
	if resp["runName"] == "" || resp["template"] != testTemplateName || resp["source"] != webhookReceiver {
		t.Fatalf("unexpected response body: %#v", resp)
	}

	if len(logger.events) != 1 {
		t.Fatalf("expected one audit event, got %d", len(logger.events))
	}
	if !logger.events[0].Allowed || logger.events[0].TemplateName != testTemplateName || logger.events[0].Source != webhookReceiver {
		t.Fatalf("unexpected audit event: %#v", logger.events[0])
	}
}

func TestHandler_ReusedEventIDIsRejected(t *testing.T) {
	key := []byte("webhook-secret")
	body := `{"id":"evt-duplicate","eventContext":{"alertname":"CrashLoopBackOff"}}`

	c := webhookClient(t,
		webhookPolicy(),
		webhookTemplate(),
		webhookSecret(key),
	)
	logger := &captureWebhookLogger{}
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: logger,
		},
	}

	firstTS := fmt.Sprintf("%d", time.Now().Unix())
	firstReq := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	firstReq.Header.Set(webhook.TimestampHeader, firstTS)
	firstReq.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(firstTS, []byte(body), key))
	firstResp := httptest.NewRecorder()
	handler.ServeHTTP(firstResp, firstReq)
	if firstResp.Code != http.StatusCreated {
		t.Fatalf("expected first request to create a run, got %d body=%s", firstResp.Code, firstResp.Body.String())
	}

	secondTS := fmt.Sprintf("%d", time.Now().Add(2*time.Second).Unix())
	secondReq := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	secondReq.Header.Set(webhook.TimestampHeader, secondTS)
	secondReq.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(secondTS, []byte(body), key))
	secondResp := httptest.NewRecorder()
	handler.ServeHTTP(secondResp, secondReq)

	if secondResp.Code != http.StatusConflict {
		t.Fatalf("expected second request with same event ID to be rejected, got %d body=%s", secondResp.Code, secondResp.Body.String())
	}

	runs := listRuns(t, c)
	if len(runs) != 1 {
		t.Fatalf("expected exactly one AgentRun after replay, got %d", len(runs))
	}
}

func TestHandler_MissingEventIDRejected(t *testing.T) {
	key := []byte("webhook-secret")
	body := `{"eventContext":{"alertname":"CrashLoopBackOff"}}`
	ts := fmt.Sprintf("%d", time.Now().Unix())

	c := webhookClient(t,
		webhookPolicy(),
		webhookTemplate(),
		webhookSecret(key),
	)
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: &captureWebhookLogger{},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), key))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing event ID, got %d body=%s", resp.Code, resp.Body.String())
	}
	if runs := listRuns(t, c); len(runs) != 0 {
		t.Fatalf("expected no AgentRuns for missing event ID, got %d", len(runs))
	}
}

func TestHandler_MissingSignatureRejected(t *testing.T) {
	key := []byte("webhook-secret")
	body := testAlertBody
	ts := fmt.Sprintf("%d", time.Now().Unix())

	c := webhookClient(t,
		webhookPolicy(),
		webhookTemplate(),
		webhookSecret(key),
	)
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: &captureWebhookLogger{},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing signature, got %d body=%s", resp.Code, resp.Body.String())
	}
	if runs := listRuns(t, c); len(runs) != 0 {
		t.Fatalf("expected no AgentRuns for unsigned request, got %d", len(runs))
	}
}

func TestHandler_StaleSignatureRejected(t *testing.T) {
	key := []byte("webhook-secret")
	body := testAlertBody
	ts := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())

	c := webhookClient(t,
		webhookPolicy(),
		webhookTemplate(),
		webhookSecret(key),
	)
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:       c,
			Replay:       webhook.NewInMemoryReplayStore(),
			AuditLogger:  &captureWebhookLogger{},
			ReplayWindow: webhook.DefaultReplayWindow,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), key))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stale signature, got %d body=%s", resp.Code, resp.Body.String())
	}
	if runs := listRuns(t, c); len(runs) != 0 {
		t.Fatalf("expected no AgentRuns for stale request, got %d", len(runs))
	}
}

func TestHandler_MalformedJSONRejected(t *testing.T) {
	key := []byte("webhook-secret")
	body := `{"id":"evt-123","eventContext":`
	ts := fmt.Sprintf("%d", time.Now().Unix())

	c := webhookClient(t,
		webhookPolicy(),
		webhookTemplate(),
		webhookSecret(key),
	)
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: &captureWebhookLogger{},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), key))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d body=%s", resp.Code, resp.Body.String())
	}
	if runs := listRuns(t, c); len(runs) != 0 {
		t.Fatalf("expected no AgentRuns for malformed JSON, got %d", len(runs))
	}
}

func TestHandler_AmbiguousReceiverRejected(t *testing.T) {
	key := []byte("webhook-secret")
	body := testAlertBody
	ts := fmt.Sprintf("%d", time.Now().Unix())

	templateA := webhookTemplate()
	templateB := webhookTemplate()
	templateB.Name = "sre-remediation-b"

	c := webhookClient(t,
		webhookPolicy(),
		templateA,
		templateB,
		webhookSecret(key),
	)
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: &captureWebhookLogger{},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), key))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusConflict {
		t.Fatalf("expected 409 for ambiguous receiver, got %d body=%s", resp.Code, resp.Body.String())
	}
	if runs := listRuns(t, c); len(runs) != 0 {
		t.Fatalf("expected no AgentRuns for ambiguous receiver, got %d", len(runs))
	}
}

// A sender must retry after a transient lookup failure, so it must not be
// told that the receiver does not exist.
func TestHandler_ReceiverLookupStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listErr error
		want    int
	}{
		{name: "missing receiver", want: http.StatusNotFound},
		{name: "transient list failure", listErr: errors.New("etcdserver: request timed out"), want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if tc.listErr != nil {
						return tc.listErr
					}
					return c.List(ctx, list, opts...)
				},
			}).Build()
			handler := &webhook.Handler{Config: webhook.HandlerConfig{
				Client:      c,
				Replay:      webhook.NewInMemoryReplayStore(),
				AuditLogger: &captureWebhookLogger{},
			}}

			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, signedRequest(testAlertBody, []byte("webhook-secret")))

			if resp.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", resp.Code, tc.want, resp.Body.String())
			}
		})
	}
}

// signedRequest returns a receiver request with a current, well-formed
// signature, so it passes the checks that need no Secret.
func signedRequest(body string, key []byte) *http.Request {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), key))
	return req
}

// countingReader records whether the handler read the request body.
type countingReader struct {
	reads int
	body  *strings.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.body.Read(p)
}

// TestHandler_UnauthenticatedRequestCostsNoBodyOrAPICall verifies that a
// request with missing, malformed or stale signature headers is refused
// before the handler reads its body or calls the API server.
func TestHandler_UnauthenticatedRequestCostsNoBodyOrAPICall(t *testing.T) {
	now := time.Now()
	current := fmt.Sprintf("%d", now.Unix())
	valid := webhook.ComputeSignature(current, []byte(testAlertBody), []byte("webhook-secret"))
	for _, tc := range []struct {
		name, signature, timestamp string
	}{
		{name: "missing signature", timestamp: current},
		{name: "malformed signature", signature: "sha256=zz", timestamp: current},
		{name: "short signature", signature: "sha256=abcd", timestamp: current},
		{name: "wrong scheme", signature: "sha1=" + strings.TrimPrefix(valid, "sha256="), timestamp: current},
		{name: "missing timestamp", signature: valid},
		{name: "malformed timestamp", signature: valid, timestamp: "yesterday"},
		{name: "stale timestamp", signature: valid, timestamp: fmt.Sprintf("%d", now.Add(-time.Hour).Unix())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var apiCalls int
			c := fake.NewClientBuilder().WithScheme(webhookScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					apiCalls++
					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					apiCalls++
					return c.List(ctx, list, opts...)
				},
			}).WithObjects(webhookPolicy(), webhookTemplate(), webhookSecret([]byte("webhook-secret"))).Build()
			handler := &webhook.Handler{Config: webhook.HandlerConfig{
				Client:      c,
				Replay:      webhook.NewInMemoryReplayStore(),
				AuditLogger: &captureWebhookLogger{},
				Now:         func() time.Time { return now },
			}}
			body := &countingReader{body: strings.NewReader(testAlertBody)}
			req := httptest.NewRequest(http.MethodPost, "/webhook/default/"+webhookReceiver, body)
			if tc.signature != "" {
				req.Header.Set(webhook.SignatureHeader, tc.signature)
			}
			if tc.timestamp != "" {
				req.Header.Set(webhook.TimestampHeader, tc.timestamp)
			}
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusUnauthorized || body.reads != 0 || apiCalls != 0 {
				t.Fatalf("status = %d, body reads = %d, API calls = %d; want 401 with no reads or calls",
					resp.Code, body.reads, apiCalls)
			}
		})
	}
}

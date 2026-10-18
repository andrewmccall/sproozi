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

// Package webhook is the signed inbound event receiver. It verifies an
// HMAC-signed HTTP POST and creates one AgentRun from the receiving template.
// It is not a Kubernetes admission webhook.
package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
)

const (
	ReceiverLabel        = "sproozi.com/webhook-receiver"
	SecretNameAnnotation = "sproozi.com/webhook-secret"
	maxBodyBytes         = 512 * 1024
)

var (
	errTemplateNotFound  = errors.New("webhook: receiving template not found")
	errTemplateAmbiguous = errors.New("webhook: receiving template is ambiguous")
	errSecretNameMissing = errors.New("webhook: receiving template missing webhook secret annotation")
	errEventIDMissing    = errors.New("webhook: event id is required")
)

type HandlerConfig struct {
	Client       client.Client
	Replay       ReplayStore
	AuditLogger  audit.WebhookWriter
	ReplayWindow time.Duration
	Now          func() time.Time
	// SecretNamespace holds every receiver's HMAC Secret. It is separate from
	// the template namespace, so the webhook can read HMAC keys without being
	// able to read the provider and GitHub credentials beside the templates.
	SecretNamespace string
}

// DefaultSecretNamespace is the installed namespace for HMAC Secrets.
const DefaultSecretNamespace = "sproozi-webhooks"

type Handler struct {
	Config HandlerConfig
}

type payload struct {
	ID           string            `json:"id"`
	EventContext map[string]string `json:"eventContext"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace, receiver, ok := parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	now := time.Now
	if h.Config.Now != nil {
		now = h.Config.Now
	}

	event := audit.WebhookEvent{
		Timestamp:         now().UTC(),
		TemplateNamespace: namespace,
		Operation:         "webhook.ingest",
		Source:            receiver,
	}

	if r.Method != http.MethodPost {
		h.deny(w, event, http.StatusMethodNotAllowed, "only POST is accepted")
		return
	}

	window := h.Config.ReplayWindow
	if window == 0 {
		window = DefaultReplayWindow
	}
	// Refuse an unsigned, malformed or stale request before reading its body
	// or calling the API server. Only a request that could be authentic costs
	// a body read, a template and policy lookup and a Secret read.
	signed, err := ParseSignedHeaders(r.Header.Get(SignatureHeader), r.Header.Get(TimestampHeader), now(), window)
	if err != nil {
		h.deny(w, event, http.StatusUnauthorized, signatureDenyReason(err))
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer func() { _ = body.Close() }()

	rawBody, err := io.ReadAll(body)
	if err != nil {
		h.deny(w, event, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return
	}

	tmpl, policy, err := h.resolveTemplate(r.Context(), namespace, receiver)
	if err != nil {
		// Anything other than a definite lookup result is transient; the sender
		// should retry rather than treat the receiver as missing.
		status, reason := http.StatusServiceUnavailable, "webhook: receiving template lookup failed"
		switch {
		case errors.Is(err, errTemplateNotFound), apierrors.IsNotFound(err):
			status, reason = http.StatusNotFound, err.Error()
		case errors.Is(err, errTemplateAmbiguous):
			status, reason = http.StatusConflict, err.Error()
		case errors.Is(err, errSecretNameMissing):
			status, reason = http.StatusInternalServerError, err.Error()
		}
		h.deny(w, event, status, reason)
		return
	}
	event.TemplateName = tmpl.Name

	secretNamespace := h.Config.SecretNamespace
	if secretNamespace == "" {
		secretNamespace = DefaultSecretNamespace
	}
	secretName := tmpl.Annotations[SecretNameAnnotation]
	var secret corev1.Secret
	if err := h.Config.Client.Get(r.Context(), client.ObjectKey{Namespace: secretNamespace, Name: secretName}, &secret); err != nil {
		h.deny(w, event, http.StatusInternalServerError, "webhook: failed to load HMAC secret")
		return
	}
	key, ok := secret.Data[hmacKeyField]
	if !ok {
		h.deny(w, event, http.StatusInternalServerError, ErrNoHMACKey.Error())
		return
	}
	// An empty or short key would let anyone who can guess it create runs.
	// Name the Secret for the administrator, never the key or its length.
	if len(key) < MinHMACKeyBytes {
		log.FromContext(r.Context()).Error(ErrShortHMACKey, "Refused webhook HMAC Secret",
			"namespace", secretNamespace, "name", secretName)
		h.deny(w, event, http.StatusInternalServerError, ErrShortHMACKey.Error())
		return
	}

	if err := signed.Verify(rawBody, key); err != nil {
		h.deny(w, event, http.StatusUnauthorized, signatureDenyReason(err))
		return
	}
	eventTime := signed.EventTime

	var req payload
	if err := json.Unmarshal(rawBody, &req); err != nil {
		h.deny(w, event, http.StatusBadRequest, "webhook: malformed JSON payload")
		return
	}
	if req.ID == "" {
		h.deny(w, event, http.StatusBadRequest, errEventIDMissing.Error())
		return
	}

	if h.Config.Replay == nil {
		h.deny(w, event, http.StatusServiceUnavailable, "webhook: replay store unavailable")
		return
	}
	if err := h.Config.Replay.CheckAndRecord(r.Context(), replayKey(namespace, receiver, req.ID), ReplayExpiry(eventTime, window)); err != nil {
		h.deny(w, event, http.StatusConflict, err.Error())
		return
	}

	run, err := (&AgentRunCreator{
		Client: h.Config.Client,
		Now:    now,
	}).Create(r.Context(), tmpl, policy, req.EventContext)
	if err != nil {
		h.deny(w, event, http.StatusInternalServerError, err.Error())
		return
	}

	event.Allowed = true
	event.RunName = run.Name
	h.Config.AuditLogger.LogWebhook(event)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"namespace": namespace,
		"runName":   run.Name,
		"source":    receiver,
		"template":  tmpl.Name,
	})
}

func (h *Handler) resolveTemplate(ctx context.Context, namespace, receiver string) (*sprooziv1alpha1.AgentTemplate, *sprooziv1alpha1.AgentPolicy, error) {
	var templates sprooziv1alpha1.AgentTemplateList
	if err := h.Config.Client.List(ctx, &templates, client.InNamespace(namespace), client.MatchingLabels{ReceiverLabel: receiver}); err != nil {
		return nil, nil, err
	}
	if len(templates.Items) == 0 {
		return nil, nil, errTemplateNotFound
	}
	if len(templates.Items) != 1 {
		return nil, nil, errTemplateAmbiguous
	}

	tmpl := &templates.Items[0]
	if tmpl.Annotations[SecretNameAnnotation] == "" {
		return nil, nil, errSecretNameMissing
	}

	var policy sprooziv1alpha1.AgentPolicy
	if err := h.Config.Client.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      tmpl.Spec.PolicyRef.Name,
	}, &policy); err != nil {
		return nil, nil, err
	}
	return tmpl, &policy, nil
}

func parsePath(path string) (string, string, bool) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[0] != "webhook" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func replayKey(namespace, receiver, eventID string) string {
	sum := sha256.Sum256([]byte(namespace + "\n" + receiver + "\n" + eventID))
	return hex.EncodeToString(sum[:])
}

// ReplayExpiry returns when an accepted event's replay record may be removed.
// A replica accepts the event until eventTime+window by its own clock, while
// the record's expiry is judged by whichever replica reads or sweeps it. The
// extra window keeps the record until every replica whose clock is within one
// window of the others has stopped accepting the event.
func ReplayExpiry(eventTime time.Time, window time.Duration) time.Time {
	return eventTime.Add(2 * window)
}

// signatureDenyReason names a stale timestamp, which a sender can correct, and
// otherwise does not say which check failed.
func signatureDenyReason(err error) string {
	if errors.Is(err, ErrStaleTimestamp) {
		return ErrStaleTimestamp.Error()
	}
	return "signature verification failed"
}

func (h *Handler) deny(w http.ResponseWriter, event audit.WebhookEvent, status int, reason string) {
	event.Allowed = false
	event.DenyReason = reason
	h.Config.AuditLogger.LogWebhook(event)
	http.Error(w, reason, status)
}

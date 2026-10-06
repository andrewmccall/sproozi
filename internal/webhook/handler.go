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
	"sigs.k8s.io/controller-runtime/pkg/client"

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
}

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

	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer func() { _ = body.Close() }()

	rawBody, err := io.ReadAll(body)
	if err != nil {
		h.deny(w, event, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return
	}

	tmpl, policy, err := h.resolveTemplate(r.Context(), namespace, receiver)
	if err != nil {
		status := http.StatusNotFound
		switch err {
		case errTemplateAmbiguous:
			status = http.StatusConflict
		case errSecretNameMissing:
			status = http.StatusInternalServerError
		}
		h.deny(w, event, status, err.Error())
		return
	}
	event.TemplateName = tmpl.Name

	secretName := tmpl.Annotations[SecretNameAnnotation]
	var secret corev1.Secret
	if err := h.Config.Client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: secretName}, &secret); err != nil {
		h.deny(w, event, http.StatusInternalServerError, "webhook: failed to load HMAC secret")
		return
	}
	key, ok := secret.Data[hmacKeyField]
	if !ok {
		h.deny(w, event, http.StatusInternalServerError, ErrNoHMACKey.Error())
		return
	}

	window := h.Config.ReplayWindow
	if window == 0 {
		window = DefaultReplayWindow
	}
	eventTime, err := VerifySignature(
		r.Header.Get(SignatureHeader),
		r.Header.Get(TimestampHeader),
		rawBody,
		key,
		now(),
		window,
	)
	if err != nil {
		reason := "signature verification failed"
		if errors.Is(err, ErrStaleTimestamp) {
			reason = ErrStaleTimestamp.Error()
		}
		h.deny(w, event, http.StatusUnauthorized, reason)
		return
	}

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
	if err := h.Config.Replay.CheckAndRecord(r.Context(), replayKey(namespace, receiver, req.ID), eventTime.Add(window)); err != nil {
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

func (h *Handler) deny(w http.ResponseWriter, event audit.WebhookEvent, status int, reason string) {
	event.Allowed = false
	event.DenyReason = reason
	h.Config.AuditLogger.LogWebhook(event)
	http.Error(w, reason, status)
}

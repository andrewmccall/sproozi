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

// Package githubgateway is a minimal git smart HTTP proxy that enforces
// AgentPolicy repository and branch constraints, injects GitHub App credentials,
// and logs every allow/deny decision. No actions are performed on the agent's
// behalf — the agent uses standard git operations and the GitHub REST/GraphQL
// APIs through this proxy to do its own work.
package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	uploadPackEndpoint = "git-upload-pack"
)

const (
	receivePackEndpoint = "git-receive-pack"
)

// HandlerConfig holds all dependencies for Handler.
type HandlerConfig struct {
	// TokenProvider provides GitHub App installation tokens.
	TokenProvider TokenProvider

	// AuditLogger writes structured audit events for every decision.
	AuditLogger audit.Writer

	// GitBaseURL is the upstream for git smart HTTP (e.g. "https://github.com").
	GitBaseURL string

	// APIBaseURL is the upstream for GitHub REST API (e.g. "https://api.github.com").
	APIBaseURL string

	// HTTPClient is the HTTP client used to forward requests upstream.
	HTTPClient *http.Client
}

// Handler is the HTTP handler for the capability proxy.
type Handler struct {
	// Config holds all dependencies and configuration.
	Config HandlerConfig
}

// ServeHTTP dispatches the request to a git or REST API handler after
// authenticating the run and checking policy constraints.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	e := audit.Event{Timestamp: time.Now().UTC(), Operation: r.Method + " " + r.URL.Path}

	identity, ok := gateway.IdentityFromContext(ctx)
	if !ok {
		h.deny(w, e, http.StatusUnauthorized, "gateway identity required")
		return
	}
	e = identity.AuditEvent(r.Method + " " + r.URL.Path)
	e.Service, e.SemanticLevel = "github", "semantic"
	e.Capability = string(sprooziv1alpha1.CapabilityGitHubPullRequest)
	e.RunID = identity.Run.Namespace + "/" + identity.Run.Name
	e.RunUID = string(identity.Run.UID)
	e.PolicyName = identity.Policy.Name
	e.PolicyGeneration = identity.Policy.Generation

	// 3. Require github.pull_request in both run and policy.
	hasCapability := slices.Contains(identity.Run.Spec.Capabilities, sprooziv1alpha1.CapabilityGitHubPullRequest) &&
		slices.Contains(identity.Policy.Spec.AllowedCapabilities, sprooziv1alpha1.CapabilityGitHubPullRequest)
	if !hasCapability {
		h.deny(w, e, http.StatusForbidden, "github.pull_request capability not granted")
		return
	}

	// 3. Route to the appropriate operation handler.
	switch {
	case isGitPath(r.URL.Path):
		h.handleGit(w, r, e, identity)
	case isPRCreate(r):
		h.handlePRCreate(w, r, e, identity)
	case isGraphQL(r):
		h.handleGraphQL(w, r, e, identity)
	default:
		h.deny(w, e, http.StatusForbidden, "operation not permitted")
	}
}

// isGitPath reports whether path is a git smart HTTP endpoint.
func isGitPath(path string) bool {
	return gitEndpoint(path) != ""
}

// isPRCreate reports whether the request is a GitHub REST API PR creation call.
// Only POST /repos/{owner}/{repo}/pulls is permitted.
func isPRCreate(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	return len(parts) == 4 && parts[0] == "repos" && parts[3] == "pulls" && validGitHubRepoPart(parts[1]) && validGitHubRepoPart(parts[2])
}

// parseGitRepo extracts "owner/repo" from a git smart HTTP path such as
// /{owner}/{repo}.git/info/refs or /{owner}/{repo}.git/git-receive-pack.
func parseGitRepo(path string) (string, bool) {
	path = strings.TrimPrefix(path, "/")
	before, _, ok := strings.Cut(path, ".git/")
	if !ok {
		return "", false
	}
	parts := strings.Split(before, "/")
	if len(parts) != 2 || !validGitHubRepoPart(parts[0]) || !validGitHubRepoPart(parts[1]) {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

// gitEndpoint accepts only the smart-HTTP paths used by ordinary git. A
// repository-looking prefix is not sufficient: forwarding arbitrary paths
// under an allowed repository would turn this boundary into a generic
// GitHub-credential proxy.
func gitEndpoint(path string) string {
	const suffix = ".git/"
	_, after, ok := strings.Cut(path, suffix)
	if !ok {
		return ""
	}
	switch endpoint := after; endpoint {
	case "info/refs", uploadPackEndpoint, receivePackEndpoint:
		return endpoint
	default:
		return ""
	}
}

func validGitHubRepoPart(value string) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// parseAPIRepo extracts "owner/repo" from a REST API path such as
// /repos/{owner}/{repo}/pulls.
func parseAPIRepo(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || !validGitHubRepoPart(parts[1]) || !validGitHubRepoPart(parts[2]) {
		return "", false
	}
	return parts[1] + "/" + parts[2], true
}

// handleGit validates and proxies git smart HTTP requests.
func (h *Handler) handleGit(w http.ResponseWriter, r *http.Request, e audit.Event, identity *gateway.RunIdentity) {
	ctx := r.Context()

	repo, ok := parseGitRepo(r.URL.Path)
	if !ok {
		h.deny(w, e, http.StatusBadRequest, "invalid git URL")
		return
	}

	e.Resource = repo
	if !slices.Contains(identity.Policy.Spec.GitHubPullRequest.Repositories, repo) {
		h.deny(w, e, http.StatusForbidden, "repository not permitted by policy")
		return
	}
	endpoint := gitEndpoint(r.URL.Path)
	switch endpoint {
	case "info/refs":
		query := r.URL.Query()
		service := query.Get("service")
		if r.Method != http.MethodGet || len(query) != 1 || (service != uploadPackEndpoint && service != receivePackEndpoint) {
			h.deny(w, e, http.StatusForbidden, "git discovery request is not permitted")
			return
		}
	case uploadPackEndpoint, receivePackEndpoint:
		if r.Method != http.MethodPost || r.URL.RawQuery != "" {
			h.deny(w, e, http.StatusForbidden, "git service request is not permitted")
			return
		}
	}

	// For push operations, parse the complete bounded command section and allow
	// exactly one creation in this immutable run's branch namespace.
	if endpoint == receivePackEndpoint {
		inspectedBody, err := inspectReceivePack(r.Body, string(identity.Run.UID))
		r.Body = inspectedBody
		if err != nil {
			h.deny(w, e, http.StatusForbidden, "push not permitted: "+err.Error())
			return
		}
	}

	ghToken, err := h.Config.TokenProvider.Token(ctx)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to obtain GitHub credentials")
		return
	}

	upstream := h.Config.GitBaseURL + r.URL.RequestURI()
	h.forward(w, r, e, upstream, "basic", ghToken)
}

// handlePRCreate validates and proxies a POST /repos/{owner}/{repo}/pulls request.
func (h *Handler) handlePRCreate(w http.ResponseWriter, r *http.Request, e audit.Event, identity *gateway.RunIdentity) {
	ctx := r.Context()

	repo, ok := parseAPIRepo(r.URL.Path)
	if !ok {
		h.deny(w, e, http.StatusBadRequest, "invalid repository path")
		return
	}

	e.Resource = repo
	if !slices.Contains(identity.Policy.Spec.GitHubPullRequest.Repositories, repo) {
		h.deny(w, e, http.StatusForbidden, "repository not permitted by policy")
		return
	}
	// Validate requested head/base before contacting GitHub. A denied request
	// must not create an out-of-scope PR and only be rejected after read-back.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		h.deny(w, e, http.StatusBadRequest, "pull request body is invalid")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var requested struct {
		Head string `json:"head"`
		Base string `json:"base"`
	}
	if json.Unmarshal(body, &requested) != nil || !runOwnedBranch(requested.Head, string(identity.Run.UID)) ||
		!policyBranch(requested.Base, identity.Policy.Spec.GitHubPullRequest.AllowedBaseBranches) {
		h.deny(w, e, http.StatusForbidden, "pull request head or base is not permitted by policy")
		return
	}

	ghToken, err := h.Config.TokenProvider.Token(ctx)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to obtain GitHub credentials")
		return
	}

	upstream, err := url.JoinPath(h.Config.APIBaseURL, r.URL.Path)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build upstream URL")
		return
	}
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	h.forwardPRCreate(w, r, e, upstream, ghToken, identity, repo)
}

type upstreamPullRequest struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func (h *Handler) forwardPRCreate(w http.ResponseWriter, r *http.Request, e audit.Event, upstreamURL, token string, identity *gateway.RunIdentity, repo string) {
	response, err := h.doUpstream(r, upstreamURL, token)
	if err != nil {
		h.deny(w, e, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		h.deny(w, e, http.StatusBadGateway, "could not read upstream response")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		h.copyResponse(w, response, body)
		h.auditAllowed(e, response.StatusCode)
		return
	}
	var created upstreamPullRequest
	if err := json.Unmarshal(body, &created); err != nil || created.Number <= 0 {
		h.deny(w, e, http.StatusBadGateway, "upstream pull request response was invalid")
		return
	}
	readbackURL, err := url.JoinPath(h.Config.APIBaseURL, "repos", strings.Split(repo, "/")[0], strings.Split(repo, "/")[1], "pulls", fmt.Sprint(created.Number))
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build pull request read-back URL")
		return
	}
	readbackReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, readbackURL, nil)
	readback, err := h.doUpstream(readbackReq, readbackURL, token)
	if err != nil || readback.StatusCode != http.StatusOK {
		if readback != nil {
			_ = readback.Body.Close()
		}
		h.deny(w, e, http.StatusBadGateway, "pull request read-back failed")
		return
	}
	readbackBody, err := io.ReadAll(io.LimitReader(readback.Body, 1<<20))
	_ = readback.Body.Close()
	var verified upstreamPullRequest
	expectedURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo, created.Number)
	if err != nil || json.Unmarshal(readbackBody, &verified) != nil || verified.Number != created.Number || verified.HTMLURL != expectedURL || verified.Head.Ref == "" || !graphqlHexSHA.MatchString(verified.Head.SHA) || verified.Base.Ref == "" || verified.Base.Repo.FullName != repo {
		h.deny(w, e, http.StatusBadGateway, "pull request read-back was invalid")
		return
	}
	if !policyBranch(verified.Base.Ref, identity.Policy.Spec.GitHubPullRequest.AllowedBaseBranches) {
		h.deny(w, e, http.StatusForbidden, "pull request base branch not permitted by policy")
		return
	}
	if !runOwnedBranch(verified.Head.Ref, string(identity.Run.UID)) {
		h.deny(w, e, http.StatusForbidden, "pull request head branch is not owned by this run")
		return
	}
	e.ArtifactURL = verified.HTMLURL
	h.copyResponse(w, response, body)
	h.auditAllowed(e, response.StatusCode)
}

func runOwnedBranch(branch, runUID string) bool {
	return runUID != "" && strings.HasPrefix(branch, runHeadBranchPrefix(runUID)) && validRefName("refs/heads/"+branch)
}

func runHeadBranchPrefix(runUID string) string { return "sproozi/" + runUID + "/" }

func policyBranch(branch string, allowed []string) bool {
	return validRefName("refs/heads/"+branch) && slices.Contains(allowed, branch)
}

func (h *Handler) doUpstream(r *http.Request, upstreamURL, token string) (*http.Response, error) {
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		return nil, err
	}
	copySafeHeaders(upstream.Header, r.Header)
	upstream.Header.Set("Authorization", "Bearer "+token)
	return h.doHTTP(upstream)
}

// doHTTP makes redirects an explicit, policy-checked operation. A default
// http.Client follows Location responses and could otherwise carry the
// injected provider credential to an untrusted host.
func (h *Handler) doHTTP(request *http.Request) (*http.Response, error) {
	if h.Config.HTTPClient == nil {
		return nil, fmt.Errorf("upstream HTTP client is unavailable")
	}
	client := *h.Config.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if err := budget.FromContext(request.Context()).SpendRequest(); err != nil {
		return nil, err
	}
	return client.Do(request)
}

func copySafeHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "authorization", "proxy-authorization", "cookie", "host", "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "x-forwarded-port":
			continue
		case "connection", "keep-alive", "proxy-authenticate", "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (h *Handler) copyResponse(w http.ResponseWriter, response *http.Response, body []byte) {
	for k, vv := range response.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}
func (h *Handler) auditAllowed(e audit.Event, status int) {
	e.Allowed = true
	e.UpstreamStatus = status
	h.Config.AuditLogger.Log(e)
}

// forward proxies the request to upstreamURL, injecting GitHub App credentials
// and stripping any credential the sandbox supplied. authStyle is "basic" for
// git smart HTTP or "bearer" for the REST API.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, e audit.Event, upstreamURL, authStyle, token string) {
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	upstream.ContentLength = r.ContentLength

	// Copy only non-credential, non-hop-by-hop headers. The request token is
	// authentication for this gateway, never authority for the provider.
	copySafeHeaders(upstream.Header, r.Header)

	// Inject GitHub App credentials.
	switch authStyle {
	case "basic":
		upstream.SetBasicAuth("x-access-token", token)
	default:
		upstream.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.doHTTP(upstream)
	if err != nil {
		h.deny(w, e, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	e.Allowed = true
	e.UpstreamStatus = resp.StatusCode
	h.Config.AuditLogger.Log(e)
}

// deny writes a plain-text HTTP error and logs a denied audit event.
func (h *Handler) deny(w http.ResponseWriter, e audit.Event, status int, reason string) {
	e.Allowed = false
	e.DenyReason = reason
	h.Config.AuditLogger.Log(e)
	http.Error(w, reason, status)
}

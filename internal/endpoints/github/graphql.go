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

package github

// This file deliberately implements only the small GraphQL surface used by
// the pinned gh client. It is not a general GraphQL proxy. In particular,
// arbitrary queries, aliases, extra root fields, mutations, and repository
// identifiers that cannot be resolved to a policy-listed repository fail
// closed before a mutation is sent upstream.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const (
	titleField   = "title"
	nameVariable = "name"
)

const (
	repositoryInfoOperation = "RepositoryInfo"
	repositoryIDField       = "repositoryId"
)

const (
	pullRequestByNumberOperation = "PullRequestByNumber"
	graphqlQuery                 = "query"
	pullRequestCreateOperation   = "PullRequestCreate"
	ownerVariable                = "owner"
	repoVariable                 = "repo"
	baseRefNameField             = "baseRefName"
	variablesField               = "variables"
	inputVariable                = "input"
	headRefNameField             = "headRefName"
)

const maxGraphQLBody = 1 << 20

var graphqlHexSHA = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

type graphQLEnvelope struct {
	Query         string                     `json:"query"`
	Variables     map[string]json.RawMessage `json:"variables"`
	OperationName string                     `json:"operationName"`
}

type graphqlOperation struct {
	Kind string
	Name string
	Root string
}

func isGraphQL(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.Trim(r.URL.Path, "/") == "graphql"
}

func (h *Handler) handleGraphQL(w http.ResponseWriter, r *http.Request, e audit.Event, identity *gateway.RunIdentity) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGraphQLBody+1))
	if err != nil || len(body) == 0 || len(body) > maxGraphQLBody {
		h.deny(w, e, http.StatusBadRequest, "GraphQL request body is invalid")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	envelope, err := decodeGraphQLEnvelope(body)
	if err != nil {
		h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
		return
	}
	operation, err := classifyGraphQL(envelope.Query)
	if err != nil {
		h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
		return
	}
	if envelope.OperationName != "" && envelope.OperationName != operation.Name {
		h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
		return
	}
	if (operation.Name == repositoryInfoOperation || operation.Name == pullRequestByNumberOperation) &&
		(operation.Kind != graphqlQuery || operation.Root != "repository") {
		h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
		return
	}
	if operation.Name != repositoryInfoOperation && operation.Name != pullRequestByNumberOperation && operation.Name != pullRequestCreateOperation {
		h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
		return
	}
	if operation.Name == pullRequestCreateOperation {
		if err := validatePullRequestCreateDocument(envelope.Query); err != nil {
			h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
			return
		}
	}

	// Resolve the repository from variables before forwarding any read. This
	// means the policy decision is based on the requested repository, not on a
	// host or an arbitrary GraphQL selection.
	var repo string
	switch operation.Name {
	case repositoryInfoOperation:
		repo, err = repositoryFromVariables(envelope.Variables, ownerVariable, nameVariable)
	case pullRequestByNumberOperation:
		repo, err = repositoryFromPRViewVariables(envelope.Variables)
	case pullRequestCreateOperation:
		err = validateGraphQLCreateEnvelope(operation, envelope.Variables, string(identity.Run.UID), identity)
	}
	if err != nil {
		h.deny(w, e, http.StatusForbidden, "GraphQL repository or operation is not permitted")
		return
	}
	if repo != "" && !slices.Contains(identity.Policy.Spec.GitHubPullRequest.Repositories, repo) {
		e.Resource = repo
		h.deny(w, e, http.StatusForbidden, "repository not permitted by policy")
		return
	}
	e.Resource = repo

	ghToken, err := h.Config.TokenProvider.Token(r.Context())
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to obtain GitHub credentials")
		return
	}

	if operation.Name != pullRequestCreateOperation {
		upstream, err := url.JoinPath(h.Config.APIBaseURL, "graphql")
		if err != nil {
			h.deny(w, e, http.StatusInternalServerError, "failed to build upstream URL")
			return
		}
		h.forward(w, r, e, upstream, "bearer", ghToken)
		return
	}

	h.handleGraphQLPRCreate(w, r, e, identity, envelope, ghToken)
}

func decodeGraphQLEnvelope(body []byte) (graphQLEnvelope, error) {
	var envelope graphQLEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.Query == "" || envelope.Variables == nil {
		return graphQLEnvelope{}, fmt.Errorf("invalid GraphQL envelope")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return graphQLEnvelope{}, fmt.Errorf("trailing GraphQL envelope data")
	}
	return envelope, nil
}

// classifyGraphQL extracts only the operation header and its direct root
// field. The lexer ignores comments and strings, so a forbidden field in a
// title/body/string literal cannot influence the decision.
func classifyGraphQL(query string) (graphqlOperation, error) {
	tokens, err := lexGraphQL(query)
	if err != nil || len(tokens) == 0 {
		return graphqlOperation{}, fmt.Errorf("invalid GraphQL query")
	}
	// Fragment expansion and spreads are deliberately outside this small,
	// auditable grammar. Rejecting them prevents a forbidden field from being
	// hidden behind an otherwise permitted root selection.
	for _, token := range tokens {
		if token == "fragment" || token == "..." {
			return graphqlOperation{}, fmt.Errorf("GraphQL fragments are not supported")
		}
	}
	i := 0
	if tokens[i] == "{" {
		return graphqlOperation{}, fmt.Errorf("anonymous GraphQL operations are not supported")
	}
	if tokens[i] != graphqlQuery && tokens[i] != "mutation" {
		return graphqlOperation{}, fmt.Errorf("unsupported GraphQL operation type")
	}
	operation := graphqlOperation{Kind: tokens[i]}
	i++
	if i >= len(tokens) || !isGraphQLName(tokens[i]) {
		return graphqlOperation{}, fmt.Errorf("named GraphQL operation required")
	}
	operation.Name = tokens[i]
	// Find the operation's top-level selection set. GraphQL variable types do
	// not contain braces, while strings/comments have already been removed.
	for i < len(tokens) && tokens[i] != "{" {
		i++
	}
	if i == len(tokens) {
		return graphqlOperation{}, fmt.Errorf("GraphQL selection missing")
	}
	return classifyRootSelection(tokens[i:], operation)
}

func classifyRootSelection(tokens []string, operation graphqlOperation) (graphqlOperation, error) {
	depth, parens, brackets := 0, 0, 0
	for i := range len(tokens) {
		token := tokens[i]
		switch token {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				if operation.Root == "" {
					return graphqlOperation{}, fmt.Errorf("GraphQL root field missing")
				}
				return operation, nil
			}
		case "(":
			parens++
		case ")":
			parens--
		case "[":
			brackets++
		case "]":
			brackets--
		default:
			if depth == 1 && parens == 0 && brackets == 0 && isGraphQLName(token) {
				if i+1 < len(tokens) && tokens[i+1] == ":" {
					return graphqlOperation{}, fmt.Errorf("GraphQL aliases are not supported")
				}
				if operation.Root != "" {
					return graphqlOperation{}, fmt.Errorf("multiple GraphQL root fields")
				}
				operation.Root = token
			}
		}
		if depth < 0 || parens < 0 || brackets < 0 {
			return graphqlOperation{}, fmt.Errorf("malformed GraphQL selection")
		}
	}
	return graphqlOperation{}, fmt.Errorf("malformed GraphQL selection")
}

func isGraphQLName(value string) bool {
	if value == "" || (value[0] != '_' && (value[0] < 'A' || value[0] > 'Z') && (value[0] < 'a' || value[0] > 'z')) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if value[i] != '_' && (value[i] < 'A' || value[i] > 'Z') && (value[i] < 'a' || value[i] > 'z') && (value[i] < '0' || value[i] > '9') {
			return false
		}
	}
	return true
}

func lexGraphQL(query string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(query); {
		switch query[i] {
		case ' ', '\t', '\r', '\n', ',':
			i++
		case '#':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case '"':
			if i+2 < len(query) && query[i:i+3] == "\"\"\"" {
				i += 3
				end := strings.Index(query[i:], "\"\"\"")
				if end < 0 {
					return nil, fmt.Errorf("unterminated GraphQL block string")
				}
				i += end + 3
				continue
			}
			i++
			for i < len(query) {
				if query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] == '"' {
					i++
					break
				}
				i++
			}
			if i > len(query) || (i == len(query) && query[len(query)-1] != '"') {
				return nil, fmt.Errorf("unterminated GraphQL string")
			}
		default:
			if isGraphQLNameStart(query[i]) {
				start := i
				i++
				for i < len(query) && isGraphQLNamePart(query[i]) {
					i++
				}
				tokens = append(tokens, query[start:i])
				continue
			}
			if strings.ContainsRune("{}():!$[]=@|&", rune(query[i])) {
				tokens = append(tokens, query[i:i+1])
				i++
				continue
			}
			if query[i] == '.' && i+2 < len(query) && query[i:i+3] == "..." {
				tokens = append(tokens, "...")
				i += 3
				continue
			}
			// Numbers are only expected in variable defaults, which this
			// narrow surface does not need. Retain them as opaque tokens so
			// malformed names cannot become root fields.
			if query[i] >= '0' && query[i] <= '9' || query[i] == '-' {
				start := i
				i++
				for i < len(query) && (query[i] >= '0' && query[i] <= '9' || strings.ContainsRune(".eE+-", rune(query[i]))) {
					i++
				}
				tokens = append(tokens, query[start:i])
				continue
			}
			return nil, fmt.Errorf("invalid GraphQL character")
		}
	}
	return tokens, nil
}

func isGraphQLNameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isGraphQLNamePart(value byte) bool {
	return isGraphQLNameStart(value) || value >= '0' && value <= '9'
}

func repositoryFromVariables(variables map[string]json.RawMessage, ownerKey, repoKey string) (string, error) {
	if len(variables) != 2 {
		return "", fmt.Errorf("unexpected GraphQL variables")
	}
	owner, err := graphqlString(variables[ownerKey])
	if err != nil {
		return "", err
	}
	repo, err := graphqlString(variables[repoKey])
	if err != nil || !validRepoPart(owner) || !validRepoPart(repo) {
		return "", fmt.Errorf("invalid GraphQL repository")
	}
	return owner + "/" + repo, nil
}

func repositoryFromPRViewVariables(variables map[string]json.RawMessage) (string, error) {
	if len(variables) != 3 {
		return "", fmt.Errorf("unexpected GraphQL variables")
	}
	if _, err := graphqlPositiveInt(variables["pr_number"]); err != nil {
		return "", err
	}
	return repositoryFromVariables(map[string]json.RawMessage{
		ownerVariable: variables[ownerVariable],
		repoVariable:  variables[repoVariable],
	}, ownerVariable, repoVariable)
}

func graphqlPositiveInt(raw json.RawMessage) (int64, error) {
	var value int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value <= 0 {
		return 0, fmt.Errorf("GraphQL variable must be a positive integer")
	}
	return value, nil
}

func validRepoPart(value string) bool {
	if value == "" || len(value) > 100 || value == "." || value == ".." || strings.ContainsAny(value, "/\\:@?#%\x00\r\n") {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func graphqlString(raw json.RawMessage) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value == "" {
		return "", fmt.Errorf("GraphQL variable must be a non-empty string")
	}
	return value, nil
}

func validateGraphQLCreateEnvelope(operation graphqlOperation, variables map[string]json.RawMessage, runUID string, identity *gateway.RunIdentity) error {
	if operation.Kind != "mutation" || operation.Root != "createPullRequest" || len(variables) != 1 {
		return fmt.Errorf("unsupported GraphQL mutation")
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(variables[inputVariable], &input) != nil || input == nil {
		return fmt.Errorf("invalid GraphQL mutation input")
	}
	for key := range input {
		switch key {
		case baseRefNameField, "body", "draft", headRefNameField, "maintainerCanModify", repositoryIDField, titleField:
		default:
			return fmt.Errorf("unsupported GraphQL mutation input")
		}
	}
	head, err := graphqlString(input[headRefNameField])
	if err != nil || !runOwnedBranch(head, runUID) {
		return fmt.Errorf("head branch is outside this run")
	}
	base, err := graphqlString(input[baseRefNameField])
	if err != nil || !policyBranch(base, identity.Policy.Spec.GitHubPullRequest.AllowedBaseBranches) {
		return fmt.Errorf("base branch is outside policy")
	}
	if _, err := graphqlString(input[repositoryIDField]); err != nil {
		return fmt.Errorf("repository ID is required")
	}
	if _, err := graphqlString(input[titleField]); err != nil {
		return fmt.Errorf("pull request title is required")
	}
	return nil
}

type graphQLMutationResponse struct {
	Data struct {
		CreatePullRequest struct {
			PullRequest struct {
				URL string `json:"url"`
			} `json:"pullRequest"`
		} `json:"createPullRequest"`
	} `json:"data"`
	Errors json.RawMessage `json:"errors"`
}

func (h *Handler) handleGraphQLPRCreate(w http.ResponseWriter, r *http.Request, e audit.Event, identity *gateway.RunIdentity, envelope graphQLEnvelope, token string) {
	input := make(map[string]json.RawMessage)
	if err := json.Unmarshal(envelope.Variables[inputVariable], &input); err != nil {
		h.deny(w, e, http.StatusForbidden, "GraphQL operation is not permitted")
		return
	}
	repositoryID, _ := graphqlString(input[repositoryIDField])
	repo, err := h.resolveGraphQLRepositoryID(r, token, repositoryID)
	if err != nil || !slices.Contains(identity.Policy.Spec.GitHubPullRequest.Repositories, repo) {
		h.deny(w, e, http.StatusForbidden, "repository not permitted by policy")
		return
	}
	e.Resource = repo

	upstream, err := url.JoinPath(h.Config.APIBaseURL, "graphql")
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build upstream URL")
		return
	}
	payload, err := cannedPullRequestCreatePayload(input)
	if err != nil {
		h.deny(w, e, http.StatusInternalServerError, "failed to build GraphQL operation")
		return
	}
	forwarded := r.Clone(r.Context())
	forwarded.Body = io.NopCloser(bytes.NewReader(payload))
	response, err := h.doUpstream(forwarded, upstream, token)
	if err != nil {
		h.deny(w, e, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGraphQLBody+1))
	if err != nil || len(body) > maxGraphQLBody {
		h.deny(w, e, http.StatusBadGateway, "upstream GraphQL response was invalid")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		h.copyResponse(w, response, body)
		h.auditAllowed(e, response.StatusCode)
		return
	}
	var created graphQLMutationResponse
	if json.Unmarshal(body, &created) != nil || len(created.Errors) != 0 || created.Data.CreatePullRequest.PullRequest.URL == "" {
		h.deny(w, e, http.StatusBadGateway, "upstream GraphQL mutation response was invalid")
		return
	}
	repoFromURL, number, err := parseGitHubPRURL(created.Data.CreatePullRequest.PullRequest.URL)
	if err != nil || repoFromURL != repo {
		h.deny(w, e, http.StatusBadGateway, "upstream pull request identity was invalid")
		return
	}
	verified, err := h.readBackGraphQLPR(r, token, repo, number)
	if err != nil || verified.Repository != repo || verified.Number != number || verified.URL != created.Data.CreatePullRequest.PullRequest.URL || !runOwnedBranch(verified.HeadRef, string(identity.Run.UID)) || !policyBranch(verified.BaseRef, identity.Policy.Spec.GitHubPullRequest.AllowedBaseBranches) || !graphqlHexSHA.MatchString(verified.HeadOID) {
		h.deny(w, e, http.StatusBadGateway, "pull request read-back was invalid")
		return
	}
	e.ArtifactURL = verified.URL
	h.copyResponse(w, response, body)
	h.auditAllowed(e, response.StatusCode)
}

type verifiedGraphQLPR struct {
	Number     int64
	URL        string
	Repository string
	HeadRef    string
	HeadOID    string
	BaseRef    string
}

func (h *Handler) resolveGraphQLRepositoryID(r *http.Request, token, repositoryID string) (string, error) {
	if repositoryID == "" {
		return "", fmt.Errorf("missing repository ID")
	}
	query := `query RepositoryByID($id: ID!) { node(id: $id) { ... on Repository { nameWithOwner } } }`
	body, _ := json.Marshal(map[string]any{graphqlQuery: query, variablesField: map[string]string{"id": repositoryID}})
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.Config.APIBaseURL+"/graphql", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := h.doUpstream(request, request.URL.String(), token)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	var payload struct {
		Data struct {
			Node struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"node"`
		} `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, maxGraphQLBody)).Decode(&payload) != nil || len(payload.Errors) != 0 || !validRepositoryName(payload.Data.Node.NameWithOwner) {
		return "", fmt.Errorf("repository ID did not resolve")
	}
	return payload.Data.Node.NameWithOwner, nil
}

func validRepositoryName(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validRepoPart(parts[0]) && validRepoPart(parts[1])
}

func parseGitHubPRURL(value string) (string, int64, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", 0, fmt.Errorf("invalid pull request URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || !validRepoPart(parts[0]) || !validRepoPart(parts[1]) {
		return "", 0, fmt.Errorf("invalid pull request URL")
	}
	number, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("invalid pull request number")
	}
	return parts[0] + "/" + parts[1], number, nil
}

func (h *Handler) readBackGraphQLPR(r *http.Request, token, repository string, number int64) (verifiedGraphQLPR, error) {
	parts := strings.Split(repository, "/")
	query := `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number) { number url headRefName headRefOid baseRefName repository { nameWithOwner } } } }`
	payload, _ := json.Marshal(map[string]any{graphqlQuery: query, variablesField: map[string]any{ownerVariable: parts[0], repoVariable: parts[1], "pr_number": number}})
	request, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, h.Config.APIBaseURL+"/graphql", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response, err := h.doUpstream(request, request.URL.String(), token)
	if err != nil {
		return verifiedGraphQLPR{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var result struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					Number     int64  `json:"number"`
					URL        string `json:"url"`
					HeadRef    string `json:"headRefName"`
					HeadOID    string `json:"headRefOid"`
					BaseRef    string `json:"baseRefName"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, maxGraphQLBody)).Decode(&result) != nil || len(result.Errors) != 0 || result.Data.Repository.PullRequest == nil {
		return verifiedGraphQLPR{}, fmt.Errorf("invalid pull request read-back")
	}
	pr := result.Data.Repository.PullRequest
	return verifiedGraphQLPR{Number: pr.Number, URL: pr.URL, Repository: pr.Repository.NameWithOwner, HeadRef: pr.HeadRef, HeadOID: pr.HeadOID, BaseRef: pr.BaseRef}, nil
}

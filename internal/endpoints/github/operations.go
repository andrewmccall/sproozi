package github

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// pullRequestCreateDocument is the only mutation document this capability
// forwards. The values are supplied as variables after they have been
// authorized, so the document cannot smuggle a different repository or ref.
const pullRequestCreateDocument = `mutation PullRequestCreate($input: CreatePullRequestInput!) { createPullRequest(input: $input) { pullRequest { id url } } }`

func validatePullRequestCreateDocument(query string) error {
	got, err := lexGraphQL(query)
	if err != nil {
		return err
	}
	want, err := lexGraphQL(pullRequestCreateDocument)
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("unsupported GraphQL mutation document")
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("unsupported GraphQL mutation document")
		}
	}
	return nil
}

func cannedPullRequestCreatePayload(input map[string]json.RawMessage) ([]byte, error) {
	return json.Marshal(graphQLEnvelope{
		Query:         pullRequestCreateDocument,
		Variables:     map[string]json.RawMessage{inputVariable: mustJSON(input)},
		OperationName: pullRequestCreateOperation,
	})
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

// boundedReadDocument accepts the fragment-free client surface. The fixed
// headers bind every argument to the variables subsequently authorized by the
// handler; the finite selection trees permit only scalar repository/PR metadata.
// Reconstructing the validated tokens makes authorization and forwarding share
// exactly one document, including its end (no trailing operation or directives).
func boundedReadDocument(operation, query string) (string, error) {
	tokens, err := lexGraphQL(query)
	if err != nil {
		return "", err
	}
	var prefix string
	var fields readFields
	suffix := []string{"}"}
	switch operation {
	case repositoryInfoOperation:
		prefix = `query RepositoryInfo($owner: String!, $name: String!) { repository(owner: $owner, name: $name)`
		fields = readFields{"id": nil, nameVariable: nil, "nameWithOwner": nil, ownerVariable: {"login": nil}, "defaultBranchRef": {nameVariable: nil}}
	case pullRequestByNumberOperation:
		prefix = `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number)`
		fields = readFields{"id": nil, "number": nil, "url": nil, titleField: nil, bodyField: nil, "state": nil, headRefNameField: nil, "headRefOid": nil, baseRefNameField: nil, "repository": {"nameWithOwner": nil}}
		suffix = append(suffix, "}")
	default:
		return "", fmt.Errorf("unsupported GraphQL read")
	}
	header, _ := lexGraphQL(prefix)
	if len(tokens) < len(header) || !slices.Equal(tokens[:len(header)], header) {
		return "", fmt.Errorf("unsupported GraphQL read arguments")
	}
	rest, err := readSelection(tokens[len(header):], fields)
	if err != nil || !slices.Equal(rest, suffix) {
		return "", fmt.Errorf("unsupported GraphQL read selection")
	}
	return strings.Join(tokens, " "), nil
}

type readFields map[string]readFields

func readSelection(tokens []string, fields readFields) ([]string, error) {
	if len(tokens) == 0 || tokens[0] != "{" {
		return nil, fmt.Errorf("selection required")
	}
	tokens = tokens[1:]
	seen := make(map[string]bool)
	for len(tokens) > 0 && tokens[0] != "}" {
		name := tokens[0]
		nested, ok := fields[name]
		if !ok || seen[name] {
			return nil, fmt.Errorf("unsupported or duplicate field")
		}
		seen[name] = true
		tokens = tokens[1:]
		if nested != nil {
			var err error
			tokens, err = readSelection(tokens, nested)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(tokens) == 0 || len(seen) == 0 {
		return nil, fmt.Errorf("empty or incomplete selection")
	}
	return tokens[1:], nil
}

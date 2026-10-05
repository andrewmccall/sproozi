package github

import (
	"encoding/json"
	"fmt"
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

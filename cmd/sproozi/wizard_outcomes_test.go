package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
)

// wizardOutcomesFile records, for every registered selector, an unregistered
// one and the empty kind, what the setup wizard asks and which sign-in each
// answer chooses. A change that only moves the sign-in choices leaves it
// byte-identical; regenerate it with SPROOZI_UPDATE_OUTCOMES=1 only for a
// change meant to alter it.
const wizardOutcomesFile = "testdata/wizard-outcomes.json"

func TestWizardOutcomesAreUnchanged(t *testing.T) {
	type outcome struct {
		Kind    string `json:"kind"`
		Answer  int    `json:"answer"`
		Asked   string `json:"asked"`
		SignIn  bool   `json:"signIn"`
		Token   bool   `json:"token"`
		ChatGPT bool   `json:"chatgpt"`
		Error   string `json:"error"`
	}
	var outcomes []outcome
	kinds := append(harness.Selectors(), "not-registered", "")
	for _, kind := range kinds {
		for answer := 1; answer <= 4; answer++ {
			q := &options{harness: kind}
			var out strings.Builder
			answers := bufio.NewReader(strings.NewReader(fmt.Sprintf("%d\n%d\n", answer, answer)))
			err := q.wizard(answers, &out, localsetup.Installation{})
			o := outcome{Kind: kind, Answer: answer, Asked: out.String(), SignIn: q.signIn, Token: q.token, ChatGPT: q.chatgpt}
			if err != nil {
				o.Error = err.Error()
			}
			outcomes = append(outcomes, o)
		}
	}
	// An agent already set up is offered its current choice first.
	for _, kind := range harness.ManagedKinds() {
		q := &options{harness: string(kind)}
		var out strings.Builder
		inst := localsetup.Installation{Harnesses: []localsetup.Harness{{Harness: kind}}}
		err := q.wizard(bufio.NewReader(strings.NewReader("1\n")), &out, inst)
		o := outcome{Kind: string(kind) + " (set up)", Answer: 1, Asked: out.String(), SignIn: q.signIn, Token: q.token,
			ChatGPT: q.chatgpt}
		if err != nil {
			o.Error = err.Error()
		}
		outcomes = append(outcomes, o)
	}
	got, err := json.MarshalIndent(outcomes, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("SPROOZI_UPDATE_OUTCOMES") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wizardOutcomesFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(wizardOutcomesFile)
	if err != nil {
		t.Fatalf("%v; record the outcomes with SPROOZI_UPDATE_OUTCOMES=1", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the setup wizard's questions or choices changed; %s records them. If the change is meant, "+
			"regenerate it with SPROOZI_UPDATE_OUTCOMES=1 and show the difference in its own commit", wizardOutcomesFile)
	}
}

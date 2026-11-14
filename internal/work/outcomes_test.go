//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// Work submitted with the work token takes its own cap; the same request
// from the home principal takes the executor's, here none.
func TestWorkTokenSubmissionsTakeTheirOwnCap(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.MaxDuration = 0
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	taskToken, home := strings.Repeat("t", 32), strings.Repeat("h", 32)
	h, err := NewHandler(o, taskToken, homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{taskToken, http.StatusForbidden}, {home, http.StatusOK}} {
		b, _ := json.Marshal(Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: c.Harness,
			Executor: c.Placement, Task: fixtureTask, MaxDuration: "none"})
		r := httptest.NewRequest(http.MethodPost, "/work/run", strings.NewReader(string(b)))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("no deadline with token %q…: %d %s, want %d", tc.token[:1], w.Code, w.Body.String(), tc.want)
		}
	}
}

func TestWorkHTTPAndMCPShareActionableRejections(t *testing.T) {
	t.Parallel()
	c, _ := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	token := strings.Repeat("t", 32)
	h, err := NewHandler(o, token, homeToken(strings.Repeat("h", 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		input  Submit
		status int
		code   profile.Code
	}{
		{"invalid", Submit{}, 400, profile.CodeInvalid},
		{"project identity", projectRequest("bad/project"), 400, profile.CodeInvalid},
		{"project member", projectRequest("private"), 403, profile.CodeDenied},
		{"project repository", Submit{RequestID: uuid.NewString(), Project: "sproozi-dev", Harness: c.Harness, Executor: c.Placement, Repository: "different", Task: fixtureTask}, 403, profile.CodeDenied},
		{"repository", Submit{RequestID: uuid.NewString(), Harness: c.Harness, Executor: c.Placement, Repository: "unapproved", Task: fixtureTask}, 403, profile.CodeDenied},
		{"harness", Submit{RequestID: uuid.NewString(), Harness: harness.NativeClaude, Executor: c.Placement, Repository: repositorySproozi, Task: fixtureTask}, 422, profile.CodeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.input)
			r := httptest.NewRequest(http.MethodPost, "/work/run", strings.NewReader(string(b)))
			r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			var failure struct {
				Error profile.Code `json:"error"`
			}
			if err = json.Unmarshal(w.Body.Bytes(), &failure); err != nil || w.Code != tc.status || failure.Error != tc.code {
				t.Fatalf("got %d %s: %v", w.Code, w.Body.String(), err)
			}
			_, refusal := o.Submit(t.Context(), tc.input)
			result, _, err := tool(View{}, refusal)
			content, marshalErr := json.Marshal(result)
			if err != nil || marshalErr != nil || result == nil || !result.IsError || !strings.Contains(string(content), string(tc.code)) {
				t.Fatalf("MCP rejection missing stable outcome: %+v %v", result, err)
			}
		})
	}
}

func TestWorkStatusRetainsSafeAsyncRecoveryCategory(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	p.refusePrepare = &profile.Rejection{Code: profile.CodeDenied}
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	initial := submit(t, o)
	status := waitDriver(t, o, initial.ID)
	if status.Phase != phasePreparing {
		t.Fatalf("refused preparation: %+v", status)
	}
	if !strings.HasPrefix(status.Problem, "denied:") || !strings.Contains(status.Problem, "approved repository") || !strings.Contains(status.Problem, "recover this session") {
		t.Fatalf("status lost actionable category: %+v", status)
	}
	if status.CheckpointDigest != "" || status.ThreadID != "" {
		t.Fatalf("refused preparation manufactured completion: %+v", status)
	}
}

func TestExportProjectMetadataRejectionsRemainUnsupported(t *testing.T) {
	t.Parallel()
	c, _ := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	exported, err := o.BeginExport(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project", "reference", "metadata"} {
		t.Run(name, func(t *testing.T) {
			invalid := exported
			switch name {
			case "project":
				invalid.Project = "bad/project"
			case "reference":
				invalid.RepositoryReference = "https://user:secret@example.invalid/repo"
			case "metadata":
				metadata := *exported.CheckpointInfo
				metadata.ThreadID = "different-thread"
				invalid.CheckpointInfo = &metadata
			}
			err := ValidateExport(invalid, c.MaxCheckpointBytes)
			if err == nil || OutcomeCode(err) != profile.CodeUnsupported {
				t.Fatalf("Invalid %s lost safe compatibility outcome: %v", name, err)
			}
		})
	}
}

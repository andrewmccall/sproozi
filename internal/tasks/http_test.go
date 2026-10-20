package tasks_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/tasks"
)

const taskToken = "0123456789abcdef0123456789abcdef"

// stubService answers task calls without a cluster.
type stubService struct{}

func (stubService) Submit(context.Context, tasks.Submit) (tasks.View, error) {
	return tasks.View{}, nil
}
func (stubService) Status(_ context.Context, r tasks.Reference) (tasks.View, error) {
	return tasks.View{Reference: r, Phase: "Running"}, nil
}
func (stubService) Wait(context.Context, tasks.Reference, time.Duration) (tasks.View, error) {
	return tasks.View{}, nil
}
func (stubService) Cancel(context.Context, tasks.Reference) (tasks.View, error) {
	return tasks.View{}, errors.New("not found")
}

// countingBody records whether the handler read the request body to the end.
type countingBody struct {
	io.Reader
	drained bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.drained = true
	}
	return n, err
}

func (*countingBody) Close() error { return nil }

func TestRejectedRequestBodyIsReadBeforeTheResponse(t *testing.T) {
	handler, err := tasks.NewHandler(stubService{}, taskToken)
	if err != nil {
		t.Fatal(err)
	}
	for name, header := range map[string]string{"missing": "", "wrong": "Bearer " + strings.Repeat("x", 32)} {
		body := &countingBody{Reader: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)}
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request.Body = body
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || !body.drained {
			t.Fatalf("%s token: status %d, body drained %v", name, response.Code, body.drained)
		}
	}
}

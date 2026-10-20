package tasks_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/tasks"
)

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

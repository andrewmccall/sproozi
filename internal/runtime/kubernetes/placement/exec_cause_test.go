package placement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The kept cause is one line of at most maxCause bytes, whatever the exec
// reported. Guards: the bound and the joining of lines in execCause.
func TestAFailedCommandKeepsOneBoundedLine(t *testing.T) {
	t.Parallel()
	long := execCause(errors.New(strings.Repeat("kubelet said ", 1000)))
	if len(long) > maxCause+len("…") || !strings.HasSuffix(long, "…") {
		t.Fatalf("a cause of %d bytes was kept: %q", len(long), long[:min(len(long), 80)])
	}
	if lines := execCause(errors.New("first line\nsecond\tline\r\nthird")); lines != "first line second line third" {
		t.Fatalf("a cause on several lines was kept as %q", lines)
	}
}

// Each layer of execCause's redaction, with a case that only that layer
// catches, and the runtime's own identifiers, which none removes. Guards:
// the url.Error branch (a URL the pattern would not match), the pattern's
// http(s)/ws(s) alternative (a URL with no exec query), its exec-query
// alternative (a query with no scheme, plain or percent-encoded), and that
// the pattern leaves containerd and socket addresses and other quoted text
// alone.
func TestEachLayerOfAFailedCommandsRedaction(t *testing.T) {
	t.Parallel()
	secret := "secret-notes.env"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"a url.Error in the chain keeps its operation and inner error",
			fmt.Errorf("error sending request: %w", &url.Error{Op: "Post",
				URL: "/api/v1/namespaces/sproozi-agents/pods/run-abc/exec", Err: io.EOF}),
			"Post request: EOF"},
		{"an http URL in the text is removed",
			errors.New(`error sending request: Post "https://10.0.0.1:6443/api/v1/namespaces/a/pods/b/exec": EOF`),
			"error sending request: Post request: EOF"},
		{"a websocket URL in the text is removed",
			errors.New(`dial wss://10.0.0.1:6443/api/v1/namespaces/a/pods/b/exec: EOF`),
			"dial request EOF"},
		{"an exec query without a scheme is removed",
			errors.New("upgrade failed for /api/v1/namespaces/a/pods/b/exec?command=%2Fusr%2Flocal%2Fbin%2Fcheckpoint&command=" + secret + " EOF"),
			"upgrade failed for request EOF"},
		{"a percent-encoded exec query is removed",
			errors.New("upgrade failed for /api/v1/namespaces/a/pods/b/exec%3Fcommand%3D%252Fusr%26command%3D" + secret + " EOF"),
			"upgrade failed for request EOF"},
		{"a runtime's container ID and socket are kept",
			errors.New("failed to exec in container containerd://abc123: dial unix:///run/containerd/containerd.sock: connection refused"),
			"failed to exec in container containerd://abc123: dial unix:///run/containerd/containerd.sock: connection refused"},
		{"other quoted text is kept",
			errors.New(`error stream protocol error: invalid exit code value "x?y=1"`),
			`error stream protocol error: invalid exit code value "x?y=1"`},
	} {
		if got := execCause(test.err); got != test.want || strings.Contains(got, secret) {
			t.Errorf("%s: kept %q, want %q", test.name, got, test.want)
		}
	}
}

// failingExec fails every command with err(args, container).
type failingExec struct {
	err func(args []string, container string) error
}

func (f failingExec) Execute(_ context.Context, _, _, container string, args []string, _ io.Reader, _, _ io.Writer) error {
	return f.err(args, container)
}
func (failingExec) Logs(context.Context, string, string, string) ([]byte, error) { return nil, nil }

// execURL is the exec request URL client-go builds for args: every
// argument is a command parameter of its query.
func execURL(args []string, container string) string {
	query := url.Values{"command": args, "container": {container}}
	for _, stream := range []string{"stdout", "stderr"} {
		query.Set(stream, strconv.FormatBool(true))
	}
	return "https://10.0.0.1:6443/api/v1/namespaces/sproozi-agents/pods/run-abc/exec?" + query.Encode()
}

// The error client-go returns when the exec request itself fails carries the
// whole request URL, whose query holds every argument of the command: files a
// user selected, the thread, the instruction digest. A failed fixed command
// keeps the request's operation and its inner error, never the URL, for each
// fixed command, whether the error is wrapped as client-go's spdy.Negotiate
// wraps it (%v, so the url.Error is only text) or kept in the chain. Guards:
// execCause's removal of URLs, and operation's omission of arguments.
func TestAFailedCommandKeepsNoneOfItsArguments(t *testing.T) {
	t.Parallel()
	// The arguments as the placement passes them, with a selected file, a
	// thread and a digest that must not be kept.
	restore := strings.Fields("--harness codex --instruction-digest deadbeefdigest --max-bytes 1024")
	capture := append(strings.Fields("--home /workspace/session/codex --workspace /workspace/session/workspace "+
		"--thread 0190aaaa-thread --include-ignored secret-notes.env"), restore...)
	commands := map[string][]string{
		"capture-stopped": append([]string{helperPath, helperCapture}, capture...),
		"prepare":         append([]string{helperPath, helperPrepare}, restore...),
		"finish":          {helperPath, helperFinish},
	}
	for _, op := range []harness.NativeOp{harness.NativeGate, harness.NativeStatus, harness.NativeQuiesce, harness.NativeActivate} {
		commands[string(op)] = []string{nativePath, string(op)}
	}
	wraps := map[string]func(*url.Error) error{
		"as client-go wraps it": func(u *url.Error) error { return fmt.Errorf("error sending request: %v", u) },
		"in the chain":          func(u *url.Error) error { return fmt.Errorf("error sending request: %w", u) },
	}
	p, _, _, _, _ := setup(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: runtimekube.AgentsNamespace, Name: "run-abc"}}
	for name, args := range commands {
		for how, wrap := range wraps {
			container := runtimekube.CaptureContainer
			if args[0] == nativePath {
				container = nativeContainer
			}
			p.exec = failingExec{err: func(args []string, container string) error {
				return wrap(&url.Error{Op: "Post", URL: execURL(args, container),
					Err: errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")})
			}}
			_, err := p.control(t.Context(), pod, container, args, nil, 1024)
			if err == nil {
				t.Fatalf("%s %s: the command did not fail", name, how)
			}
			text := err.Error()
			for _, kept := range append([]string{"command=", "?", "://", "%2F", "run-abc/exec"}, args[2:]...) {
				if strings.Contains(text, kept) {
					t.Errorf("%s %s: the error keeps %q: %s", name, how, kept, text)
				}
			}
			if !strings.Contains(text, "Post") || !strings.Contains(text, "connection refused") ||
				!strings.Contains(text, " "+args[1]+" in container "+container) {
				t.Errorf("%s %s: the error lost the operation or its cause: %s", name, how, text)
			}
		}
	}
}

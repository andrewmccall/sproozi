package placement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
	query := url.Values{"command": args, "container": {container}, "stdout": {"true"}, "stderr": {"true"}}
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
	ignored, thread, digest := "secret-notes.env", "0190aaaa-thread", "deadbeefdigest"
	commands := map[string][]string{
		"capture-stopped": {helperPath, helperCapture, "--harness", "codex", "--home", "/workspace/session/codex",
			"--workspace", "/workspace/session/workspace", "--thread", thread, "--instruction-digest", digest,
			"--max-bytes", "1024", "--include-ignored", ignored},
		"prepare": {helperPath, helperPrepare, "--harness", "codex", "--instruction-digest", digest, "--max-bytes", "1024"},
		"finish":  {helperPath, helperFinish},
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

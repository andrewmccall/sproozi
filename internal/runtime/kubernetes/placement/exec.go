package placement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type commandExecutor interface {
	Execute(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error
	// Logs reads the last lines a container wrote, where the native
	// supervisor writes its exit report.
	Logs(ctx context.Context, namespace, pod, container string) ([]byte, error)
}

// exitLogLines and exitLogBytes bound the output read for an exit report,
// as the Docker placement reads the last 50 lines.
const (
	exitLogLines = int64(50)
	exitLogBytes = int64(64 << 10)
)

type executor struct {
	config *rest.Config
	client *kubernetes.Clientset
}

func newExecutor(config *rest.Config) (commandExecutor, error) {
	c, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &executor{config: rest.CopyConfig(config), client: c}, nil
}
func (e *executor) Execute(ctx context.Context, namespace, pod, container string, command []string, input io.Reader, output, errout io.Writer) error {
	u := e.client.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(pod).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: container, Command: command, Stdin: input != nil, Stdout: true, Stderr: true, TTY: false}, scheme.ParameterCodec).URL()
	x, err := remotecommand.NewSPDYExecutor(e.config, "POST", u)
	if err != nil {
		return err
	}
	return x.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: input, Stdout: output, Stderr: errout})
}

func (e *executor) Logs(ctx context.Context, namespace, pod, container string) ([]byte, error) {
	lines, limit := exitLogLines, exitLogBytes
	return e.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &lines, LimitBytes: &limit}).DoRaw(ctx)
}

// boundedOutput keeps at most max bytes of a command's output. It does not
// embed bytes.Buffer: io.Copy would then use Buffer.ReadFrom and bypass Write.
// Output past the bound fails, unless truncate keeps the first max bytes and
// discards the rest.
type boundedOutput struct {
	buffer   bytes.Buffer
	max      int64
	truncate bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	room := b.max - int64(b.buffer.Len())
	if int64(len(data)) <= room {
		return b.buffer.Write(data)
	}
	if !b.truncate {
		return 0, errors.New("placement: command output exceeds bound")
	}
	_, _ = b.buffer.Write(data[:max(room, 0)])
	return len(data), nil
}

func (b *boundedOutput) Bytes() []byte { return b.buffer.Bytes() }

func (p *Placement) control(ctx context.Context, pod *corev1.Pod, container string, args []string, input []byte, max int64) ([]byte, error) {
	output, errout := &boundedOutput{max: max}, &boundedOutput{max: 16 << 10, truncate: true}
	var reader io.Reader
	if input != nil {
		reader = bytes.NewReader(input)
	}
	err := p.exec.Execute(ctx, pod.Namespace, pod.Name, container, args, reader, output, errout)
	if err != nil {
		code, ok := checkpoint.HelperExitCode(err)
		return nil, &commandFailed{code: code, exited: ok, command: operation(args), container: container, cause: execCause(err)}
	}
	return output.Bytes(), nil
}

// commandFailed is a fixed command that failed. exited reports that the
// command ran and ended with code, as the exec reported it; otherwise the
// exec or its stream failed. cause is the exec's own error text, which says
// which (execCause). The command's output, which the sandbox can write to,
// is never kept.
type commandFailed struct {
	code      int
	exited    bool
	command   string
	container string
	cause     string
}

func (f *commandFailed) Error() string {
	return fmt.Sprintf("placement: fixed command unavailable: %s in container %s: %s", f.command, f.container, f.cause)
}

// operation names a fixed command by its executable and operation, leaving
// out its arguments, such as the files a capture selects.
func operation(args []string) string {
	if len(args) < 2 {
		return strings.Join(args, " ")
	}
	return path.Base(args[0]) + " " + args[1]
}

// maxCause bounds the exec error text a failed command keeps.
const maxCause = 512

// requestURL matches what the text of an exec error may hold of its request:
// a URL, quoted or not, or any word with a query. The exec request's URL
// carries every argument of the command in its query, such as the files a
// capture selects, its thread and the instruction digest.
var requestURL = regexp.MustCompile(`"?[A-Za-z][A-Za-z0-9+.-]*://[^\s"]*"?|[^\s"]*\?[^\s"]*=[^\s"]*`)

// execCause is err's text on one line, at most maxCause bytes, without the
// exec request's URL. It comes from the exec client, the API server, the
// kubelet and the container runtime: an exit code, a broken or reset stream,
// a refused connection, a container that is gone, a timeout, never the
// command's own output, which travels on its own streams. A *url.Error in the
// chain keeps its operation and inner error; client-go also folds one into
// plain text (spdy.Negotiate's "error sending request: %v"), so any URL left
// in the text is replaced as well.
func execCause(err error) string {
	text := err.Error()
	var request *url.Error
	if errors.As(err, &request) {
		text = request.Op + " request: " + request.Err.Error()
	}
	text = requestURL.ReplaceAllString(text, "request")
	cause := strings.ToValidUTF8(strings.Join(strings.Fields(text), " "), "?")
	if len(cause) > maxCause {
		cause = strings.ToValidUTF8(cause[:maxCause], "") + "…"
	}
	return cause
}

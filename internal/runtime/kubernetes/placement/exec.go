package placement

import (
	"bytes"
	"context"
	"errors"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type commandExecutor interface {
	Execute(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error
}
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

type boundedOutput struct {
	bytes.Buffer
	max int64
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if int64(b.Len())+int64(len(data)) > b.max {
		return 0, errors.New("placement: command output exceeds bound")
	}
	return b.Buffer.Write(data)
}
func (p *Placement) control(ctx context.Context, pod *corev1.Pod, container string, args []string, input []byte, max int64) ([]byte, error) {
	output, errout := &boundedOutput{max: max}, &boundedOutput{max: 16 << 10}
	var reader io.Reader
	if input != nil {
		reader = bytes.NewReader(input)
	}
	err := p.exec.Execute(ctx, pod.Namespace, pod.Name, container, args, reader, output, errout)
	if err != nil {
		return nil, errors.New("placement: fixed command unavailable")
	}
	return output.Bytes(), nil
}

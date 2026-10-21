package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// portForwarder opens one Kubernetes port-forward stream per connection, as
// "kubectl port-forward" does for each accepted local connection, but hands the
// stream straight to the caller instead of listening on a local port. It tries
// the WebSocket protocol first and falls back to SPDY for older API servers.
func portForwarder(config *rest.Config, kube kubernetes.Interface, namespace string) func(context.Context, string, int32) (net.Conn, error) {
	return func(ctx context.Context, pod string, port int32) (net.Conn, error) {
		u := kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
		roundTripper, upgrader, err := spdy.RoundTripperFor(config)
		if err != nil {
			return nil, err
		}
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, u)
		if websocket, wsErr := portforward.NewSPDYOverWebsocketDialer(u, config); wsErr == nil {
			dialer = portforward.NewFallbackDialer(websocket, dialer, func(err error) bool {
				return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
			})
		}
		streams, err := dialStreams(ctx, dialer)
		if err != nil {
			return nil, err
		}
		headers := http.Header{}
		headers.Set(corev1.StreamType, corev1.StreamTypeError)
		headers.Set(corev1.PortHeader, strconv.Itoa(int(port)))
		headers.Set(corev1.PortForwardRequestIDHeader, "0")
		failures, err := streams.CreateStream(headers)
		if err != nil {
			_ = streams.Close()
			return nil, err
		}
		_ = failures.Close() // Nothing is written to the error stream.
		headers.Set(corev1.StreamType, corev1.StreamTypeData)
		data, err := streams.CreateStream(headers)
		if err != nil {
			_ = streams.Close()
			return nil, err
		}
		t := &tunnel{streams: streams, data: data}
		go t.watch(failures)
		return t, nil
	}
}

// dialStreams runs the blocking upgrade, abandoning it when ctx ends.
func dialStreams(ctx context.Context, dialer httpstream.Dialer) (httpstream.Connection, error) {
	type result struct {
		streams httpstream.Connection
		err     error
	}
	done := make(chan result, 1)
	go func() {
		streams, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		done <- result{streams, err}
	}()
	select {
	case r := <-done:
		return r.streams, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.streams != nil {
				_ = r.streams.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// tunnel is one port-forward data stream used as a net.Conn. TLS to the
// executor runs over it, so the stream carries only ciphertext. Deadlines are
// not supported by the stream; callers bound requests with contexts, which
// close the connection.
type tunnel struct {
	streams httpstream.Connection
	data    httpstream.Stream
	mu      sync.Mutex
	failed  error
}

// watch records a failure the kubelet reports, such as nothing listening on
// the port, and ends the tunnel so blocked reads return it.
func (t *tunnel) watch(failures io.Reader) {
	message, err := io.ReadAll(io.LimitReader(failures, 4096))
	if err != nil || len(message) == 0 {
		return
	}
	t.mu.Lock()
	t.failed = fmt.Errorf("port-forward to the remote executor failed: %s", message)
	t.mu.Unlock()
	_ = t.streams.Close()
}

func (t *tunnel) failure(err error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return t.failed
	}
	return err
}

func (t *tunnel) Read(p []byte) (int, error) {
	n, err := t.data.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = t.failure(err)
	}
	return n, err
}

func (t *tunnel) Write(p []byte) (int, error) {
	n, err := t.data.Write(p)
	if err != nil {
		err = t.failure(err)
	}
	return n, err
}

func (t *tunnel) Close() error {
	_ = t.data.Reset()
	return t.streams.Close()
}

func (t *tunnel) LocalAddr() net.Addr                { return tunnelAddr{} }
func (t *tunnel) RemoteAddr() net.Addr               { return tunnelAddr{} }
func (t *tunnel) SetDeadline(_ time.Time) error      { return nil }
func (t *tunnel) SetReadDeadline(_ time.Time) error  { return nil }
func (t *tunnel) SetWriteDeadline(_ time.Time) error { return nil }

type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "kubernetes-port-forward" }
func (tunnelAddr) String() string  { return "kubernetes-port-forward" }

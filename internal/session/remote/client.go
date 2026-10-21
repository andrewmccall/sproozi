package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// Client is home's handle on a recorded executor: a session placement, a
// terminal and the remote work transport. It contacts the cluster only when
// used, so home starts while the cluster is unreachable. After a failure that
// is not the executor's own answer, or a rejected token, it connects again on
// the next call, reloading the kubeconfig (and so a refreshed token) and the
// published CA, and checking the executor still describes itself as recorded.
type Client struct {
	executor Executor
	open     func(context.Context, Executor) (*profile.Client, error)

	mu       sync.Mutex
	current  *profile.Client
	profiles func(digest string) ([]byte, error)
}

// NewClient validates a recorded executor without contacting it.
func NewClient(e Executor) (*Client, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if e.Name == "" || e.Harness.Validate() != nil || e.MaxCheckpointBytes < 1 {
		return nil, errors.New("remote: a recorded executor needs its name, harness and checkpoint limit")
	}
	return &Client{executor: e, open: connect}, nil
}

// Executor is the recorded executor this client reaches.
func (c *Client) Executor() Executor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.executor
}

// Update points the client at a new way of reaching the same named executor,
// such as another context or kubeconfig file. The next call connects again.
func (c *Client) Update(e Executor) error {
	if err := e.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.Name != c.executor.Name {
		return fmt.Errorf("remote: executor %q cannot become %q", c.executor.Name, e.Name)
	}
	c.executor = e
	if c.current != nil {
		c.current.Close()
		c.current = nil
	}
	return nil
}

// connect opens a profile client and checks it reaches the recorded executor.
func connect(ctx context.Context, e Executor) (*profile.Client, error) {
	cluster, err := openCluster(e.Selection)
	if err != nil {
		return nil, err
	}
	connection, err := cluster.connection(ctx, e.MaxCheckpointBytes)
	if err != nil {
		return nil, err
	}
	client, err := profile.Connect(connection)
	if err != nil {
		return nil, err
	}
	d, err := client.Describe(ctx)
	if err != nil {
		client.Close()
		return nil, cluster.describeProblem(err)
	}
	if d.Placement != e.Name || d.Harness != e.Harness || d.MaxCheckpointBytes != e.MaxCheckpointBytes {
		client.Close()
		return nil, fmt.Errorf("the remote executor reached by %s now describes itself as %q (%s, checkpoints up to %d bytes), "+
			"but home recorded %q (%s, %d bytes); it is not the executor this session used. Restore it, or select "+
			"the right context and namespace", e.Selection, d.Placement, d.Harness, d.MaxCheckpointBytes,
			e.Name, e.Harness, e.MaxCheckpointBytes)
	}
	return client, nil
}

func (c *Client) client(ctx context.Context) (*profile.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		return c.current, nil
	}
	client, err := c.open(ctx, c.executor)
	if err != nil {
		return nil, fmt.Errorf("remote executor %q: %w", c.executor.Name, err)
	}
	client.SetProfileSource(c.profiles)
	c.current = client
	return client, nil
}

// settle keeps the connection after the executor's own answer and drops it
// otherwise, so the next call connects again.
func (c *Client) settle(used *profile.Client, err error) error {
	var answer *profile.RemoteError
	if err == nil || errors.As(err, &answer) && answer.Status != 0 && answer.Status != http.StatusUnauthorized {
		return err
	}
	c.mu.Lock()
	if c.current == used {
		c.current = nil
		used.Close()
	}
	c.mu.Unlock()
	return err
}

// SetProfileSource supplies retained profile archives for launches that name one.
func (c *Client) SetProfileSource(source func(digest string) ([]byte, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.profiles = source
	if c.current != nil {
		c.current.SetProfileSource(source)
	}
}

// Close releases idle connections; it does not revoke an allocation.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		c.current.Close()
		c.current = nil
	}
}

func (c *Client) Prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	p, err := c.client(ctx)
	if err != nil {
		return session.Incarnation{}, err
	}
	i, err := p.Prepare(ctx, l, data)
	return i, c.settle(p, err)
}

func (c *Client) Activate(ctx context.Context, l session.Launch, i session.Incarnation) error {
	p, err := c.client(ctx)
	if err != nil {
		return err
	}
	return c.settle(p, p.Activate(ctx, l, i))
}

func (c *Client) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	p, err := c.client(ctx)
	if err != nil {
		return session.Stopped{}, err
	}
	s, err := p.Stop(ctx, l, i)
	if errors.Is(err, session.ErrUsageUnsealed) {
		return s, err // A fenced stop with unsealed usage is the executor's answer.
	}
	return s, c.settle(p, err)
}

func (c *Client) Inspect(ctx context.Context, l session.Launch) (session.Observation, error) {
	p, err := c.client(ctx)
	if err != nil {
		return session.Observation{}, err
	}
	o, err := p.Inspect(ctx, l)
	return o, c.settle(p, err)
}

func (c *Client) Abort(ctx context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	p, err := c.client(ctx)
	if err != nil {
		return session.Observation{}, err
	}
	o, err := p.Abort(ctx, l, i)
	return o, c.settle(p, err)
}

func (c *Client) Attach(ctx context.Context, l session.Launch, i session.Incarnation, input io.Reader, output io.Writer) error {
	p, err := c.client(ctx)
	if err != nil {
		return err
	}
	return c.settle(p, p.Attach(ctx, l, i, input, output))
}

// CallOwner carries one remote work operation; see work.ClientFor. A failure to
// connect reads as the work owner being unavailable, with the cause attached
// for the local CLI.
func (c *Client) CallOwner(ctx context.Context, action string, input, output any) error {
	p, err := c.client(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", profile.OwnerOutcomeMessage(profile.CodeUnavailable),
			&profile.RemoteError{Code: profile.CodeUnavailable, Transport: err})
	}
	return c.settle(p, p.CallOwner(ctx, action, input, output))
}

var (
	_ session.Placement         = (*Client)(nil)
	_ session.TerminalPlacement = (*Client)(nil)
)

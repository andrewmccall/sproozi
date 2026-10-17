// Command checkpoint is a fixed-purpose trusted placement helper. It transfers
// continuity bytes only; the placement service owns process fencing, admission,
// transport authentication, quota-backed staging and activation.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
)

type selections []string

func (s *selections) String() string { return fmt.Sprint([]string(*s)) }
func (s *selections) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Checkpoint operation failed:", err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("capture-stopped or restore is required")
	}
	if len(args) == 1 && args[0] == "finish" {
		return finish()
	}
	if len(args) == 3 && args[0] == "hold" && args[1] == "--deadline" {
		deadline, err := time.Parse(time.RFC3339Nano, args[2])
		if err != nil {
			return err
		}
		return hold(deadline)
	}
	c, err := parseCommand(args[0], args[1:])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	switch args[0] {
	case "prepare":
		return c.prepare(ctx, input, output)
	case "prepare-workspace":
		return c.prepareWorkspace(ctx, input, output)
	case "capture-stopped":
		return c.capture(ctx, output)
	case "restore":
		return c.restore(ctx, input)
	default:
		return fmt.Errorf("unsupported checkpoint operation")
	}
}

type command struct {
	kind                                           harness.NativeKind
	home, repository, thread, instructions, target string
	limit                                          int64
	timeout                                        time.Duration
	ignored                                        selections
}

func parseCommand(operation string, args []string) (command, error) {
	flags := flag.NewFlagSet(operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	kind := flags.String("harness", "", "explicit native harness")
	home := flags.String("home", "", "source Codex home after writer fencing")
	repository := flags.String("workspace", "", "source Git workspace after writer fencing")
	thread := flags.String("thread", "", "exact native UUID")
	instructions := flags.String("instruction-digest", "", "trusted approved instruction digest for managed admission")
	target := flags.String("destination", "", "absent inert destination directory")
	limit := flags.Int64("max-bytes", 64<<20, "maximum encoded checkpoint bytes")
	timeout := flags.Duration("timeout", time.Minute, "maximum checkpoint operation duration")
	var ignored selections
	flags.Var(&ignored, "include-ignored", "individual ignored file explicitly selected for transfer; repeatable")
	if err := flags.Parse(args); err != nil {
		return command{}, err
	}
	if flags.NArg() != 0 || *limit <= 0 || *limit > 1<<30 || *timeout <= 0 || *timeout > 10*time.Minute {
		return command{}, fmt.Errorf("invalid checkpoint limits or arguments")
	}
	return command{harness.NativeKind(*kind), *home, *repository, *thread, *instructions,
		*target, *limit, *timeout, ignored}, nil
}

func (c command) fixedDestination() bool {
	return c.target == "" && c.home == "" && c.repository == "" && c.thread == "" && len(c.ignored) == 0
}
func (c command) read(input io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(input, c.limit+1))
}
func (c command) prepare(ctx context.Context, input io.Reader, output io.Writer) error {
	if !c.fixedDestination() || c.instructions == "" || c.kind.Validate() != nil {
		return fmt.Errorf("prepare requires only limits and independently trusted instruction digest")
	}
	data, err := c.read(input)
	if err != nil {
		return err
	}
	if err = checkpoint.ValidateContinuity(data, c.kind, c.instructions, c.limit); err != nil {
		return err
	}
	return prepare(ctx, data, c.limit, output)
}
func (c command) prepareWorkspace(ctx context.Context, input io.Reader, output io.Writer) error {
	if !c.fixedDestination() || c.instructions != "" || c.kind.Validate() != nil {
		return fmt.Errorf("prepare-workspace requires only fixed destination and limits")
	}
	data, err := c.read(input)
	if err != nil {
		return err
	}
	return prepareWorkspace(ctx, c.kind, data, c.limit, output)
}
func (c command) capture(ctx context.Context, output io.Writer) error {
	if c.home == "" || c.repository == "" || c.thread == "" || c.target != "" || c.kind.Validate() != nil {
		return fmt.Errorf("capture requires source home, workspace and thread")
	}
	data, _, err := checkpoint.CaptureStopped(ctx, c.kind, c.home, c.repository, c.thread, c.ignored, c.limit)
	if err != nil {
		return err
	}
	if c.instructions != "" {
		if err = checkpoint.ValidateContinuity(data, c.kind, c.instructions, c.limit); err != nil {
			return err
		}
	}
	_, err = io.Copy(output, bytes.NewReader(data))
	return err
}
func (c command) restore(ctx context.Context, input io.Reader) error {
	if c.target == "" || c.home != "" || c.repository != "" || c.thread != "" ||
		len(c.ignored) != 0 || c.instructions != "" || c.kind != "" {
		return fmt.Errorf("restore requires only an absent destination and limits")
	}
	data, err := c.read(input)
	if err != nil {
		return err
	}
	_, err = checkpoint.Restore(ctx, data, c.target, c.limit)
	return err
}

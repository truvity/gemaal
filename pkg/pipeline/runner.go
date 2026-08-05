package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Command describes one external invocation: argv, working directory,
// environment additions (KEY=VALUE, appended to the inherited
// environment) and optional stdin.
type Command struct {
	Argv  []string
	Dir   string
	Env   []string
	Stdin io.Reader
}

// Runner executes external commands. The flows depend on this interface
// only — tests inject a scripted stub, the CLI injects ExecRunner.
type Runner interface {
	// Run executes the command, streaming its output through.
	Run(ctx context.Context, c Command) error
	// Output executes the command and captures its stdout (stderr still
	// streams through).
	Output(ctx context.Context, c Command) (string, error)
}

// ExecRunner runs commands via os/exec, inheriting the process
// environment plus the command's additions.
type ExecRunner struct{}

func (ExecRunner) command(ctx context.Context, c Command) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin = c.Stdin
	cmd.Stderr = os.Stderr

	return cmd
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, c Command) error {
	cmd := r.command(ctx, c)
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(c.Argv, " "), err)
	}

	return nil
}

// Output implements Runner.
func (r ExecRunner) Output(ctx context.Context, c Command) (string, error) {
	var buf bytes.Buffer

	cmd := r.command(ctx, c)
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("%s: %w", strings.Join(c.Argv, " "), err)
	}

	return buf.String(), nil
}

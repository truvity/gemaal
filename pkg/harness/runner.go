package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes external commands (helm, kubectl). The cluster helpers
// depend on this seam only; tests inject scripted stubs and assert argv.
type Runner interface {
	// Run executes the command, streaming its output through.
	Run(ctx context.Context, argv ...string) error

	// Output executes the command and captures its stdout (stderr still
	// streams through).
	Output(ctx context.Context, argv ...string) (string, error)
}

// ExecRunner is the real Runner: os/exec with stderr and stdin attached,
// so interactive credential flows and helm/kubectl progress reach the
// terminal.
type ExecRunner struct{}

func (ExecRunner) command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, argv ...string) error {
	cmd := r.command(ctx, argv)
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}

	return nil
}

// Output implements Runner.
func (r ExecRunner) Output(ctx context.Context, argv ...string) (string, error) {
	var stdout bytes.Buffer

	cmd := r.command(ctx, argv)
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}

	return stdout.String(), nil
}

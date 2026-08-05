package identity

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes one external command and captures its stdout. The
// drivers depend on this seam only; tests inject scripted stubs.
type Runner interface {
	Output(ctx context.Context, argv ...string) (string, error)
}

// ExecRunner is the real Runner: os/exec with stderr and stdin passed
// through, so interactive credential flows (a first-time browser SSO
// login under kubectl) reach the terminal — only stdout is parsed.
type ExecRunner struct{}

// Output implements Runner.
func (ExecRunner) Output(ctx context.Context, argv ...string) (string, error) {
	var stdout bytes.Buffer

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}

	return stdout.String(), nil
}

// runner returns the configured Runner or the real one.
func runner(r Runner) Runner {
	if r != nil {
		return r
	}

	return ExecRunner{}
}

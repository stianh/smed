package run

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// capturingRunner records the exact command it was asked to run, so
// Become's command-construction logic can be checked without touching a
// shell at all.
type capturingRunner struct {
	gotCommand string
	output     string
	err        error
}

func (c *capturingRunner) Run(ctx context.Context, command string) (string, error) {
	c.gotCommand = command
	return c.output, c.err
}

func TestBecomeDefaultsToRoot(t *testing.T) {
	inner := &capturingRunner{}
	b := &Become{Inner: inner}

	if _, err := b.Run(context.Background(), "whoami"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.HasPrefix(inner.gotCommand, "sudo -n sh -c ") {
		t.Fatalf("got command %q, want it to start with %q", inner.gotCommand, "sudo -n sh -c ")
	}
	if strings.Contains(inner.gotCommand, "-u ") {
		t.Fatalf("got command %q, want no -u flag when targeting root (the default)", inner.gotCommand)
	}
	if !strings.Contains(inner.gotCommand, "'whoami'") {
		t.Fatalf("got command %q, want the original command quoted inside it", inner.gotCommand)
	}
}

func TestBecomeTargetsNonRootUser(t *testing.T) {
	inner := &capturingRunner{}
	b := &Become{Inner: inner, TargetUser: "postgres"}

	if _, err := b.Run(context.Background(), "id"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(inner.gotCommand, "-u 'postgres'") {
		t.Fatalf("got command %q, want -u 'postgres'", inner.gotCommand)
	}
}

func TestBecomeQuotesCommandSafely(t *testing.T) {
	inner := &capturingRunner{}
	b := &Become{Inner: inner}

	// A command containing a single quote must survive intact — this is
	// exactly the kind of thing shellquote.Quote exists to get right.
	if _, err := b.Run(context.Background(), `echo "it's a test"`); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(inner.gotCommand, `echo "it`) {
		t.Fatalf("got command %q, original command text missing or mangled", inner.gotCommand)
	}
}

func TestBecomeDetectsMissingNOPASSWD(t *testing.T) {
	inner := &capturingRunner{
		output: "sudo: a password is required\n",
		err:    &exec.ExitError{},
	}
	b := &Become{Inner: inner}

	_, err := b.Run(context.Background(), "whoami")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "NOPASSWD not configured") {
		t.Fatalf("error = %q, want it to call out missing NOPASSWD specifically", err.Error())
	}
}

// localSudoRunner runs commands against the real local shell, exactly
// like file package's localRunner.
type localSudoRunner struct{}

func (localSudoRunner) Run(ctx context.Context, cmd string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	return string(out), err
}

func TestBecomeAgainstRealSudoWithoutNOPASSWD(t *testing.T) {
	// This machine's own sudo requires a password for this user — a
	// real, not simulated, "NOPASSWD not configured" case. If that ever
	// stops being true in whatever environment runs this test, the
	// skip below keeps it honest rather than silently asserting nothing.
	check := exec.Command("sudo", "-n", "true")
	if err := check.Run(); err == nil {
		t.Skip("this environment has passwordless sudo configured; nothing to detect")
	}

	b := &Become{Inner: localSudoRunner{}}
	_, err := b.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("expected an error — this environment's sudo requires a password")
	}
	if !strings.Contains(err.Error(), "NOPASSWD not configured") {
		t.Fatalf("error = %q, want it to call out missing NOPASSWD specifically", err.Error())
	}
}

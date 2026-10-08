package command

import (
	"context"
	"errors"
	"testing"

	"github.com/stianh/smed/module"
)

type capturingRunner struct {
	gotCommand string
	out        string
	err        error
}

func (c *capturingRunner) Run(ctx context.Context, cmd string) (string, error) {
	c.gotCommand = cmd
	return c.out, c.err
}

func TestDiffAlwaysReportsUpdate(t *testing.T) {
	r := Run{Cmd: "echo hi"}
	change, err := r.Diff(r.Desired(), nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update — command has no concept of already-done", change.Action)
	}
}

func TestRunModuleAlwaysAppliesEvenOnRepeatedCalls(t *testing.T) {
	// Proves the whole point: calling RunModule (the normal
	// Read-Diff-Apply-Verify sequence) twice in a row runs the command
	// twice, unlike every other module in this repo.
	r := &capturingRunner{}
	c := Run{Cmd: "systemctl restart nginx"}

	first, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("first RunModule: %v", err)
	}
	if !first.Changed || r.gotCommand != "systemctl restart nginx" {
		t.Fatalf("first run: got Changed=%v, command=%q", first.Changed, r.gotCommand)
	}

	r.gotCommand = ""
	second, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("second RunModule: %v", err)
	}
	if !second.Changed || r.gotCommand != "systemctl restart nginx" {
		t.Fatalf("second run: want it to run again (Changed=true, command re-issued), got Changed=%v, command=%q", second.Changed, r.gotCommand)
	}
}

func TestApplyPropagatesCommandFailure(t *testing.T) {
	r := &capturingRunner{out: "unit not found", err: errors.New("exit status 5")}
	c := Run{Cmd: "systemctl restart ghost"}

	_, err := module.RunModule(context.Background(), c, r)
	if err == nil {
		t.Fatal("expected an error when the underlying command fails")
	}
}

func TestVerifyIsAlwaysNoOp(t *testing.T) {
	c := Run{Cmd: "echo hi"}
	if err := c.Verify(context.Background(), &capturingRunner{}, c.Desired()); err != nil {
		t.Fatalf("Verify: %v, want nil — nothing to re-check", err)
	}
}

func TestUnlessSkipsApplyWhenAlreadySatisfied(t *testing.T) {
	r := &capturingRunner{}
	c := Run{
		Cmd:    "tar -xzf release.tar.gz -C /opt/app",
		Unless: func(ctx context.Context, run module.Runner) (bool, error) { return true, nil },
	}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if result.Changed {
		t.Fatal("expected Changed=false — Unless reported already satisfied")
	}
	if r.gotCommand != "" {
		t.Fatalf("expected Cmd to never run, but got %q", r.gotCommand)
	}
}

func TestUnlessRunsApplyWhenNotSatisfied(t *testing.T) {
	r := &capturingRunner{}
	c := Run{
		Cmd:    "tar -xzf release.tar.gz -C /opt/app",
		Unless: func(ctx context.Context, run module.Runner) (bool, error) { return false, nil },
	}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed || r.gotCommand != "tar -xzf release.tar.gz -C /opt/app" {
		t.Fatalf("expected Cmd to run, got Changed=%v, command=%q", result.Changed, r.gotCommand)
	}
}

func TestUnlessPropagatesItsOwnError(t *testing.T) {
	c := Run{
		Cmd:    "tar -xzf release.tar.gz -C /opt/app",
		Unless: func(ctx context.Context, run module.Runner) (bool, error) { return false, errors.New("boom") },
	}

	_, err := module.RunModule(context.Background(), c, &capturingRunner{})
	if err == nil {
		t.Fatal("expected an error when Unless itself fails")
	}
}

func TestNilUnlessAlwaysRuns(t *testing.T) {
	// Same as TestRunModuleAlwaysAppliesEvenOnRepeatedCalls, but
	// explicit about the zero-value case: Unless unset must behave
	// identically to before this field existed.
	r := &capturingRunner{}
	c := Run{Cmd: "echo hi"}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — nil Unless means always run")
	}
}

func TestPathExistsReturnsTrueWhenPathIsPresent(t *testing.T) {
	r := &capturingRunner{out: "yes\n"}
	satisfied, err := PathExists("/opt/app/release")(context.Background(), r)
	if err != nil {
		t.Fatalf("PathExists check: %v", err)
	}
	if !satisfied {
		t.Fatal("expected satisfied=true when the probe prints yes")
	}
	if r.gotCommand != `test -e '/opt/app/release' && echo yes || echo no` {
		t.Fatalf("got command %q", r.gotCommand)
	}
}

func TestPathExistsReturnsFalseWhenPathIsMissing(t *testing.T) {
	r := &capturingRunner{out: "no\n"}
	satisfied, err := PathExists("/opt/app/release")(context.Background(), r)
	if err != nil {
		t.Fatalf("PathExists check: %v", err)
	}
	if satisfied {
		t.Fatal("expected satisfied=false when the probe prints no")
	}
}

func TestPathExistsAsUnlessIntegratesWithRunModule(t *testing.T) {
	r := &scriptedCommandRunner{responses: []string{"no\n", ""}} // test -e -> missing, then the real command
	c := Run{Cmd: "tar -xzf release.tar.gz -C /opt/app/release", Unless: PathExists("/opt/app/release")}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — the path doesn't exist yet")
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 commands (the check, then the real one), got %d: %v", len(r.calls), r.calls)
	}
}

type scriptedCommandRunner struct {
	responses []string
	calls     []string
}

func (s *scriptedCommandRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.calls = append(s.calls, cmd)
	if len(s.calls) > len(s.responses) {
		return "", errors.New("scriptedCommandRunner: ran out of scripted responses")
	}
	return s.responses[len(s.calls)-1], nil
}

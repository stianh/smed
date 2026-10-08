package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stianh/smed/module"
)

// scriptedRunner plays back one response per call and records every
// command it was asked to run — same pattern as module/apt's tests.
type scriptedRunner struct {
	responses []scriptedResponse
	calls     []string
}

type scriptedResponse struct {
	out string
	err error
}

func (s *scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.calls = append(s.calls, cmd)
	if len(s.calls) > len(s.responses) {
		return "", errors.New("scriptedRunner: ran out of scripted responses")
	}
	r := s.responses[len(s.calls)-1]
	return r.out, r.err
}

func TestReadParsesActiveAndEnabled(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "active\n" + sep + "\nenabled\n"},
	}}
	got, err := Unit{Name: "nginx"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := state{Active: true, Enabled: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadParsesInactiveAndDisabled(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "inactive\n" + sep + "\ndisabled\n"},
	}}
	got, err := Unit{Name: "nginx"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := state{Active: false, Enabled: false}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadTreatsMissingUnitAsNotActiveNotEnabled(t *testing.T) {
	// A unit that doesn't exist at all: is-active prints "inactive",
	// is-enabled prints an error about a missing unit file to stderr
	// (captured here via 2>&1). Neither should error Read — both count
	// as false, same as any other unrecognized text would.
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "inactive\n" + sep + "\nFailed to get unit file state for ghost.service: No such file or directory\n"},
	}}
	got, err := Unit{Name: "ghost"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := state{Active: false, Enabled: false}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadTreatsFailedStateAsNotActive(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "failed\n" + sep + "\nenabled\n"},
	}}
	got, err := Unit{Name: "nginx"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := state{Active: false, Enabled: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDiffNoOpWhenMatching(t *testing.T) {
	u := Unit{Name: "nginx", Active: true, Enabled: true}
	change, err := u.Diff(u.Desired(), state{Active: true, Enabled: true})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestDiffUpdateWhenActiveDiffers(t *testing.T) {
	u := Unit{Name: "nginx", Active: true, Enabled: true}
	change, err := u.Diff(u.Desired(), state{Active: false, Enabled: true})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update", change.Action)
	}
}

func TestDiffUpdateWhenEnabledDiffers(t *testing.T) {
	u := Unit{Name: "nginx", Active: true, Enabled: true}
	change, err := u.Diff(u.Desired(), state{Active: true, Enabled: false})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update", change.Action)
	}
}

func TestApplyEnablesAndStarts(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	u := Unit{Name: "nginx", Active: true, Enabled: true}
	result, err := u.Apply(context.Background(), r, module.Change{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected exactly 1 command, got %d: %v", len(r.calls), r.calls)
	}
	got := r.calls[0]
	if !strings.Contains(got, "systemctl enable 'nginx'") || !strings.Contains(got, "systemctl start 'nginx'") {
		t.Fatalf("got command %q, want both enable and start", got)
	}
}

func TestApplyDisablesAndStops(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	u := Unit{Name: "nginx", Active: false, Enabled: false}
	if _, err := u.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	if !strings.Contains(got, "systemctl disable 'nginx'") || !strings.Contains(got, "systemctl stop 'nginx'") {
		t.Fatalf("got command %q, want both disable and stop", got)
	}
}

func TestApplyHandlesMixedActiveAndEnabled(t *testing.T) {
	// Running right now, but not enabled at boot — the independent
	// combination, to make sure Apply doesn't assume they always move
	// together.
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	u := Unit{Name: "oneshot-task", Active: true, Enabled: false}
	if _, err := u.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	if !strings.Contains(got, "systemctl disable 'oneshot-task'") || !strings.Contains(got, "systemctl start 'oneshot-task'") {
		t.Fatalf("got command %q, want disable (not enabled) + start (active)", got)
	}
}

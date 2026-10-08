package apt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stianh/smed/module"
)

func init() {
	// Real runs want a few seconds of backoff between lock retries;
	// these tests are checking the retry *logic*, not the wall clock,
	// so there's no reason to actually wait.
	lockRetryDelay = time.Millisecond
}

// scriptedRunner plays back a fixed sequence of responses, one per call,
// and records every command it was asked to run — enough to simulate
// "fails with lock contention twice then succeeds" or "stale cache, one
// update, then succeeds" without touching a real apt at all.
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

func TestReadParsesInstalledPackage(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "install ok installed\t1.24.0-2\n"},
	}}
	got, err := Installed{Name: "nginx"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := state{Installed: true, Version: "1.24.0-2"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadTreatsUnknownPackageAsNotInstalled(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "dpkg-query: no packages found matching ghost-package\n", err: errors.New("exit status 1")},
	}}
	got, err := Installed{Name: "ghost-package"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(state).Installed {
		t.Fatal("expected Installed=false for an unknown package")
	}
}

func TestReadTreatsRemovedConfigFilesStateAsNotInstalled(t *testing.T) {
	// A package that was removed but left config behind: known to
	// dpkg, but not actually present — must count as "not installed"
	// so Diff says Create, not NoOp.
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "deinstall ok config-files\t1.2.3\n"},
	}}
	got, err := Installed{Name: "old-package"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(state).Installed {
		t.Fatal("expected Installed=false for deinstall ok config-files")
	}
}

func TestDiffCreateWhenMissing(t *testing.T) {
	i := Installed{Name: "nginx"}
	change, err := i.Diff(i.Desired(), state{Installed: false})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Create {
		t.Fatalf("Action = %v, want Create", change.Action)
	}
}

func TestDiffNoOpWhenVersionUnspecified(t *testing.T) {
	i := Installed{Name: "nginx"} // no Version pinned — any installed version is fine
	change, err := i.Diff(i.Desired(), state{Installed: true, Version: "1.18.0-1"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestDiffUpdateWhenVersionPinnedAndDiffers(t *testing.T) {
	i := Installed{Name: "nginx", Version: "1.24.0-2"}
	change, err := i.Diff(i.Desired(), state{Installed: true, Version: "1.18.0-1"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update", change.Action)
	}
}

func TestApplySucceedsOnFirstTry(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "Setting up nginx ...\n"},
	}}
	result, err := Installed{Name: "nginx"}.Apply(context.Background(), r, module.Change{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected exactly 1 command, got %d: %v", len(r.calls), r.calls)
	}
}

func TestApplyRecoversFromStaleCacheWithOneUpdate(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "E: Unable to locate package nginx\n", err: errors.New("exit status 100")},
		{out: "Reading package lists... Done\n"}, // apt-get update
		{out: "Setting up nginx ...\n"},          // retried install
	}}
	result, err := Installed{Name: "nginx"}.Apply(context.Background(), r, module.Change{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true")
	}
	if len(r.calls) != 3 {
		t.Fatalf("expected exactly 3 commands (install, update, install), got %d: %v", len(r.calls), r.calls)
	}
	if r.calls[1] != "apt-get update 2>&1" {
		t.Fatalf("second command = %q, want apt-get update", r.calls[1])
	}
}

func TestApplyOnlyUpdatesOnceEvenIfStillNotFoundAfter(t *testing.T) {
	// If the package genuinely doesn't exist anywhere, updating once
	// and still failing is a real, terminal error — not a reason to
	// update over and over.
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "E: Unable to locate package ghost\n", err: errors.New("exit status 100")},
		{out: "Reading package lists... Done\n"},
		{out: "E: Unable to locate package ghost\n", err: errors.New("exit status 100")},
	}}
	_, err := Installed{Name: "ghost"}.Apply(context.Background(), r, module.Change{})
	if err == nil {
		t.Fatal("expected an error — package still not found after one update")
	}
	if len(r.calls) != 3 {
		t.Fatalf("expected exactly 3 commands (no further updates/retries), got %d: %v", len(r.calls), r.calls)
	}
}

func TestApplyRetriesOnDpkgLockThenSucceeds(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "E: Could not get lock /var/lib/dpkg/lock-frontend\n", err: errors.New("exit status 100")},
		{out: "E: Could not get lock /var/lib/dpkg/lock-frontend\n", err: errors.New("exit status 100")},
		{out: "Setting up nginx ...\n"},
	}}
	result, err := Installed{Name: "nginx"}.Apply(context.Background(), r, module.Change{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true")
	}
	if len(r.calls) != 3 {
		t.Fatalf("expected exactly 3 install attempts, got %d: %v", len(r.calls), r.calls)
	}
	for _, c := range r.calls {
		if c != r.calls[0] {
			t.Fatalf("expected every retry to be the identical install command, got %q and %q", r.calls[0], c)
		}
	}
}

func TestApplyGivesUpAfterTooManyLockRetries(t *testing.T) {
	responses := make([]scriptedResponse, 0, maxApplyAttempts)
	for i := 0; i < maxApplyAttempts; i++ {
		responses = append(responses, scriptedResponse{
			out: "E: Could not get lock /var/lib/dpkg/lock-frontend\n",
			err: errors.New("exit status 100"),
		})
	}
	r := &scriptedRunner{responses: responses}

	_, err := Installed{Name: "nginx"}.Apply(context.Background(), r, module.Change{})
	if err == nil {
		t.Fatal("expected an error after exhausting lock retries")
	}
	if len(r.calls) != maxApplyAttempts {
		t.Fatalf("expected exactly %d attempts, got %d", maxApplyAttempts, len(r.calls))
	}
}

func TestApplyReturnsRealFailuresImmediately(t *testing.T) {
	// An unrelated real failure (e.g. a broken postinst script) must
	// not be retried at all — retrying is only for the two specific,
	// recognized transient conditions.
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "dpkg: error processing package nginx (--configure):\n installed nginx package post-installation script subprocess returned error exit status 1\n", err: errors.New("exit status 1")},
	}}
	_, err := Installed{Name: "nginx"}.Apply(context.Background(), r, module.Change{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry for an unrecognized failure), got %d: %v", len(r.calls), r.calls)
	}
}

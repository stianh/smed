// Package command implements module.Module for running an arbitrary
// shell command: smed's version of Ansible's command/shell modules.
// There's no meaningful Read/Diff for "has this command already run" —
// a command isn't a resource with state to converge toward — so by
// default this module is non-idempotent: Apply runs every single time
// it's reached, same as Ansible's own command module.
//
// The main reason this exists at all rather than staying hand-rolled
// inline (every demo so far had done exactly that) is handlers: a
// restart action ("systemctl restart X") isn't a convergent resource
// state the way service.Unit{Active: true} is — starting an
// already-active unit is a no-op, not a restart — so a handler usually
// needs something action-shaped to wrap, and this is the generic one.
//
// Unless is the escape hatch for the other real case this module
// covers: a command that genuinely does have an "already done" state,
// but no module exists for it yet (installing a GPG keyring via a
// shell pipeline, joining a tailnet, unpacking a release tarball into
// a venv) — the same job Ansible's creates:/removes: do for an
// otherwise-unconditional command/shell task.
package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Run executes Cmd whenever it's reached in the graph — unconditionally
// if Unless is nil (the default, unchanged from before Unless existed),
// or only if Unless reports the condition isn't already satisfied.
type Run struct {
	Cmd string

	// Unless, checked once during Read, is this command's idempotency
	// gate: if it reports true (whatever it checks is already true on
	// the real target), Diff reports NoOp and Apply never runs. nil
	// means always run — correct for something like a restart, which
	// has no "already done" state to check at all. PathExists below
	// covers the single most common case (Ansible's creates: path) as
	// a one-line constructor.
	Unless func(ctx context.Context, r module.Runner) (bool, error)
}

// state carries Read's one finding — whether Unless was already
// satisfied — forward to Diff. Doing the actual check in Read, not
// Diff, keeps Diff a pure function of two States, same as every other
// module: Unless needs a Runner to do real I/O, and Read is the only
// method in this contract allowed to have one.
type state struct {
	satisfied bool
}

func (r Run) Desired() module.State { return nil }

// Read runs Unless, if set, and carries its result forward as state —
// the only thing this module's state exists for. No Unless means
// nothing to check, so Diff always reports Update, unchanged from
// before this field existed.
func (r Run) Read(ctx context.Context, run module.Runner) (module.State, error) {
	if r.Unless == nil {
		return state{}, nil
	}
	satisfied, err := r.Unless(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("check unless: %w", err)
	}
	return state{satisfied: satisfied}, nil
}

// Diff reports NoOp if Read found Unless already satisfied; otherwise
// Update, the same unconditional "run it" this module always reported
// before Unless existed.
func (r Run) Diff(desired, current module.State) (module.Change, error) {
	if cur, ok := current.(state); ok && cur.satisfied {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: r.Cmd}, nil
}

func (r Run) Apply(ctx context.Context, run module.Runner, change module.Change) (module.Result, error) {
	out, err := run.Run(ctx, r.Cmd)
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("run %q: %w", r.Cmd, err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

// Verify is a no-op: there's no desired state to re-check after the
// fact — running the command was the entire point, not a means to some
// other convergent state.
func (r Run) Verify(ctx context.Context, run module.Runner, desired module.State) error {
	return nil
}

// PathExists returns an Unless check satisfied once path exists on
// the target — the direct equivalent of Ansible's creates: path, and
// by far the most common shape an idempotency gate needs: "only run
// this if it hasn't already produced the thing it produces."
func PathExists(path string) func(context.Context, module.Runner) (bool, error) {
	return func(ctx context.Context, r module.Runner) (bool, error) {
		out, err := r.Run(ctx, fmt.Sprintf("test -e %s && echo yes || echo no", shellquote.Quote(path)))
		if err != nil {
			return false, fmt.Errorf("check path exists %s: %w", path, err)
		}
		return strings.TrimSpace(out) == "yes", nil
	}
}

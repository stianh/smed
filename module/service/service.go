// Package service implements module.Module for systemd unit state:
// smed's version of Ansible's service module. Minimal on purpose — it
// only manages whether an existing unit is active and/or enabled, not
// whether the unit file itself exists (that's file.Content's job if
// you're deploying the unit file too). "Restart nginx when its config
// changes" is handled a layer up, by HostSpec.Notify/Handler — see
// README.md, "A real dependency graph, not a flat task list" — not by
// anything in this module.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Unit ensures an existing systemd unit's Active (running right now) and
// Enabled (starts on boot) state match. Both are always managed — there's
// no "don't care" value, unlike file's optional Mode/Owner/Group — since
// wanting one managed independently of the other is the rare case, not
// the common one; that's a cheap follow-up if it's ever actually needed.
type Unit struct {
	Name    string
	Active  bool
	Enabled bool
}

type state struct {
	Active  bool
	Enabled bool
}

func (u Unit) Desired() module.State {
	return state{Active: u.Active, Enabled: u.Enabled}
}

// sep separates the two systemctl outputs in one round trip. Distinctive
// enough that it won't collide with anything systemctl or a unit's own
// name could print.
const sep = "---SMED-SEP---"

// Read combines is-active and is-enabled into one round trip, and
// forces the overall command to exit 0 (`; true`) so a unit that's
// inactive, disabled, or doesn't exist at all is a normal result to
// parse rather than a Read error — systemctl's own exit codes for those
// states aren't 0, but none of them are failures from Read's point of
// view. Only text that doesn't parse as a recognized state is an error.
func (u Unit) Read(ctx context.Context, r module.Runner) (module.State, error) {
	cmd := fmt.Sprintf(
		"systemctl is-active %s 2>&1; echo %s; systemctl is-enabled %s 2>&1; true",
		shellquote.Quote(u.Name), sep, shellquote.Quote(u.Name),
	)
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("read service %s: %w", u.Name, err)
	}

	parts := strings.SplitN(out, sep, 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("read service %s: unexpected output %q", u.Name, out)
	}
	activeOut := strings.TrimSpace(parts[0])
	enabledOut := strings.TrimSpace(parts[1])

	// Anything other than the exact "active"/"enabled" text — inactive,
	// failed, activating, a missing-unit error message, whatever else
	// a given systemd version prints — counts as false. There's no
	// state worth distinguishing further for what this module manages.
	return state{
		Active:  activeOut == "active",
		Enabled: enabledOut == "enabled",
	}, nil
}

func (u Unit) Diff(desired, current module.State) (module.Change, error) {
	d, ok := desired.(state)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not service.state", desired)
	}
	cur, ok := current.(state)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not service.state", current)
	}

	var diffs []string
	if cur.Active != d.Active {
		diffs = append(diffs, fmt.Sprintf("active %v -> %v", cur.Active, d.Active))
	}
	if cur.Enabled != d.Enabled {
		diffs = append(diffs, fmt.Sprintf("enabled %v -> %v", cur.Enabled, d.Enabled))
	}
	if len(diffs) == 0 {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: fmt.Sprintf("%s: %s", u.Name, strings.Join(diffs, ", "))}, nil
}

// Apply unconditionally enforces both Active and Enabled, not just
// whichever one Diff happened to notice first — the same reasoning as
// file.Content.Apply: fixing only the diffed attribute would leave a
// second, independently-differing one wrong, and Verify would (correctly)
// catch it afterward.
func (u Unit) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	enableVerb := "disable"
	if u.Enabled {
		enableVerb = "enable"
	}
	activeVerb := "stop"
	if u.Active {
		activeVerb = "start"
	}

	cmd := fmt.Sprintf(
		"systemctl %s %s 2>&1 && systemctl %s %s 2>&1",
		enableVerb, shellquote.Quote(u.Name), activeVerb, shellquote.Quote(u.Name),
	)
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply service %s: %w", u.Name, err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (u Unit) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, u, r, desired)
}

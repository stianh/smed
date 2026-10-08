package ufw

import (
	"context"
	"fmt"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Default manages ufw's enabled state and default incoming/outgoing
// policy.
//
// SAFETY: this module enforces no ordering protection against locking
// yourself out. Setting IncomingPolicy to "deny" while Enabled is true,
// without an allow Rule already in place for the port you're connected
// on, will cut off new connections on that port — including over
// Tailscale: traffic arriving on tailscale0 still passes through the
// normal netfilter INPUT chain, it is not exempted from ufw's default
// policy. Apply any Rule steps that must stay reachable *before* this
// one in your own chain, the same way the Ansible role this was built
// against installs ufw, adds its allow rules, and only then enables it.
// This module has no way to know what you're connected over and
// deliberately doesn't guess.
type Default struct {
	Enabled bool
	// IncomingPolicy and OutgoingPolicy are "allow"/"deny"/"reject";
	// "" means don't manage that direction's policy.
	IncomingPolicy string
	OutgoingPolicy string
}

type defaultState struct {
	Enabled        bool
	IncomingPolicy string
	OutgoingPolicy string
}

func (d Default) Desired() module.State {
	return defaultState{Enabled: d.Enabled, IncomingPolicy: d.IncomingPolicy, OutgoingPolicy: d.OutgoingPolicy}
}

func (d Default) Read(ctx context.Context, r module.Runner) (module.State, error) {
	out, err := r.Run(ctx, "ufw status verbose 2>&1")
	if err != nil {
		return nil, fmt.Errorf("read ufw default: %w", err)
	}

	var cur defaultState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Status:"):
			cur.Enabled = strings.TrimSpace(strings.TrimPrefix(line, "Status:")) == "active"
		case strings.HasPrefix(line, "Default:"):
			// "Default: deny (incoming), allow (outgoing), disabled (routed)"
			rest := strings.TrimSpace(strings.TrimPrefix(line, "Default:"))
			for _, seg := range strings.Split(rest, ",") {
				fields := strings.Fields(strings.TrimSpace(seg))
				if len(fields) != 2 {
					continue
				}
				policy, direction := fields[0], strings.Trim(fields[1], "()")
				switch direction {
				case "incoming":
					cur.IncomingPolicy = policy
				case "outgoing":
					cur.OutgoingPolicy = policy
				}
			}
		}
	}
	return cur, nil
}

func (d Default) Diff(desired, current module.State) (module.Change, error) {
	ds, ok := desired.(defaultState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not ufw.defaultState", desired)
	}
	cur, ok := current.(defaultState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not ufw.defaultState", current)
	}

	var diffs []string
	if ds.Enabled != cur.Enabled {
		diffs = append(diffs, fmt.Sprintf("enabled %v -> %v", cur.Enabled, ds.Enabled))
	}
	if ds.IncomingPolicy != "" && cur.IncomingPolicy != ds.IncomingPolicy {
		diffs = append(diffs, fmt.Sprintf("incoming policy %s -> %s", cur.IncomingPolicy, ds.IncomingPolicy))
	}
	if ds.OutgoingPolicy != "" && cur.OutgoingPolicy != ds.OutgoingPolicy {
		diffs = append(diffs, fmt.Sprintf("outgoing policy %s -> %s", cur.OutgoingPolicy, ds.OutgoingPolicy))
	}
	if len(diffs) == 0 {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: strings.Join(diffs, ", ")}, nil
}

// Apply sets policy before touching the enabled state — minimizing the
// window of being in a bad state, though see the SAFETY note on
// Default: this orders its own two actions safely, it cannot order
// itself relative to Rule steps elsewhere in your chain.
func (d Default) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	var cmds []string
	if d.IncomingPolicy != "" {
		cmds = append(cmds, fmt.Sprintf("ufw default %s incoming", shellquote.Quote(d.IncomingPolicy)))
	}
	if d.OutgoingPolicy != "" {
		cmds = append(cmds, fmt.Sprintf("ufw default %s outgoing", shellquote.Quote(d.OutgoingPolicy)))
	}
	if d.Enabled {
		cmds = append(cmds, "ufw --force enable")
	} else {
		cmds = append(cmds, "ufw --force disable")
	}

	out, err := r.Run(ctx, strings.Join(cmds, " && ")+" 2>&1")
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply ufw default: %w", err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (d Default) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, d, r, desired)
}

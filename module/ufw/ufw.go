// Package ufw implements module.Module for Ubuntu's firewall frontend:
// smed's version of community.general.ufw. Two separate types, matching
// the project's taste for small focused modules over one that branches
// on "what kind of thing is this": Rule (an allow/deny rule — only ever
// added, never removed, same no-Absent pattern as file/apt/service/user)
// and Default (enabled state + default incoming/outgoing policy).
//
// ufw has no machine-readable status output, so Read parses
// `ufw status` / `ufw status verbose` text directly — a pragmatic
// heuristic grounded in real captured output (see ufw_test.go), not a
// formal parser. Known limitations, stated rather than hidden: outbound
// rules aren't matched (the role this was built against only ever adds
// inbound allow rules), and an app-profile rule (e.g. "OpenSSH") is
// matched by its name appearing in the line, not by independently
// verifying the port it resolves to.
//
// Read the safety note on Default before using it — getting the
// enable/policy ordering wrong on a real host can lock out SSH access
// entirely, including over Tailscale (traffic still passes through the
// normal netfilter INPUT chain on tailscale0; it isn't exempted).
package ufw

import (
	"context"
	"fmt"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Rule is a single allow/deny rule, either a port+protocol spec or a
// named application profile (ufw app list) — App and Port/Proto are
// mutually exclusive; set one or the other.
type Rule struct {
	Allow bool   // true = allow, false = deny
	Port  string // e.g. "5432"; ignored if App is set
	Proto string // e.g. "tcp"; "" means unspecified (matches any proto for this port)
	From  string // source CIDR/IP; "" means "Anywhere" (ufw's own term for any source)
	App   string // a named profile from `ufw app list`, e.g. "OpenSSH"
}

type ruleState struct {
	Exists bool
}

func (r Rule) Desired() module.State { return ruleState{Exists: true} }

// Read checks whether a matching rule is already active. Never mutates
// — `ufw status` is purely informational.
func (r Rule) Read(ctx context.Context, run module.Runner) (module.State, error) {
	out, err := run.Run(ctx, "ufw status 2>&1")
	if err != nil {
		return nil, fmt.Errorf("read ufw rule: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if r.matches(line) {
			return ruleState{Exists: true}, nil
		}
	}
	return ruleState{Exists: false}, nil
}

// matches is the pragmatic heuristic described in the package doc:
// the line must carry this rule's action, and either contain
// "(AppName)" or start with the port/proto spec, and contain the
// expected source text somewhere.
//
// Caught against a real host: ufw prints "ALLOW IN" for a rule added
// as a plain port/proto spec, but just "ALLOW" (no direction) for one
// added via the "from X to any port Y" form — this module's own Apply
// uses the latter whenever From is set, so matching on "ALLOW IN"
// literally would never recognize the rules it had just created. This
// module never produces an explicit OUT rule, so dropping the
// direction suffix entirely costs nothing and matches both real forms.
func (r Rule) matches(line string) bool {
	action := "DENY"
	if r.Allow {
		action = "ALLOW"
	}
	if !strings.Contains(line, action) {
		return false
	}

	if r.App != "" {
		return strings.Contains(line, "("+r.App+")")
	}

	spec := r.Port
	if r.Proto != "" {
		spec += "/" + r.Proto
	}
	if !strings.HasPrefix(strings.TrimSpace(line), spec) {
		return false
	}

	from := r.From
	if from == "" {
		from = "Anywhere"
	}
	return strings.Contains(line, from)
}

func (r Rule) Diff(desired, current module.State) (module.Change, error) {
	cur, ok := current.(ruleState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not ufw.ruleState", current)
	}
	if cur.Exists {
		return module.Change{Action: module.NoOp}, nil
	}
	verb := "deny"
	if r.Allow {
		verb = "allow"
	}
	return module.Change{Action: module.Create, Description: fmt.Sprintf("ufw %s %s", verb, r.describe())}, nil
}

func (r Rule) describe() string {
	if r.App != "" {
		return r.App
	}
	spec := r.Port
	if r.Proto != "" {
		spec += "/" + r.Proto
	}
	if r.From != "" {
		return fmt.Sprintf("%s from %s", spec, r.From)
	}
	return spec
}

func (r Rule) Apply(ctx context.Context, run module.Runner, change module.Change) (module.Result, error) {
	out, err := run.Run(ctx, r.applyCmd())
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply ufw rule: %w", err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (r Rule) applyCmd() string {
	action := "deny"
	if r.Allow {
		action = "allow"
	}
	if r.App != "" {
		return fmt.Sprintf("ufw %s %s 2>&1", action, shellquote.Quote(r.App))
	}
	if r.From == "" {
		spec := r.Port
		if r.Proto != "" {
			spec += "/" + r.Proto
		}
		return fmt.Sprintf("ufw %s %s 2>&1", action, shellquote.Quote(spec))
	}
	cmd := fmt.Sprintf("ufw %s from %s to any port %s", action, shellquote.Quote(r.From), shellquote.Quote(r.Port))
	if r.Proto != "" {
		cmd += " proto " + shellquote.Quote(r.Proto)
	}
	return cmd + " 2>&1"
}

func (r Rule) Verify(ctx context.Context, run module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, r, run, desired)
}

// Package apt implements module.Module for apt packages: smed's version
// of Ansible's apt module. Only apt is built, not a generic
// "package manager" abstraction — if dnf support is ever needed it's its
// own sibling package with its own Installed type, same reasoning as
// keeping file.Content and file.Directory separate rather than one
// polymorphic type branching on what kind of thing it is.
package apt

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Installed ensures Name is installed, at Version if set ("" means any
// installed version is fine).
type Installed struct {
	Name    string
	Version string
}

type state struct {
	Installed bool
	Version   string
}

func (i Installed) Desired() module.State {
	return state{Installed: true, Version: i.Version}
}

// Read uses dpkg-query, not apt, because dpkg-query answers "what's
// actually on disk" directly from the local package database — no
// network, no lock contention with a concurrent install elsewhere,
// genuinely read-only. An unknown package is a normal outcome here (not
// installed), not a Read failure: dpkg-query's own "no packages found"
// message on an unrecognized name is treated as exactly that, not
// propagated as an error.
func (i Installed) Read(ctx context.Context, r module.Runner) (module.State, error) {
	cmd := fmt.Sprintf(`dpkg-query -W -f='${Status}\t${Version}\n' %s 2>&1`, shellquote.Quote(i.Name))
	out, err := r.Run(ctx, cmd)
	trimmed := strings.TrimSpace(out)

	if trimmed == "" || strings.Contains(trimmed, "no packages found matching") {
		return state{Installed: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read package %s: %w (%s)", i.Name, err, trimmed)
	}

	parts := strings.SplitN(trimmed, "\t", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("read package %s: unexpected dpkg-query output %q", i.Name, trimmed)
	}
	status, version := parts[0], parts[1]
	if status != "install ok installed" {
		// Known to dpkg but not actually installed right now — e.g.
		// "deinstall ok config-files" (removed, config left behind).
		// Treated the same as not installed: Diff will say Create.
		return state{Installed: false}, nil
	}
	return state{Installed: true, Version: version}, nil
}

func (i Installed) Diff(desired, current module.State) (module.Change, error) {
	d, ok := desired.(state)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not apt.state", desired)
	}
	cur, ok := current.(state)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not apt.state", current)
	}

	if !cur.Installed {
		return module.Change{Action: module.Create, Description: fmt.Sprintf("install %s", i.Name)}, nil
	}
	if d.Version != "" && cur.Version != d.Version {
		return module.Change{Action: module.Update, Description: fmt.Sprintf("change %s version %s -> %s", i.Name, cur.Version, d.Version)}, nil
	}
	return module.Change{Action: module.NoOp}, nil
}

const maxApplyAttempts = 6

// lockRetryDelay is a var, not a const, so tests can shrink it — real
// runs want a few seconds of backoff, tests want this to take
// milliseconds, and the retry logic itself is what's under test here,
// not the wall-clock wait.
var lockRetryDelay = 2 * time.Second

// Apply runs apt-get install directly, and self-heals exactly two kinds
// of failure rather than guessing up front what's needed:
//
//   - stale package list ("unable to locate package" / "version ... was
//     not found"): run apt-get update once, then retry. Cheaper than an
//     unconditional update before every install, since a fresh cache
//     costs nothing extra here.
//   - dpkg lock held by a concurrent apt/dpkg invocation on the same
//     host: wait briefly and retry, bounded — this is a real
//     correctness gap otherwise, since the DAG runs independent steps
//     concurrently and apt does not support that on one host.
//
// Anything else is a real failure and returns immediately — this loop
// is not a generic "retry until it works".
func (i Installed) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	pkg := i.Name
	if i.Version != "" {
		pkg = i.Name + "=" + i.Version
	}
	installCmd := fmt.Sprintf("DEBIAN_FRONTEND=noninteractive apt-get install -y %s 2>&1", shellquote.Quote(pkg))

	updatedOnce := false
	var out string
	var err error
	for attempt := 1; attempt <= maxApplyAttempts; attempt++ {
		out, err = r.Run(ctx, installCmd)
		if err == nil {
			return module.Result{Changed: true, Output: out}, nil
		}

		switch {
		case looksLikeDpkgLockHeld(out) && attempt < maxApplyAttempts:
			select {
			case <-time.After(lockRetryDelay):
			case <-ctx.Done():
				return module.Result{Output: out}, ctx.Err()
			}
			continue

		case looksLikePackageNotFound(out) && !updatedOnce:
			if _, uerr := r.Run(ctx, "apt-get update 2>&1"); uerr != nil {
				return module.Result{Output: out}, fmt.Errorf("apt-get update: %w", uerr)
			}
			updatedOnce = true
			continue

		default:
			return module.Result{Output: out}, fmt.Errorf("install %s: %w", i.Name, err)
		}
	}
	return module.Result{Output: out}, fmt.Errorf("install %s: %w", i.Name, err)
}

func (i Installed) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, i, r, desired)
}

func looksLikePackageNotFound(out string) bool {
	return strings.Contains(out, "Unable to locate package") ||
		(strings.Contains(out, "Version") && strings.Contains(out, "was not found"))
}

func looksLikeDpkgLockHeld(out string) bool {
	return strings.Contains(out, "Could not get lock") ||
		strings.Contains(out, "dpkg frontend lock") ||
		strings.Contains(out, "Unable to acquire the dpkg")
}

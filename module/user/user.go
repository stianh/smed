// Package user implements module.Module for Linux user accounts: smed's
// version of Ansible's user module. Deliberately minimal: no password
// management at all — not even via the secret package (see README.md,
// "Secrets") — there's no password field to hold a resolved value
// correctly or incorrectly, which is a stronger guarantee than a field
// that's merely well-behaved. No Absent/delete either, consistent with
// every other module so far (file, apt, service all only converge
// toward existing; none of them use module.Delete yet). Always needs
// root — useradd/usermod do — same as apt.
package user

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Account ensures a user exists with the given attributes. Every
// attribute except Name is optional ("" means don't manage it) — same
// convention as file.Content's Mode/Owner/Group.
type Account struct {
	Name  string
	UID   string // "" means let the system assign one
	Group string // primary group name; "" means use the system default
	// Groups is the exact desired set of supplementary (non-primary)
	// group memberships, applied via usermod/useradd -G. nil means
	// don't manage supplementary groups at all; a non-nil empty slice
	// means manage them to exactly none (usermod -G ''), removing
	// whatever secondary groups the account currently has. This
	// *replaces* the set rather than only adding to it — Ansible's own
	// default (append: no) — so it's the one mode this supports; a
	// pure "add without ever removing" mode is a cheap follow-up if
	// it's ever actually needed, not something this silently does.
	Groups  []string
	Home    string // "" means use the system default (e.g. /home/<name>)
	Shell   string // "" means use the system default
	Comment string // GECOS field
	// System, if true, creates the account with useradd -r (system
	// account: no home directory by default) instead of useradd -m
	// (create a home directory) — the two conventions real service
	// accounts (postgres, deploy) and real human accounts actually use.
	System bool
}

type state struct {
	Exists  bool
	UID     string
	Group   string
	Groups  []string // supplementary groups only, normalized (sorted, deduped)
	Home    string
	Shell   string
	Comment string
}

func (a Account) Desired() module.State {
	return state{
		Exists: true, UID: a.UID, Group: a.Group, Groups: normalizeGroups(a.Groups),
		Home: a.Home, Shell: a.Shell, Comment: a.Comment,
	}
}

// normalizeGroups sorts and dedupes, so comparison doesn't care about
// the order a caller happened to list groups in or an accidental
// duplicate. Preserves nil (vs. returning an empty-but-non-nil slice),
// since nil vs. empty is the "don't manage" vs. "manage to none"
// distinction Account.Groups documents.
func normalizeGroups(groups []string) []string {
	if groups == nil {
		return nil
	}
	seen := make(map[string]bool, len(groups))
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}

const sep = "---SMED-SEP---"

// Read combines getent (the passwd record), id -gn (the primary
// group's name — passwd only has the numeric gid), and id -Gn (every
// group the user belongs to, primary included) into one round trip. A
// nonexistent user is a normal result (Exists=false), not a Read
// error: getent exits nonzero and prints nothing for an unknown name,
// which the `; true` at the end keeps from being treated as a failure.
func (a Account) Read(ctx context.Context, r module.Runner) (module.State, error) {
	cmd := fmt.Sprintf(
		"getent passwd %s 2>&1; echo %s; id -gn %s 2>&1; echo %s; id -Gn %s 2>&1; true",
		shellquote.Quote(a.Name), sep, shellquote.Quote(a.Name), sep, shellquote.Quote(a.Name),
	)
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("read user %s: %w", a.Name, err)
	}

	parts := strings.SplitN(out, sep, 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("read user %s: unexpected output %q", a.Name, out)
	}
	passwdLine := strings.TrimSpace(parts[0])
	groupName := strings.TrimSpace(parts[1])
	allGroupsLine := strings.TrimSpace(parts[2])

	if passwdLine == "" {
		return state{Exists: false}, nil
	}

	// getent passwd format: name:x:uid:gid:comment:home:shell
	fields := strings.Split(passwdLine, ":")
	if len(fields) != 7 {
		return nil, fmt.Errorf("read user %s: unexpected getent output %q", a.Name, passwdLine)
	}

	// id -Gn lists every group the user belongs to, primary included —
	// subtract the primary to get the supplementary set Account.Groups
	// actually manages.
	var secondary []string
	for _, g := range strings.Fields(allGroupsLine) {
		if g != groupName {
			secondary = append(secondary, g)
		}
	}

	return state{
		Exists:  true,
		UID:     fields[2],
		Group:   groupName,
		Groups:  normalizeGroups(secondary),
		Comment: fields[4],
		Home:    fields[5],
		Shell:   fields[6],
	}, nil
}

func (a Account) Diff(desired, current module.State) (module.Change, error) {
	d, ok := desired.(state)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not user.state", desired)
	}
	cur, ok := current.(state)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not user.state", current)
	}

	if !cur.Exists {
		return module.Change{Action: module.Create, Description: fmt.Sprintf("create user %s", a.Name)}, nil
	}

	var diffs []string
	if d.UID != "" && cur.UID != d.UID {
		diffs = append(diffs, fmt.Sprintf("uid %s -> %s", cur.UID, d.UID))
	}
	if d.Group != "" && cur.Group != d.Group {
		diffs = append(diffs, fmt.Sprintf("group %s -> %s", cur.Group, d.Group))
	}
	if a.Groups != nil && !slices.Equal(cur.Groups, d.Groups) {
		diffs = append(diffs, fmt.Sprintf("groups %v -> %v", cur.Groups, d.Groups))
	}
	if d.Home != "" && cur.Home != d.Home {
		diffs = append(diffs, fmt.Sprintf("home %s -> %s", cur.Home, d.Home))
	}
	if d.Shell != "" && cur.Shell != d.Shell {
		diffs = append(diffs, fmt.Sprintf("shell %s -> %s", cur.Shell, d.Shell))
	}
	if d.Comment != "" && cur.Comment != d.Comment {
		diffs = append(diffs, fmt.Sprintf("comment %q -> %q", cur.Comment, d.Comment))
	}
	if len(diffs) == 0 {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: fmt.Sprintf("%s: %s", a.Name, strings.Join(diffs, ", "))}, nil
}

// Apply unconditionally enforces every managed attribute, not just
// whichever one Diff noticed first — same reasoning as every other
// module here: fixing only the diffed attribute would leave a second,
// independently-differing one wrong, and Verify would (correctly) catch
// it afterward.
func (a Account) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	var cmd string
	if change.Action == module.Create {
		cmd = "useradd"
		if a.System {
			cmd += " -r"
		} else {
			cmd += " -m"
		}
	} else {
		cmd = "usermod"
	}
	cmd += flagArgs(a) + " " + shellquote.Quote(a.Name) + " 2>&1"

	out, err := r.Run(ctx, cmd)
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply user %s: %w", a.Name, err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (a Account) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, a, r, desired)
}

// flagArgs builds the useradd/usermod flags shared by Create and
// Update — whichever attributes are managed (non-empty), nothing more.
func flagArgs(a Account) string {
	var b strings.Builder
	if a.UID != "" {
		fmt.Fprintf(&b, " -u %s", shellquote.Quote(a.UID))
	}
	if a.Group != "" {
		fmt.Fprintf(&b, " -g %s", shellquote.Quote(a.Group))
	}
	if a.Groups != nil {
		fmt.Fprintf(&b, " -G %s", shellquote.Quote(strings.Join(a.Groups, ",")))
	}
	if a.Home != "" {
		fmt.Fprintf(&b, " -d %s", shellquote.Quote(a.Home))
	}
	if a.Shell != "" {
		fmt.Fprintf(&b, " -s %s", shellquote.Quote(a.Shell))
	}
	if a.Comment != "" {
		fmt.Fprintf(&b, " -c %s", shellquote.Quote(a.Comment))
	}
	return b.String()
}

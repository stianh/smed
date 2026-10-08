// Package file implements module.Module for file content and directories:
// smed's version of Ansible's copy and file(state=directory) modules.
// Templating (Go's own text/template — see README.md, "Config is Go,
// not a data file") is a layer above this, not inside it — Content
// just wants exact bytes at an exact path.
package file

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Content ensures a file exists at Path with exactly Data as its
// contents. Mode, Owner, and Group are optional — "" means don't manage
// that attribute at all, not "clear it".
type Content struct {
	Path  string
	Data  string
	Mode  string // e.g. "0644"
	Owner string // e.g. "deploy"
	Group string // e.g. "deploy"
}

type contentState struct {
	Exists bool
	Data   string
	Mode   string
	Owner  string
	Group  string
}

func (c Content) Desired() module.State {
	return contentState{Exists: true, Data: c.Data, Mode: c.Mode, Owner: c.Owner, Group: c.Group}
}

// Read reports whether Path exists, its content, mode, owner, and group,
// in one round trip. Never writes anything: this is the honesty
// invariant the whole plan depends on.
func (c Content) Read(ctx context.Context, r module.Runner) (module.State, error) {
	// base64 -w0: GNU coreutils wraps encoded output at 76 chars by
	// default, which would break the line-based parsing below.
	cmd := fmt.Sprintf(
		`if [ -f %s ]; then echo EXISTS && stat -c '%%U %%G %%a' %s && base64 -w0 %s; else echo MISSING; fi`,
		shellquote.Quote(c.Path), shellquote.Quote(c.Path), shellquote.Quote(c.Path),
	)
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.Path, err)
	}

	lines := strings.SplitN(strings.TrimRight(out, "\n"), "\n", 3)
	if len(lines) == 0 || lines[0] != "EXISTS" {
		return contentState{Exists: false}, nil
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("read %s: unexpected output %q", c.Path, out)
	}
	owner, group, mode, err := parseOwnerGroupMode(lines[1])
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.Path, err)
	}
	var data string
	if len(lines) == 3 {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[2]))
		if err != nil {
			return nil, fmt.Errorf("read %s: decode content: %w", c.Path, err)
		}
		data = string(decoded)
	}
	return contentState{Exists: true, Data: data, Mode: mode, Owner: owner, Group: group}, nil
}

func (c Content) Diff(desired, current module.State) (module.Change, error) {
	d, ok := desired.(contentState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not file.contentState", desired)
	}
	cur, ok := current.(contentState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not file.contentState", current)
	}

	if !cur.Exists {
		return module.Change{Action: module.Create, Description: fmt.Sprintf("create %s", c.Path)}, nil
	}

	var diffs []string
	if cur.Data != d.Data {
		diffs = append(diffs, "contents")
	}
	if d.Mode != "" {
		changed, err := modeDiffers(d.Mode, cur.Mode)
		if err != nil {
			return module.Change{}, fmt.Errorf("diff: %w", err)
		}
		if changed {
			diffs = append(diffs, fmt.Sprintf("mode (%s -> %s)", cur.Mode, d.Mode))
		}
	}
	if d.Owner != "" && cur.Owner != d.Owner {
		diffs = append(diffs, fmt.Sprintf("owner (%s -> %s)", cur.Owner, d.Owner))
	}
	if d.Group != "" && cur.Group != d.Group {
		diffs = append(diffs, fmt.Sprintf("group (%s -> %s)", cur.Group, d.Group))
	}
	if len(diffs) == 0 {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: fmt.Sprintf("update %s: %s", c.Path, strings.Join(diffs, ", "))}, nil
}

// Apply unconditionally writes the full desired state — content, mode,
// ownership, whichever of those are managed — rather than trying to
// apply only whichever attribute Diff happened to notice first. Change
// only ever says Create or Update here; both mean the same thing to
// Apply: make it match Desired, completely. This also sidesteps a real
// trap: if Apply only fixed the one attribute Diff reported, a second
// attribute that also differed would still be wrong, Verify would
// (correctly) catch it, and the module would look broken despite each
// piece of logic being individually correct.
func (c Content) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	encoded := base64.StdEncoding.EncodeToString([]byte(c.Data))
	cmd := fmt.Sprintf("echo %s | base64 -d > %s", shellquote.Quote(encoded), shellquote.Quote(c.Path))
	if c.Mode != "" {
		cmd += fmt.Sprintf(" && chmod %s %s", c.Mode, shellquote.Quote(c.Path))
	}
	if spec := ownershipSpec(c.Owner, c.Group); spec != "" {
		cmd += fmt.Sprintf(" && chown %s %s", spec, shellquote.Quote(c.Path))
	}
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply %s: %w", c.Path, err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (c Content) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, c, r, desired)
}

// FilePath implements module.Pathed.
func (c Content) FilePath() string { return c.Path }

// parseOwnerGroupMode parses one line of `stat -c '%U %G %a'` output.
func parseOwnerGroupMode(line string) (owner, group, mode string, err error) {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return "", "", "", fmt.Errorf("unexpected stat output %q", line)
	}
	return fields[0], fields[1], fields[2], nil
}

// modeDiffers compares two mode strings as numbers, not text: "0644"
// (how people write modes) and "644" (how `stat -c %a` reports them) are
// the same mode, and comparing them as strings would report a change
// forever and never actually converge.
func modeDiffers(desired, current string) (bool, error) {
	dm, err := normalizeMode(desired)
	if err != nil {
		return false, fmt.Errorf("desired mode: %w", err)
	}
	cm, err := normalizeMode(current)
	if err != nil {
		return false, fmt.Errorf("current mode: %w", err)
	}
	return dm != cm, nil
}

func normalizeMode(s string) (string, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return "", fmt.Errorf("invalid mode %q: %w", s, err)
	}
	return strconv.FormatUint(v, 8), nil
}

// ownershipSpec builds the argument chown expects, handling owner-only,
// group-only, both, or (returning "") neither.
func ownershipSpec(owner, group string) string {
	switch {
	case owner != "" && group != "":
		return owner + ":" + group
	case owner != "":
		return owner
	case group != "":
		return ":" + group
	default:
		return ""
	}
}

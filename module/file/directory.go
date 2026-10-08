package file

import (
	"context"
	"fmt"
	"strings"

	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Directory ensures a directory exists at Path, with Owner, Group, and
// Mode managed if non-empty, exactly like Content's attributes.
type Directory struct {
	Path  string
	Owner string
	Group string
	Mode  string // e.g. "0755"
}

type dirState struct {
	Exists bool
	Owner  string
	Group  string
	Mode   string
}

func (d Directory) Desired() module.State {
	return dirState{Exists: true, Owner: d.Owner, Group: d.Group, Mode: d.Mode}
}

func (d Directory) Read(ctx context.Context, r module.Runner) (module.State, error) {
	cmd := fmt.Sprintf(
		`if [ -d %s ]; then echo EXISTS && stat -c '%%U %%G %%a' %s; else echo MISSING; fi`,
		shellquote.Quote(d.Path), shellquote.Quote(d.Path),
	)
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.Path, err)
	}

	lines := strings.SplitN(strings.TrimRight(out, "\n"), "\n", 2)
	if len(lines) == 0 || lines[0] != "EXISTS" {
		return dirState{Exists: false}, nil
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("read %s: unexpected output %q", d.Path, out)
	}
	owner, group, mode, err := parseOwnerGroupMode(lines[1])
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.Path, err)
	}
	return dirState{Exists: true, Owner: owner, Group: group, Mode: mode}, nil
}

func (d Directory) Diff(desired, current module.State) (module.Change, error) {
	ds, ok := desired.(dirState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not file.dirState", desired)
	}
	cur, ok := current.(dirState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not file.dirState", current)
	}

	if !cur.Exists {
		return module.Change{Action: module.Create, Description: fmt.Sprintf("create directory %s", d.Path)}, nil
	}

	var diffs []string
	if ds.Mode != "" {
		changed, err := modeDiffers(ds.Mode, cur.Mode)
		if err != nil {
			return module.Change{}, fmt.Errorf("diff: %w", err)
		}
		if changed {
			diffs = append(diffs, fmt.Sprintf("mode (%s -> %s)", cur.Mode, ds.Mode))
		}
	}
	if ds.Owner != "" && cur.Owner != ds.Owner {
		diffs = append(diffs, fmt.Sprintf("owner (%s -> %s)", cur.Owner, ds.Owner))
	}
	if ds.Group != "" && cur.Group != ds.Group {
		diffs = append(diffs, fmt.Sprintf("group (%s -> %s)", cur.Group, ds.Group))
	}
	if len(diffs) == 0 {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: fmt.Sprintf("update directory %s: %s", d.Path, strings.Join(diffs, ", "))}, nil
}

// Apply, like Content's, unconditionally enforces the full desired state
// rather than only whatever Diff happened to report first — see the
// comment on Content.Apply for why that matters.
func (d Directory) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	cmd := fmt.Sprintf("mkdir -p %s", shellquote.Quote(d.Path))
	if d.Mode != "" {
		cmd += fmt.Sprintf(" && chmod %s %s", d.Mode, shellquote.Quote(d.Path))
	}
	if spec := ownershipSpec(d.Owner, d.Group); spec != "" {
		cmd += fmt.Sprintf(" && chown %s %s", spec, shellquote.Quote(d.Path))
	}
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply %s: %w", d.Path, err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (d Directory) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, d, r, desired)
}

// FilePath implements module.Pathed.
func (d Directory) FilePath() string { return d.Path }

// Package module is smed's contract for "a thing that knows how to read,
// diff, and change one resource" — the part Ansible never made honest
// (its --check mode just hopes modules don't lie) and Terraform never made
// stateless (it trusts a state file instead of asking the resource). See
// README.md, "Architecture decisions."
//
// One deliberate difference from an early sketch of this is worth
// recording here: that sketch showed Read/Apply taking a generic *Target
// carrying both "where" and "what". In practice every module already
// carries its own "what" as struct fields (file.Content{Path, Data}, and
// later pkg.Installed("nginx")) — the only thing it needs from outside is
// "where to run commands", which is just a Runner. So Target doesn't
// exist as a type here; Module supplies its own Desired() state instead.
package module

import (
	"context"
	"fmt"

	"github.com/stianh/smed/dag"
)

// Runner is the only thing a Module needs from the outside world: a way
// to run a shell command against the real target and get its output
// back. *run.Session satisfies this directly — no adapter needed.
type Runner interface {
	Run(ctx context.Context, command string) (output string, err error)
}

// Pathed is implemented by any Module that manages a specific
// filesystem path — file.Content and file.Directory, so far. It's how
// ownership-scope and host-receipt reconciliation (see README.md, "No
// state file — deletion without one") find out which declared
// steps fall under a zone or should be remembered, without this
// package needing to know about every module type that happens to
// manage a path. Not implementing it just means a module's steps are
// invisible to that reconciliation — an honest limitation, not a bug.
//
// Named FilePath, not Path: both implementations already have a Path
// *field* (file.Content.Path, file.Directory.Path), and Go doesn't
// allow a type to have a method and a field with the same name.
type Pathed interface {
	FilePath() string
}

// State is a point-in-time description of a resource: either what Read
// found (current) or what a Module wants (desired, from Module.Desired).
// Opaque outside the Module that defines it — Diff only ever compares two
// States produced by the same Module, so a module's own concrete type
// underneath State is an implementation detail of that module alone.
type State any

// Action classifies a Change for plan output — see README.md, "The
// shape of a playbook" for how Plan/Explain/Apply use it.
type Action int

const (
	NoOp Action = iota
	Create
	Update
	Delete
	Replace
	// Deferred means Diff couldn't be computed yet because the resource
	// this one depends on doesn't exist yet — the honest version of
	// Terraform's "(known after apply)", rather than silently skipping
	// it or crashing. No module in this package produces it yet — it's
	// here so Action's vocabulary is
	// complete from day one rather than needing a breaking change later.
	Deferred
)

func (a Action) String() string {
	switch a {
	case NoOp:
		return "no-op"
	case Create:
		return "create"
	case Update:
		return "update"
	case Delete:
		return "delete"
	case Replace:
		return "replace"
	case Deferred:
		return "deferred"
	default:
		return "unknown"
	}
}

// Change is what Diff computed: what Apply should do, and how to
// describe it in plan output.
type Change struct {
	Action      Action
	Description string
}

// Result is what Apply actually did.
type Result struct {
	Changed bool
	Output  string
}

// Module is the one contract every resource type implements: command,
// file, package, service, user, and anything added later.
type Module interface {
	// Desired returns this module instance's own desired state, built
	// from whatever fields its constructor was given (e.g.
	// file.Content{Path: "...", Data: "..."}).
	Desired() State

	// Read inspects the real target and returns its current state. Must
	// never mutate anything — this is the one invariant that makes a
	// smed plan trustworthy, unlike Ansible's --check.
	Read(ctx context.Context, r Runner) (State, error)

	// Diff compares desired against current (from Read) and returns
	// what Apply should do.
	Diff(desired, current State) (Change, error)

	// Apply performs change against the real target.
	Apply(ctx context.Context, r Runner, change Change) (Result, error)

	// Verify re-reads the target after Apply and confirms it now
	// matches desired — catching a module that silently didn't do what
	// it claimed. Most modules can implement this as one line via
	// VerifyByReread.
	Verify(ctx context.Context, r Runner, desired State) error
}

// VerifyByReread is the default Verify implementation nearly every module
// wants: read the target again, diff against desired, and error unless
// the result is NoOp. A module only needs something different here if a
// full re-read is genuinely too expensive for what it manages.
func VerifyByReread(ctx context.Context, m Module, r Runner, desired State) error {
	current, err := m.Read(ctx, r)
	if err != nil {
		return fmt.Errorf("verify: read: %w", err)
	}
	change, err := m.Diff(desired, current)
	if err != nil {
		return fmt.Errorf("verify: diff: %w", err)
	}
	if change.Action != NoOp {
		return fmt.Errorf("verify: still %s after apply (%s)", change.Action, change.Description)
	}
	return nil
}

// RunModule is the whole Read→Diff→Apply→Verify sequence a module goes
// through once — the thing a dag.Step's Run closure actually calls (see
// Step, below). Skips Apply and Verify entirely when Diff says NoOp,
// since there's nothing to do and nothing to confirm.
func RunModule(ctx context.Context, m Module, r Runner) (Result, error) {
	current, err := m.Read(ctx, r)
	if err != nil {
		return Result{}, fmt.Errorf("read: %w", err)
	}

	change, err := m.Diff(m.Desired(), current)
	if err != nil {
		return Result{}, fmt.Errorf("diff: %w", err)
	}
	if change.Action == NoOp {
		return Result{Changed: false}, nil
	}

	result, err := m.Apply(ctx, r, change)
	if err != nil {
		return result, fmt.Errorf("apply: %w", err)
	}

	if err := m.Verify(ctx, r, m.Desired()); err != nil {
		return result, err
	}
	return result, nil
}

// PlanModule is RunModule's read-only half: Read, then Diff, then stop.
// Never calls Apply or Verify, so it carries the same never-mutates
// guarantee Read itself promises — this is what makes a smed plan
// trustworthy in a way Ansible's --check mode never was (that mode
// just hopes modules don't lie; here, planning literally cannot run
// Apply, because this function never calls it).
func PlanModule(ctx context.Context, m Module, r Runner) (Change, error) {
	current, err := m.Read(ctx, r)
	if err != nil {
		return Change{}, fmt.Errorf("read: %w", err)
	}
	change, err := m.Diff(m.Desired(), current)
	if err != nil {
		return Change{}, fmt.Errorf("diff: %w", err)
	}
	return change, nil
}

// Step wraps a Module into a dag.Step — the point of integration between
// this package and the scheduler built in dag. deps are the IDs of steps
// that must resolve first, exactly as dag.Step.Deps expects.
func Step(id string, m Module, r Runner, deps ...string) dag.Step {
	return dag.Step{
		ID:   id,
		Deps: deps,
		Run: func(ctx context.Context) (bool, error) {
			result, err := RunModule(ctx, m, r)
			return result.Changed, err
		},
	}
}

// Handler is Step gated by dag.IfAnyChanged: m only actually runs if at
// least one of deps reported Changed. This is the entire "notify"
// mechanism — see README.md, "A real dependency graph, not a flat
// task list": a handler is just a Step with this gate and ordinary
// dependency edges from whatever notifies it, not a separate subsystem
// alongside the graph.
func Handler(id string, m Module, r Runner, deps ...string) dag.Step {
	return dag.Step{
		ID:    id,
		Deps:  deps,
		RunIf: dag.IfAnyChanged,
		Run: func(ctx context.Context) (bool, error) {
			result, err := RunModule(ctx, m, r)
			return result.Changed, err
		},
	}
}

// PlanStep is Step's read-only twin: it calls PlanModule instead of
// RunModule, and writes the computed Change into *out once Run executes
// — a plain pointer, not a shared map behind a mutex, because each
// dag.Step's closure is the only writer of its own out, and nothing
// reads it until the whole graph has finished (dag.Graph.Run always
// joins every goroutine via its internal WaitGroup before returning).
// Reports changed as Action != NoOp, so a plan behaves identically to
// Apply for anything downstream that gates on it — in particular
// PlanHandler, below.
func PlanStep(id string, m Module, r Runner, out *Change, deps ...string) dag.Step {
	return dag.Step{
		ID:   id,
		Deps: deps,
		Run: func(ctx context.Context) (bool, error) {
			change, err := PlanModule(ctx, m, r)
			if err != nil {
				return false, err
			}
			*out = change
			return change.Action != NoOp, nil
		},
	}
}

// PlanHandler is Handler's read-only twin, gated the same way
// (dag.IfAnyChanged against whatever notified it) — so a plan reports
// a handler as "would run" under exactly the condition it actually
// would during Apply, not some separate approximation of that rule.
func PlanHandler(id string, m Module, r Runner, out *Change, deps ...string) dag.Step {
	return dag.Step{
		ID:    id,
		Deps:  deps,
		RunIf: dag.IfAnyChanged,
		Run: func(ctx context.Context) (bool, error) {
			change, err := PlanModule(ctx, m, r)
			if err != nil {
				return false, err
			}
			*out = change
			return change.Action != NoOp, nil
		},
	}
}

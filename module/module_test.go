package module

import (
	"context"
	"errors"
	"testing"

	"github.com/stianh/smed/dag"
)

// fakeModule is a minimal Module whose every behavior is controllable by
// field, for exercising RunModule's control flow in isolation — no shell,
// no network, no real resource at all.
type fakeModule struct {
	desired    State
	current    State
	diffAction Action
	diffErr    error
	applyErr   error
	verifyErr  error
	applied    *bool // set to true if Apply was called
}

func (m fakeModule) Desired() State { return m.desired }

func (m fakeModule) Read(ctx context.Context, r Runner) (State, error) {
	return m.current, nil
}

func (m fakeModule) Diff(desired, current State) (Change, error) {
	if m.diffErr != nil {
		return Change{}, m.diffErr
	}
	return Change{Action: m.diffAction, Description: "fake change"}, nil
}

func (m fakeModule) Apply(ctx context.Context, r Runner, change Change) (Result, error) {
	if m.applied != nil {
		*m.applied = true
	}
	if m.applyErr != nil {
		return Result{}, m.applyErr
	}
	return Result{Changed: true}, nil
}

func (m fakeModule) Verify(ctx context.Context, r Runner, desired State) error {
	return m.verifyErr
}

type noopRunner struct{}

func (noopRunner) Run(ctx context.Context, command string) (string, error) { return "", nil }

func TestRunModuleSkipsApplyOnNoOp(t *testing.T) {
	applied := false
	m := fakeModule{diffAction: NoOp, applied: &applied}

	result, err := RunModule(context.Background(), m, noopRunner{})
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if result.Changed {
		t.Fatal("expected Changed=false on NoOp")
	}
	if applied {
		t.Fatal("Apply should never be called when Diff says NoOp")
	}
}

func TestRunModuleCallsApplyAndVerifyOnChange(t *testing.T) {
	applied := false
	m := fakeModule{diffAction: Create, applied: &applied}

	result, err := RunModule(context.Background(), m, noopRunner{})
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !applied {
		t.Fatal("expected Apply to be called when Diff says Create")
	}
	if !result.Changed {
		t.Fatal("expected Changed=true")
	}
}

func TestRunModulePropagatesApplyError(t *testing.T) {
	m := fakeModule{diffAction: Create, applyErr: errors.New("boom")}

	_, err := RunModule(context.Background(), m, noopRunner{})
	if err == nil {
		t.Fatal("expected error when Apply fails")
	}
}

func TestRunModulePropagatesVerifyError(t *testing.T) {
	m := fakeModule{diffAction: Create, verifyErr: errors.New("drifted")}

	_, err := RunModule(context.Background(), m, noopRunner{})
	if err == nil {
		t.Fatal("expected error when Verify fails after Apply")
	}
}

func TestRunModulePropagatesDiffError(t *testing.T) {
	applied := false
	m := fakeModule{diffErr: errors.New("bad state"), applied: &applied}

	_, err := RunModule(context.Background(), m, noopRunner{})
	if err == nil {
		t.Fatal("expected error when Diff fails")
	}
	if applied {
		t.Fatal("Apply should never run if Diff itself failed")
	}
}

func TestPlanModuleNeverCallsApply(t *testing.T) {
	applied := false
	m := fakeModule{diffAction: Create, applied: &applied}

	change, err := PlanModule(context.Background(), m, noopRunner{})
	if err != nil {
		t.Fatalf("PlanModule: %v", err)
	}
	if applied {
		t.Fatal("PlanModule must never call Apply — that's the entire point of a plan")
	}
	if change.Action != Create {
		t.Fatalf("got Action %v, want Create", change.Action)
	}
}

func TestPlanModulePropagatesDiffError(t *testing.T) {
	m := fakeModule{diffErr: errors.New("bad state")}

	_, err := PlanModule(context.Background(), m, noopRunner{})
	if err == nil {
		t.Fatal("expected error when Diff fails")
	}
}

func TestPlanStepReportsChangedWhenActionIsNotNoOp(t *testing.T) {
	m := fakeModule{diffAction: Update}
	var out Change
	step := PlanStep("configure-nginx", m, noopRunner{}, &out)

	g, err := dag.NewGraph([]dag.Step{step})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)
	if results[0].Status != dag.StatusOK || !results[0].Changed {
		t.Fatalf("got %+v, want StatusOK and Changed=true", results[0])
	}
	if out.Action != Update {
		t.Fatalf("got out.Action %v, want Update", out.Action)
	}
}

func TestPlanStepReportsNotChangedOnNoOp(t *testing.T) {
	m := fakeModule{diffAction: NoOp}
	var out Change
	step := PlanStep("already-fine", m, noopRunner{}, &out)

	g, err := dag.NewGraph([]dag.Step{step})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)
	if results[0].Changed {
		t.Fatal("expected Changed=false on NoOp")
	}
	if out.Action != NoOp {
		t.Fatalf("got out.Action %v, want NoOp", out.Action)
	}
}

// TestPlanHandlerOnlyRunsWhenNotifierWouldChange proves PlanHandler is
// gated the same way Handler is — IfAnyChanged against its deps' own
// Changed outcome, now driven by planned Action rather than an actual
// Apply.
func TestPlanHandlerOnlyRunsWhenNotifierWouldChange(t *testing.T) {
	notifier := fakeModule{diffAction: Update}
	handler := fakeModule{diffAction: Update}
	var notifierOut, handlerOut Change

	notifierStep := PlanStep("configure", notifier, noopRunner{}, &notifierOut)
	handlerStep := PlanHandler("restart", handler, noopRunner{}, &handlerOut, "configure")

	g, err := dag.NewGraph([]dag.Step{notifierStep, handlerStep})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	var handlerResult dag.Result
	for _, r := range results {
		if r.ID == "restart" {
			handlerResult = r
		}
	}
	if handlerResult.Status != dag.StatusOK {
		t.Fatalf("expected handler to run since its notifier would change, got %v", handlerResult.Status)
	}
	if handlerOut.Action != Update {
		t.Fatalf("got handlerOut.Action %v, want Update", handlerOut.Action)
	}
}

func TestPlanHandlerNotNeededWhenNotifierWouldNotChange(t *testing.T) {
	notifier := fakeModule{diffAction: NoOp}
	handler := fakeModule{diffAction: Update}
	var notifierOut, handlerOut Change

	notifierStep := PlanStep("configure", notifier, noopRunner{}, &notifierOut)
	handlerStep := PlanHandler("restart", handler, noopRunner{}, &handlerOut, "configure")

	g, err := dag.NewGraph([]dag.Step{notifierStep, handlerStep})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	var handlerResult dag.Result
	for _, r := range results {
		if r.ID == "restart" {
			handlerResult = r
		}
	}
	if handlerResult.Status != dag.StatusNotNeeded {
		t.Fatalf("expected handler to be StatusNotNeeded since its notifier wouldn't change, got %v", handlerResult.Status)
	}
	// RunIf returned false, so Run (and thus PlanModule) never executed
	// — handlerOut must still be its zero value, not whatever Diff
	// would have said if asked.
	if handlerOut != (Change{}) {
		t.Fatalf("expected handlerOut to stay zero-valued, got %+v", handlerOut)
	}
}

// TestStepIntegratesWithDAG proves module.Step actually produces a usable
// dag.Step: a two-module chain, wired as dag dependencies, runs through
// the real scheduler built in dag, not just this package's own helpers.
func TestStepIntegratesWithDAG(t *testing.T) {
	var order []string
	orderedModule := func(id string, action Action) Module {
		return orderTrackingModule{id: id, action: action, order: &order}
	}

	stepA := Step("install-nginx", orderedModule("install-nginx", Create), noopRunner{})
	stepB := Step("configure-nginx", orderedModule("configure-nginx", Create), noopRunner{}, "install-nginx")

	g, err := dag.NewGraph([]dag.Step{stepA, stepB})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	results := g.Run(context.Background(), 0)
	for _, r := range results {
		if r.Status != dag.StatusOK {
			t.Fatalf("step %q: want StatusOK, got %v (err=%v)", r.ID, r.Status, r.Err)
		}
	}

	if len(order) != 2 || order[0] != "install-nginx" || order[1] != "configure-nginx" {
		t.Fatalf("got run order %v, want [install-nginx configure-nginx]", order)
	}
}

// orderTrackingModule is a Module that records its own ID into a shared
// slice when Applied, so a dag-level test can assert real execution
// order came out of the real scheduler, not just that no error occurred.
type orderTrackingModule struct {
	id     string
	action Action
	order  *[]string
}

func (m orderTrackingModule) Desired() State { return nil }
func (m orderTrackingModule) Read(ctx context.Context, r Runner) (State, error) {
	return nil, nil
}
func (m orderTrackingModule) Diff(desired, current State) (Change, error) {
	return Change{Action: m.action}, nil
}
func (m orderTrackingModule) Apply(ctx context.Context, r Runner, change Change) (Result, error) {
	*m.order = append(*m.order, m.id)
	return Result{Changed: true}, nil
}
func (m orderTrackingModule) Verify(ctx context.Context, r Runner, desired State) error {
	return nil
}

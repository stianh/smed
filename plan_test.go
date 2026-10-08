package smed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stianh/smed/dag"
	"github.com/stianh/smed/module"
	"github.com/stianh/smed/tailnet"
)

func TestPlanStepsCapturesActionPerStep(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).
		Run(orderTrackingModule{id: "b", action: module.NoOp, order: &[]string{}})

	// namedStep.id is auto-generated (fmt.Sprintf("%T#%d", m, index)),
	// not the orderTrackingModule.id field used above for readability
	// — fetch the real keys off h.steps itself, the same way dagSteps
	// does, rather than assuming they match.
	steps, changes := h.planSteps(noopRunner{})
	g, err := dag.NewGraph(steps)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	g.Run(context.Background(), 0)

	if changes[h.steps[0].id].Action != module.Create {
		t.Fatalf("got changes[%s].Action=%v, want Create", h.steps[0].id, changes[h.steps[0].id].Action)
	}
	if changes[h.steps[1].id].Action != module.NoOp {
		t.Fatalf("got changes[%s].Action=%v, want NoOp", h.steps[1].id, changes[h.steps[1].id].Action)
	}
}

func TestPlanStepsNeverCallApply(t *testing.T) {
	var order []string
	h := Host("web1").Run(orderTrackingModule{id: "a", action: module.Create, order: &order})

	steps, _ := h.planSteps(noopRunner{})
	g, err := dag.NewGraph(steps)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	g.Run(context.Background(), 0)

	if len(order) != 0 {
		t.Fatalf("planSteps must never reach Apply, but order=%v", order)
	}
}

func TestPlanHandlerGatingMirrorsHandler(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "configure", action: module.Update, order: &[]string{}}).
		Notify("restart").
		Handler("restart", orderTrackingModule{id: "restart", action: module.Update, order: &[]string{}})

	steps, _ := h.planSteps(noopRunner{})
	g, err := dag.NewGraph(steps)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	for _, r := range results {
		if r.ID == "restart" && r.Status != dag.StatusOK {
			t.Fatalf("restart: want StatusOK since configure would change, got %v", r.Status)
		}
	}
}

func TestPlanHandlerNotNeededWhenNotifierWouldNotChange(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "configure", action: module.NoOp, order: &[]string{}}).
		Notify("restart").
		Handler("restart", orderTrackingModule{id: "restart", action: module.Update, order: &[]string{}})

	steps, _ := h.planSteps(noopRunner{})
	g, err := dag.NewGraph(steps)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	for _, r := range results {
		if r.ID == "restart" && r.Status != dag.StatusNotNeeded {
			t.Fatalf("restart: want StatusNotNeeded since configure wouldn't change, got %v", r.Status)
		}
	}
}

func TestPlanRequiresTailnet(t *testing.T) {
	_, err := Host("web1").Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).Plan(context.Background(), Options{})
	if err == nil {
		t.Fatal("expected an error when Options.Tailnet is nil")
	}
}

func TestPlanRequiresAtLeastOneStep(t *testing.T) {
	_, err := Host("web1").Plan(context.Background(), Options{Tailnet: &fakeTailnet{}})
	if err == nil {
		t.Fatal("expected an error for a HostSpec with no steps")
	}
}

func TestPlanErrorsWhenSelectorMatchesNothing(t *testing.T) {
	h := Host("ghost").Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}})
	_, err := h.Plan(context.Background(), Options{Tailnet: &fakeTailnet{peers: []tailnet.Peer{
		{Hostname: "web1"},
	}}})
	if err == nil {
		t.Fatal("expected an error when the selector matches no targets")
	}
}

func TestPlanDriftTrueWhenAnyActionIsNotNoOp(t *testing.T) {
	plan := Plan{
		"a": {{ID: "step1", Action: module.NoOp}},
		"b": {{ID: "step1", Action: module.Create}},
	}
	if !plan.Drift() {
		t.Fatal("expected Drift()=true — host b would Create")
	}
}

func TestPlanDriftTrueOnPlanError(t *testing.T) {
	plan := Plan{
		"a": {{ID: "step1", Err: errors.New("read failed")}},
	}
	if !plan.Drift() {
		t.Fatal("expected Drift()=true — a step that failed to even plan counts as drift")
	}
}

func TestPlanDriftFalseWhenEverythingIsNoOp(t *testing.T) {
	plan := Plan{
		"a": {{ID: "step1", Action: module.NoOp}},
		"b": {{ID: "step1", Action: module.NoOp}, {ID: "step2", Action: module.NoOp}},
	}
	if plan.Drift() {
		t.Fatal("expected Drift()=false — nothing would change")
	}
}

func TestPlanStringIsSortedAndIncludesDescriptionAndError(t *testing.T) {
	plan := Plan{
		"zebra":  {{ID: "step1", Action: module.NoOp}},
		"alpha":  {{ID: "step1", Action: module.Create, Description: "create /etc/nginx.conf"}},
		"middle": {{ID: "step1", Err: errors.New("ssh timeout")}},
	}
	out := plan.String()

	alphaIdx := strings.Index(out, "alpha")
	middleIdx := strings.Index(out, "middle")
	zebraIdx := strings.Index(out, "zebra")
	if alphaIdx == -1 || middleIdx == -1 || zebraIdx == -1 || !(alphaIdx < middleIdx && middleIdx < zebraIdx) {
		t.Fatalf("expected alpha < middle < zebra (sorted), got:\n%s", out)
	}
	if !strings.Contains(out, "create /etc/nginx.conf") {
		t.Fatalf("expected description in output, got:\n%s", out)
	}
	if !strings.Contains(out, "ssh timeout") {
		t.Fatalf("expected error text in output, got:\n%s", out)
	}
}

package smed

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stianh/smed/connect"
	"github.com/stianh/smed/dag"
	"github.com/stianh/smed/module"
	"github.com/stianh/smed/tailnet"
)

func targets() []connect.Target {
	return []connect.Target{
		{Hostname: "web1", Tags: []string{"web", "prod"}},
		{Hostname: "web2", Tags: []string{"web", "prod"}},
		{Hostname: "db1", Tags: []string{"db", "prod"}},
	}
}

func TestSelectTargetsByHostname(t *testing.T) {
	h := Host("web1")
	got := h.selectTargets(targets())
	if len(got) != 1 || got[0].Hostname != "web1" {
		t.Fatalf("got %+v, want exactly web1", got)
	}
}

func TestSelectTargetsByTag(t *testing.T) {
	h := Tag("web")
	got := h.selectTargets(targets())
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2 (web1, web2)", len(got))
	}
}

func TestSelectTargetsNoMatch(t *testing.T) {
	h := Host("ghost")
	got := h.selectTargets(targets())
	if len(got) != 0 {
		t.Fatalf("got %+v, want no matches", got)
	}
}

// orderTrackingModule records its own id into a shared slice when
// Applied. action controls what Diff reports — module.Create (the
// common case for these tests: RunModule always applies it, proving
// real execution order without any shell at all) or module.NoOp (to
// prove a handler notified by this step does *not* run, since Apply
// is never reached at all).
type orderTrackingModule struct {
	id     string
	action module.Action
	order  *[]string
}

func (m orderTrackingModule) Desired() module.State { return nil }
func (m orderTrackingModule) Read(ctx context.Context, r module.Runner) (module.State, error) {
	return nil, nil
}
func (m orderTrackingModule) Diff(desired, current module.State) (module.Change, error) {
	return module.Change{Action: m.action}, nil
}
func (m orderTrackingModule) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	*m.order = append(*m.order, m.id)
	return module.Result{Changed: true}, nil
}
func (m orderTrackingModule) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return nil
}

type noopRunner struct{}

func (noopRunner) Run(ctx context.Context, command string) (string, error) { return "", nil }

func TestDagStepsRunSequentiallyInDeclaredOrder(t *testing.T) {
	var order []string
	h := Host("web1").
		Run(orderTrackingModule{id: "a", action: module.Create, order: &order}).
		Run(orderTrackingModule{id: "b", action: module.Create, order: &order}).
		Run(orderTrackingModule{id: "c", action: module.Create, order: &order})

	g, err := dag.NewGraph(h.dagSteps(noopRunner{}))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)
	for _, r := range results {
		if r.Status != dag.StatusOK {
			t.Fatalf("step %q: want StatusOK, got %v (err=%v)", r.ID, r.Status, r.Err)
		}
	}

	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("got order %v, want [a b c]", order)
	}
}

func TestDagStepsFailurePropagatesToLaterSteps(t *testing.T) {
	var order []string
	failing := failingModule{}
	h := Host("web1").
		Run(orderTrackingModule{id: "a", action: module.Create, order: &order}).
		Run(failing).
		Run(orderTrackingModule{id: "c", action: module.Create, order: &order})

	g, err := dag.NewGraph(h.dagSteps(noopRunner{}))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)
	if !anyFailed(results) {
		t.Fatal("expected one step to report StatusFailed")
	}

	if order[0] != "a" {
		t.Fatalf("step a should have run first, got order %v", order)
	}
	if len(order) != 1 {
		t.Fatalf("step c should have been skipped after b failed, but order is %v", order)
	}
}

func anyFailed(results []dag.Result) bool {
	for _, r := range results {
		if r.Status == dag.StatusFailed {
			return true
		}
	}
	return false
}

type failingModule struct{}

func (failingModule) Desired() module.State { return nil }
func (failingModule) Read(ctx context.Context, r module.Runner) (module.State, error) {
	return nil, nil
}
func (failingModule) Diff(desired, current module.State) (module.Change, error) {
	return module.Change{Action: module.Create}, nil
}
func (failingModule) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	return module.Result{}, errors.New("boom")
}
func (failingModule) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return nil
}

func TestHandlerRunsWhenNotifierChanged(t *testing.T) {
	var order []string
	config := orderTrackingModule{id: "config", action: module.Create, order: &order}
	restart := orderTrackingModule{id: "restart", action: module.Update, order: &order}

	h := Host("web1").Run(config).Notify("restart-nginx").Handler("restart-nginx", restart)

	g, err := dag.NewGraph(h.dagSteps(noopRunner{}))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	byID := map[string]dag.Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["restart-nginx"].Status != dag.StatusOK {
		t.Fatalf("handler: want StatusOK, got %v", byID["restart-nginx"].Status)
	}
	if len(order) != 2 || order[0] != "config" || order[1] != "restart" {
		t.Fatalf("got order %v, want [config restart]", order)
	}
}

func TestHandlerDoesNotRunWhenNotifierDidNotChange(t *testing.T) {
	var order []string
	// NoOp: the module's own Diff decides nothing changed, so
	// RunModule never calls Apply at all — the handler's dependency
	// genuinely never changed anything, not just "reported false".
	config := orderTrackingModule{id: "config", action: module.NoOp, order: &order}
	restart := orderTrackingModule{id: "restart", action: module.Update, order: &order}

	h := Host("web1").Run(config).Notify("restart-nginx").Handler("restart-nginx", restart)

	g, err := dag.NewGraph(h.dagSteps(noopRunner{}))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	byID := map[string]dag.Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["restart-nginx"].Status != dag.StatusNotNeeded {
		t.Fatalf("handler: want StatusNotNeeded, got %v", byID["restart-nginx"].Status)
	}
	if len(order) != 0 {
		t.Fatalf("restart should never have run, got order %v", order)
	}
}

func TestHandlerWithNoNotifierNeverRuns(t *testing.T) {
	restart := orderTrackingModule{id: "restart", action: module.Update, order: &[]string{}}
	h := Host("web1").Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).Handler("unused", restart)

	g, err := dag.NewGraph(h.dagSteps(noopRunner{}))
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	for _, r := range results {
		if r.ID == "unused" && r.Status != dag.StatusNotNeeded {
			t.Fatalf("unused: want StatusNotNeeded (nobody notified it), got %v", r.Status)
		}
	}
}

func TestApplyRejectsNotifyBeforeRun(t *testing.T) {
	h := Host("web1")
	h.Notify("something") // no Run yet — construction error
	h.Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}})

	_, err := h.Apply(context.Background(), Options{Tailnet: &fakeTailnet{peers: []tailnet.Peer{{Hostname: "web1"}}}})
	if err == nil {
		t.Fatal("expected an error: Notify was called before any Run")
	}
}

func TestApplyRejectsNotifyWithNoMatchingHandler(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).
		Notify("restart-nginx") // never declared via Handler

	_, err := h.Apply(context.Background(), Options{Tailnet: &fakeTailnet{peers: []tailnet.Peer{{Hostname: "web1"}}}})
	if err == nil {
		t.Fatal("expected an error: Notify references a Handler that was never declared")
	}
}

// fakeTailnet is just enough tailnet.Tailnet to drive Apply's pre-flight
// logic without any real network at all.
type fakeTailnet struct {
	peers []tailnet.Peer
}

func (f *fakeTailnet) Peers(ctx context.Context) ([]tailnet.Peer, error) { return f.peers, nil }
func (f *fakeTailnet) Self(ctx context.Context) (tailnet.Peer, error)    { return tailnet.Peer{}, nil }
func (f *fakeTailnet) Dial(ctx context.Context, p tailnet.Peer, port string) (net.Conn, error) {
	return nil, errors.New("fakeTailnet: Dial not implemented")
}

func TestApplyRequiresTailnet(t *testing.T) {
	_, err := Host("web1").Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).Apply(context.Background(), Options{})
	if err == nil {
		t.Fatal("expected an error when Options.Tailnet is nil")
	}
}

func TestApplyRequiresAtLeastOneStep(t *testing.T) {
	_, err := Host("web1").Apply(context.Background(), Options{Tailnet: &fakeTailnet{}})
	if err == nil {
		t.Fatal("expected an error for a HostSpec with no steps")
	}
}

func TestApplyErrorsWhenSelectorMatchesNothing(t *testing.T) {
	var order []string
	h := Host("ghost").Run(orderTrackingModule{id: "a", action: module.Create, order: &order})
	_, err := h.Apply(context.Background(), Options{Tailnet: &fakeTailnet{peers: []tailnet.Peer{
		{Hostname: "web1"},
	}}})
	if err == nil {
		t.Fatal("expected an error when the selector matches no targets")
	}
}

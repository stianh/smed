// Plan/drift: the read-only half of what Apply does. See README.md,
// "The shape of a playbook" — the short version is that the value of
// "see a diff before you touch anything" never actually depended on a
// state file; it depends on being able to read the real target and
// diff it against what's declared, which smed can already do on every
// Apply. Plan is that same Read+Diff, exposed on its own, with Apply
// (and Verify) never called at all.
//
// Terraform needs two separate concepts — plan (diff against its state
// file) and drift detection (diff the state file against reality,
// because the two can diverge) — because it has two sources of truth
// that can disagree with each other. smed only ever has one source of
// truth: the real target. So here, "drift" isn't a different
// operation from "plan" — it's the same Read+Diff, used to answer a
// slightly different question ("did anything change since I last
// looked?" instead of "what would applying do?"). One mechanism, two
// questions.
package smed

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/stianh/smed/connect"
	"github.com/stianh/smed/dag"
	"github.com/stianh/smed/module"
)

// PlanResult is one declared step's computed Change, without ever
// having been applied. Err is set if Read or Diff itself failed —
// distinct from a non-nil error meaning "this would fail to apply",
// which Plan has no way to know without actually trying.
type PlanResult struct {
	ID          string
	Action      module.Action
	Description string
	Err         error
}

// Plan is what HostSpec.Plan returns: every matched host's computed
// Change per declared step, keyed by hostname. The same shape as
// Results, for the same reason — so fmt.Println(plan) just works.
type Plan map[string][]PlanResult

// String renders Plan as a table — HOST, STEP, ACTION, DESCRIPTION,
// ERROR — sorted by hostname, same convention as Results.String().
func (p Plan) String() string {
	hosts := make([]string, 0, len(p))
	for h := range p {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tSTEP\tACTION\tDESCRIPTION\tERROR")
	for _, host := range hosts {
		for _, s := range p[host] {
			errText := ""
			if s.Err != nil {
				errText = s.Err.Error()
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", host, s.ID, s.Action, s.Description, errText)
		}
	}
	w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

// Drift reports whether anything, on any host, would change or failed
// to even plan — the one bool a cron job needs to decide its exit
// code. A step that errored counts as drift too: "I can't tell you
// this still matches" is not the same as "it matches."
func (p Plan) Drift() bool {
	for _, steps := range p {
		for _, s := range steps {
			if s.Err != nil || s.Action != module.NoOp {
				return true
			}
		}
	}
	return false
}

// Plan resolves the HostSpec's selector exactly as Apply does, then
// computes what Apply would do on every match — Read and Diff only,
// nothing else, including for Owns/Receipts reconciliation (own.go):
// planOwnsStep/planReceiptStep list the real target or read the
// receipt file exactly as their mutating counterparts do, but only
// ever report what they'd delete, never actually deleting it or
// writing the receipt file back. No BatchSize/HealthCheck gating
// here: those exist to bound the blast radius of actually changing
// things, and a plan never changes anything, so there's no blast
// radius to bound.
func (h *HostSpec) Plan(ctx context.Context, opts Options) (Plan, error) {
	if opts.Tailnet == nil {
		return nil, fmt.Errorf("smed: Options.Tailnet is required")
	}
	if len(h.steps) == 0 {
		return nil, fmt.Errorf("smed: %s has no steps — nothing to Run", h.describe())
	}
	if err := h.validate(); err != nil {
		return nil, err
	}

	peers, err := opts.Tailnet.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("smed: list tailnet peers: %w", err)
	}
	all := connect.Merge(peers, opts.Static)

	selected := h.selectTargets(all)
	if len(selected) == 0 {
		return nil, fmt.Errorf("smed: %s matched no targets", h.describe())
	}

	type outcome struct {
		host string
		res  []PlanResult
		err  error
	}
	outcomes := make([]outcome, len(selected))
	var wg sync.WaitGroup
	for i, target := range selected {
		wg.Add(1)
		go func(i int, target connect.Target) {
			defer wg.Done()
			res, err := h.planOne(ctx, target, opts)
			outcomes[i] = outcome{host: target.Hostname, res: res, err: err}
		}(i, target)
	}
	wg.Wait()

	plan := make(Plan, len(outcomes))
	for _, o := range outcomes {
		if o.err != nil {
			plan[o.host] = []PlanResult{{ID: "<setup>", Err: o.err}}
			continue
		}
		plan[o.host] = o.res
	}
	return plan, nil
}

// planOne mirrors applyOne exactly, but builds a read-only graph (see
// planSteps) and never calls Apply — same one-session-per-host
// lifetime, same real dag scheduler underneath.
func (h *HostSpec) planOne(ctx context.Context, target connect.Target, opts Options) ([]PlanResult, error) {
	sess, runner, err := openRunner(ctx, target, opts)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	steps, changes := h.planSteps(runner)
	g, err := dag.NewGraph(steps)
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	// g.Run blocks until every step's goroutine has been joined (its
	// internal sync.WaitGroup guarantees that), so reading through the
	// *module.Change pointers in changes afterward is safe — nothing
	// is still writing to them.
	results := g.Run(ctx, opts.MaxWorkersPerHost)

	out := make([]PlanResult, 0, len(results))
	for _, r := range results {
		pr := PlanResult{ID: r.ID}
		switch r.Status {
		case dag.StatusFailed:
			pr.Err = r.Err
		case dag.StatusOK:
			c := changes[r.ID]
			pr.Action = c.Action
			pr.Description = c.Description
		case dag.StatusSkipped:
			pr.Description = "skipped: a dependency failed to plan"
		case dag.StatusNotNeeded:
			pr.Description = "not needed: nothing it depends on would change"
		}
		out = append(out, pr)
	}
	return out, nil
}

// planSteps is dagSteps' read-only twin: the same step chain and
// handler wiring, built with module.PlanStep/PlanHandler instead of
// Step/Handler, so Apply is never reachable from this graph at all.
// Each step gets its own *module.Change slot in the returned map,
// filled in by the closure dag.Graph.Run executes — the caller must
// not read through these until after Run returns.
func (h *HostSpec) planSteps(runner module.Runner) ([]dag.Step, map[string]*module.Change) {
	changes := make(map[string]*module.Change, len(h.steps)+len(h.handlers))

	regular := make([]dag.Step, len(h.steps))
	for i, s := range h.steps {
		var deps []string
		if i > 0 {
			deps = []string{h.steps[i-1].id}
		}
		out := new(module.Change)
		changes[s.id] = out
		regular[i] = module.PlanStep(s.id, s.m, runner, out, deps...)
	}

	steps := append([]dag.Step{}, regular...)
	for _, hd := range h.handlers {
		out := new(module.Change)
		changes[hd.id] = out
		steps = append(steps, module.PlanHandler(hd.id, hd.m, runner, out, h.notifies[hd.id]...))
	}
	steps = append(steps, h.planReconciliationSteps(runner, regular, changes)...)

	return steps, changes
}

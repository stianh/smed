// Explain: the static half of understanding a chain, as opposed to
// Plan's live half. See README.md, "The shape of a playbook" — the
// short version is that Plan and Explain answer two different
// questions. Plan asks the
// real target something ("if I ran this right now, what would
// actually change?") and so needs a live connection, same cost as
// Apply minus the mutation. Explain asks the declared HostSpec itself
// something ("why does this step exist here, what does it depend on,
// what would have to change to make this handler run?") and needs
// nothing at all — no Tailnet, no Options, no network — because the
// answer is pure structure, built straight from what Run/Notify/
// Handler/Owns/Receipts already recorded. It's the thing to reach for
// before Plan, not instead of it: confirm the graph you declared is
// the graph you meant, then ask the real world what it'd do with it.
package smed

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// StepExplanation is one node's place in the graph: what it is, what
// it depends on, and in plain words, when it actually runs.
type StepExplanation struct {
	ID     string
	Kind   string // "run", "handler", "owns", "receipts"
	Module string // fmt.Sprintf("%T", the module) — "" for owns/receipts, which aren't a module.Module at all
	Deps   []string
	Runs   string // human-readable: when this step actually executes
}

// Explanation is what HostSpec.Explain returns: the whole chain's
// structure, in declaration order (regular steps, then handlers, then
// Owns/Receipts reconciliation) — the same order dagSteps/planSteps
// build them in, so this is an honest description of the real graph,
// not a separate approximation of it.
type Explanation struct {
	Target string // h.describe(): Host("web1") or Tag("web")
	Steps  []StepExplanation
}

// String renders Explanation as a table — ID, KIND, MODULE, DEPS, RUNS
// — in declaration order. No sorting: unlike Results/Plan there's no
// map and no hostname to sort by, just the one chain, in the order you
// wrote it.
func (e Explanation) String() string {
	var b strings.Builder
	fmt.Fprintln(&b, e.Target)
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tKIND\tMODULE\tDEPS\tRUNS")
	for _, s := range e.Steps {
		deps := strings.Join(s.Deps, ", ")
		if deps == "" {
			deps = "(none)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, s.Module, deps, s.Runs)
	}
	w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

// Explain describes the chain's structure without touching anything —
// no Tailnet, no live target, not even Options. Errors the same way
// Apply/Plan's pre-flight does (no steps, a bad Notify/Handler wiring)
// since there's nothing honest to explain about a chain that doesn't
// construct correctly in the first place.
func (h *HostSpec) Explain() (Explanation, error) {
	if len(h.steps) == 0 {
		return Explanation{}, fmt.Errorf("smed: %s has no steps — nothing to Run", h.describe())
	}
	if err := h.validate(); err != nil {
		return Explanation{}, err
	}

	var steps []StepExplanation
	for i, s := range h.steps {
		var deps []string
		runs := "always (first step — no dependency)"
		if i > 0 {
			deps = []string{h.steps[i-1].id}
			runs = "always, once its dependency resolves"
		}
		steps = append(steps, StepExplanation{
			ID:     s.id,
			Kind:   "run",
			Module: fmt.Sprintf("%T", s.m),
			Deps:   deps,
			Runs:   runs,
		})
	}

	for _, hd := range h.handlers {
		deps := h.notifies[hd.id]
		runs := "never — nothing notifies this handler"
		if len(deps) > 0 {
			runs = "only if one of its deps actually changes"
		}
		steps = append(steps, StepExplanation{
			ID:     hd.id,
			Kind:   "handler",
			Module: fmt.Sprintf("%T", hd.m),
			Deps:   deps,
			Runs:   runs,
		})
	}

	var reconDeps []string
	if len(h.steps) > 0 {
		reconDeps = []string{h.steps[len(h.steps)-1].id}
	}
	for _, pattern := range h.owns {
		steps = append(steps, StepExplanation{
			ID:   "owns:" + pattern,
			Kind: "owns",
			Deps: reconDeps,
			Runs: "always, after the declared chain",
		})
	}
	for _, key := range h.receiptKeys {
		steps = append(steps, StepExplanation{
			ID:   "receipts:" + key,
			Kind: "receipts",
			Deps: reconDeps,
			Runs: "always, after the declared chain",
		})
	}

	return Explanation{Target: h.describe(), Steps: steps}, nil
}

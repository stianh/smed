// Package smed is the root of the library: the Host/Tag DSL — config is
// Go, not a data file, see README.md — smed.Host("web1").Run(...).Run(...),
// built on top of tailnet, connect, run, module, and dag.
//
// This file does not introduce any new execution model: it's the glue
// that turns a declared chain of modules into exactly what dag already
// knows how to run, against targets connect already knows how to
// resolve and run already knows how to reach.
package smed

import (
	"context"
	"fmt"
	"sync"

	"github.com/stianh/smed/connect"
	"github.com/stianh/smed/dag"
	"github.com/stianh/smed/module"
	"github.com/stianh/smed/run"
	"github.com/stianh/smed/tailnet"
)

// HostSpec is a chain of module steps to run against whichever targets
// its selector matches. Build one with Host or Tag, extend it with Run,
// execute it with Apply.
type HostSpec struct {
	hostname string // set if selecting by exact hostname
	tag      string // set if selecting by tag ("" is never a valid tag)
	steps    []namedStep
	handlers []namedHandler
	notifies map[string][]string // handler ID -> IDs of steps that notify it

	// owns and receiptKeys drive deletion-without-a-state-file — see
	// own.go. Populated by Owns/Receipts, consumed by
	// reconciliationSteps inside dagSteps.
	owns        []string
	receiptKeys []string

	// constructionErr carries a declaration-time mistake (Notify called
	// before any Run) through to Apply's own error return, since Notify
	// itself returns *HostSpec for chaining and can't surface an error
	// directly without breaking that.
	constructionErr error
}

type namedStep struct {
	id string
	m  module.Module
}

type namedHandler struct {
	id string
	m  module.Module
}

// Host begins a chain of steps to run against exactly one target,
// matched by hostname.
func Host(hostname string) *HostSpec {
	return &HostSpec{hostname: hostname}
}

// Tag begins a chain of steps to run against every target carrying tag
// (the "tag:web" → "web" form — see README.md, "Tailscale is the
// backend, not a plugin").
func Tag(tag string) *HostSpec {
	return &HostSpec{tag: tag}
}

// Run appends m to the chain. Steps run in the order they're declared:
// each one depends on the step before it, matching how
// Host(...).Run(a).Run(b) actually reads — a script, not an independent
// set. Running independent steps on one host concurrently is something
// dag already supports; this DSL just doesn't expose it yet, because
// nothing built on top of it has needed to.
func (h *HostSpec) Run(m module.Module) *HostSpec {
	id := fmt.Sprintf("%T#%d", m, len(h.steps))
	h.steps = append(h.steps, namedStep{id: id, m: m})
	return h
}

// Notify records that the step just added notifies the handler named
// id: that handler will depend on this step, and run only if this step
// (or any other step that also notifies the same id) reports Changed.
// Must follow a Run — calling it first is a construction error
// surfaced by Apply, not here, since this needs to keep returning
// *HostSpec for chaining.
//
// This is deliberately the entire "notify" primitive — see README.md,
// "A real dependency graph, not a flat task list" for why nothing more
// is needed: the handler
// (declared separately via Handler) ends up as one dag.Step with a
// dependency edge from every step that notified it, gated by
// dag.IfAnyChanged. Running once even with several notifiers, and
// running only after all of them resolve, both fall out of that for
// free — they were never separate problems, just what a flat task list
// (Ansible's shape) has no way to express.
func (h *HostSpec) Notify(id string) *HostSpec {
	if len(h.steps) == 0 {
		if h.constructionErr == nil {
			h.constructionErr = fmt.Errorf("smed: Notify(%q) called before any Run", id)
		}
		return h
	}
	last := h.steps[len(h.steps)-1].id
	if h.notifies == nil {
		h.notifies = map[string][]string{}
	}
	h.notifies[id] = append(h.notifies[id], last)
	return h
}

// Handler declares a step that only runs if something notifies it (via
// Notify) with Changed=true. A handler nobody ever notifies simply
// never runs (StatusNotNeeded) — that's not an error, there was never
// anything for it to react to. Declaring the same id twice is caught
// as a duplicate step ID when the graph is built, the same way any
// other duplicate ID would be.
func (h *HostSpec) Handler(id string, m module.Module) *HostSpec {
	h.handlers = append(h.handlers, namedHandler{id: id, m: m})
	return h
}

// Options supplies everything a HostSpec needs that isn't the steps
// themselves: how to reach the tailnet, the connection-rule table, and
// whether to escalate privilege. This is exactly the config README.md's
// "smed is a library, not a framework" decision says belongs in your
// project, not in smed.
type Options struct {
	Tailnet tailnet.Tailnet
	Rules   []connect.Rule
	Static  []connect.StaticHost

	// Become, if true, wraps every step's Runner in run.Become.
	// BecomeUser "" means root.
	Become     bool
	BecomeUser string

	Port              string // "" means "22"
	MaxWorkersPerHost int    // passed straight through to dag.Graph.Run

	// BatchSize, if > 0, splits matched targets into sequential
	// batches of this size instead of running every one of them fully
	// concurrently at once. This deliberately introduces the barrier
	// between hosts that smed otherwise avoids by design — that's the
	// entire point of batching (see README.md, "Rolling batches are two
	// independent knobs, not one feature").
	// 0 (default) means unbounded: every matched host, fully
	// concurrent, unchanged from always.
	BatchSize int

	// HealthCheck, if set, runs once per host — concurrently within a
	// batch, same as Apply itself — after that host's batch finished
	// with no step failing, before the next batch is allowed to
	// start. A non-nil error here stops the rollout before starting
	// the next batch, the same as a step failure does. This is the
	// gate that makes rolling different from plain batching: without
	// it, BatchSize alone still limits concurrency, but nothing stops
	// a broken batch from being followed by another.
	HealthCheck func(ctx context.Context, target connect.Target, runner module.Runner) error
}

func (o Options) port() string {
	if o.Port == "" {
		return "22"
	}
	return o.Port
}

// Apply resolves the HostSpec's selector against real inventory, then
// runs its step chain against every match.
//
// With BatchSize and HealthCheck both unset (the default), this is
// unchanged from always: one goroutine per host, no barrier between
// them, exactly as dag.RunAcrossHosts already guarantees at the
// single-graph level. The returned error is only ever a pre-flight
// failure (bad Options, selector matched nothing); once per-host
// execution starts, a host's own problem lands in its own entry in the
// result map, never here — the same "a failure skips only its
// dependents, nothing unrelated" rule dag already enforces inside one
// graph applies across hosts too.
//
// Setting either BatchSize or HealthCheck opts into rolling batches
// (see README.md, "Rolling batches are two independent knobs, not one
// feature") and deliberately changes that contract for this call:
// targets run in sequential batches, and a
// batch that has any failed step, or fails HealthCheck, stops the
// rollout before the next batch starts — this time surfaced as Apply's
// own returned error, with results still containing everything that
// was actually attempted. That's the correct behavior for what you
// asked for, not a violation of the no-barrier default: you opted into
// the barrier by setting one of these.
func (h *HostSpec) Apply(ctx context.Context, opts Options) (Results, error) {
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

	rolling := opts.BatchSize > 0 || opts.HealthCheck != nil
	batches := batchTargets(selected, opts.BatchSize)

	results := make(Results, len(selected))
	for i, batch := range batches {
		batchResults := h.applyBatch(ctx, batch, opts)
		for host, res := range batchResults {
			results[host] = res
		}

		if !rolling {
			continue // legacy behavior: no gating, keep going regardless
		}
		if batchResults.Failed() {
			return results, fmt.Errorf("smed: rolling batch %d/%d failed, stopping before the remaining batch(es)", i+1, len(batches))
		}
		if opts.HealthCheck != nil {
			if err := h.checkBatchHealth(ctx, batch, opts); err != nil {
				return results, fmt.Errorf("smed: rolling batch %d/%d: health check failed: %w", i+1, len(batches), err)
			}
		}
	}
	return results, nil
}

// applyBatch runs every target in batch fully concurrently — one
// goroutine per host, same fan-out Apply always used, just scoped to
// however many targets are in this batch rather than always the whole
// selected set.
func (h *HostSpec) applyBatch(ctx context.Context, batch []connect.Target, opts Options) Results {
	type outcome struct {
		host string
		res  []dag.Result
		err  error
	}
	outcomes := make([]outcome, len(batch))
	var wg sync.WaitGroup
	for i, target := range batch {
		wg.Add(1)
		go func(i int, target connect.Target) {
			defer wg.Done()
			res, err := h.applyOne(ctx, target, opts)
			outcomes[i] = outcome{host: target.Hostname, res: res, err: err}
		}(i, target)
	}
	wg.Wait()

	results := make(Results, len(outcomes))
	for _, o := range outcomes {
		if o.err != nil {
			results[o.host] = []dag.Result{{ID: "<setup>", Status: dag.StatusFailed, Err: o.err}}
			continue
		}
		results[o.host] = o.res
	}
	return results
}

// applyOne is everything that happens for a single resolved target:
// open exactly one session, build the step chain into a real
// dag.Graph, run it, and close the session — all inside this one
// goroutine, so the session's lifetime matches the work, not the
// whole batch's.
func (h *HostSpec) applyOne(ctx context.Context, target connect.Target, opts Options) ([]dag.Result, error) {
	sess, runner, err := openRunner(ctx, target, opts)
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	g, err := dag.NewGraph(h.dagSteps(runner))
	if err != nil {
		return nil, fmt.Errorf("build graph: %w", err)
	}
	return g.Run(ctx, opts.MaxWorkersPerHost), nil
}

// openRunner resolves who to connect as and opens one session for
// target — shared by applyOne and the rolling health-check, which
// needs the identical connection setup but a separate, short-lived
// session of its own (applyOne's session is already closed by the
// time a batch's results come back).
func openRunner(ctx context.Context, target connect.Target, opts Options) (*run.Session, module.Runner, error) {
	user, err := connect.Resolve(opts.Rules, target, "")
	if err != nil {
		return nil, nil, err
	}

	sess, err := run.Open(ctx, opts.Tailnet, target, user, opts.port())
	if err != nil {
		return nil, nil, fmt.Errorf("open session: %w", err)
	}

	var runner module.Runner = sess
	if opts.Become {
		runner = &run.Become{Inner: sess, TargetUser: opts.BecomeUser}
	}
	return sess, runner, nil
}

// validate checks declaration-time correctness that can't be caught as
// it happens, since Notify/Handler both return *HostSpec for chaining
// rather than an error: a Notify before any Run (constructionErr, set
// then), and a Notify naming a handler id that Handler never declared.
// Called once by Apply, before anything per-host — not inside dagSteps,
// which would otherwise repeat the same check once per matched host.
func (h *HostSpec) validate() error {
	if h.constructionErr != nil {
		return h.constructionErr
	}
	declared := make(map[string]bool, len(h.handlers))
	for _, hd := range h.handlers {
		declared[hd.id] = true
	}
	for id := range h.notifies {
		if !declared[id] {
			return fmt.Errorf("smed: Notify(%q) has no matching Handler(%q, ...)", id, id)
		}
	}
	return nil
}

// dagSteps turns the declared chain into real dag.Steps, each depending
// on the one before it, plus one dag.Step per declared Handler
// (dependencies = whichever steps notified it, via module.Handler's
// IfAnyChanged gate). Kept separate from applyOne so the sequencing
// logic is testable without opening a real connection at all. Assumes
// validate has already been called — Apply guarantees that.
func (h *HostSpec) dagSteps(runner module.Runner) []dag.Step {
	regular := make([]dag.Step, len(h.steps))
	for i, s := range h.steps {
		var deps []string
		if i > 0 {
			deps = []string{h.steps[i-1].id}
		}
		regular[i] = module.Step(s.id, s.m, runner, deps...)
	}

	steps := append([]dag.Step{}, regular...)
	for _, hd := range h.handlers {
		steps = append(steps, module.Handler(hd.id, hd.m, runner, h.notifies[hd.id]...))
	}
	steps = append(steps, h.reconciliationSteps(runner, regular)...)
	return steps
}

func (h *HostSpec) selectTargets(all []connect.Target) []connect.Target {
	var out []connect.Target
	switch {
	case h.hostname != "":
		for _, t := range all {
			if t.Hostname == h.hostname {
				out = append(out, t)
			}
		}
	case h.tag != "":
		for _, t := range all {
			if t.HasTag(h.tag) {
				out = append(out, t)
			}
		}
	}
	return out
}

func (h *HostSpec) describe() string {
	if h.hostname != "" {
		return fmt.Sprintf("Host(%q)", h.hostname)
	}
	return fmt.Sprintf("Tag(%q)", h.tag)
}

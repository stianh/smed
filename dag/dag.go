// Package dag is smed's execution engine: a dependency graph of steps,
// walked concurrently. See README.md, "A real dependency graph, not a
// flat task list" — the two properties that matter are that
// independent steps run concurrently (bounded by a worker limit) and
// that a failed step skips only its
// dependents, never anything unrelated. Across hosts, that second property
// is what "no implicit barrier" means: RunAcrossHosts gives every host its
// own independent walk of its own Graph, so one slow or failing host never
// holds up another.
//
// This package knows nothing about SSH, Tailscale, or modules — a Step's
// Run func just reports whether it changed anything and whether it
// failed. module.Step is the real Run implementation this scheduler
// runs on top of.
package dag

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Step is one node in the graph. ID must be unique within a Graph. Deps
// lists the IDs that must resolve (not necessarily succeed — see Status)
// before this step is eligible to run. Run reports whether it changed
// anything, independent of whether it also failed — a step can
// legitimately report changed=true and a non-nil error together (it did
// something, then failed a later check), and that distinction is worth
// keeping rather than collapsing into one bool.
type Step struct {
	ID   string
	Deps []string
	// RunIf, if set, is checked once every Dep has resolved: Run is
	// only actually called if RunIf returns true given each
	// dependency's Result (keyed by dependency ID). nil means always
	// run — the original, unconditional behavior. This is the entire
	// handler mechanism: IfAnyChanged plus real dependency edges, not
	// a second subsystem alongside the graph. See README.md, "A real
	// dependency graph, not a flat task list".
	RunIf func(deps map[string]Result) bool
	Run   func(ctx context.Context) (changed bool, err error)
}

// IfAnyChanged is the handler case: a Step gated by this only runs if
// at least one of its dependencies reported Changed. Combined with
// ordinary dependency edges for "notify" relationships, this is the
// whole mechanism — a handler notified by several steps is just one
// node with several incoming edges, which already runs at most once
// and already runs after all of them resolve; nothing extra was needed
// for either of those properties.
func IfAnyChanged(deps map[string]Result) bool {
	for _, d := range deps {
		if d.Changed {
			return true
		}
	}
	return false
}

// Status is a step's outcome after a Run of the graph.
type Status int

const (
	// StatusOK means Run executed and returned nil.
	StatusOK Status = iota
	// StatusFailed means Run executed and returned a non-nil error.
	StatusFailed
	// StatusSkipped means this step never ran because a dependency
	// (directly or transitively) failed or was itself skipped.
	StatusSkipped
	// StatusNotNeeded means this step had a RunIf gate that returned
	// false: it never ran, but — unlike StatusSkipped — this isn't a
	// failure propagating from upstream, it's a deliberate, successful
	// "nothing to do" (the handler-not-notified case). Dependents are
	// not blocked by it, the way they are by StatusFailed/StatusSkipped.
	StatusNotNeeded
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusFailed:
		return "failed"
	case StatusSkipped:
		return "skipped"
	case StatusNotNeeded:
		return "not-needed"
	default:
		return "unknown"
	}
}

// Result is one step's outcome from one Run of the graph. Changed is
// only meaningful when Status is StatusOK or StatusFailed — a skipped
// step never ran, so it's always false for one of those, by construction
// rather than by convention.
type Result struct {
	ID       string
	Status   Status
	Changed  bool
	Err      error
	Duration time.Duration
}

// Graph is a validated, ready-to-execute set of steps: unique IDs, every
// dependency reference resolved, no cycles. Build one with NewGraph.
type Graph struct {
	steps      map[string]Step
	dependents map[string][]string // reverse adjacency: id -> steps that depend on it
	order      []string            // stable iteration order, for deterministic seeding
}

// NewGraph validates steps and returns a Graph, or an error naming exactly
// what's wrong: a duplicate ID, a dependency on an ID that doesn't exist,
// or a cycle.
func NewGraph(steps []Step) (*Graph, error) {
	byID := make(map[string]Step, len(steps))
	order := make([]string, 0, len(steps))
	for _, s := range steps {
		if s.ID == "" {
			return nil, fmt.Errorf("step has empty ID")
		}
		if _, dup := byID[s.ID]; dup {
			return nil, fmt.Errorf("duplicate step ID %q", s.ID)
		}
		if s.Run == nil {
			return nil, fmt.Errorf("step %q has no Run func", s.ID)
		}
		byID[s.ID] = s
		order = append(order, s.ID)
	}
	for _, s := range steps {
		for _, dep := range s.Deps {
			if _, ok := byID[dep]; !ok {
				return nil, fmt.Errorf("step %q depends on unknown step %q", s.ID, dep)
			}
		}
	}

	dependents := make(map[string][]string, len(steps))
	for _, s := range steps {
		for _, dep := range s.Deps {
			dependents[dep] = append(dependents[dep], s.ID)
		}
	}

	g := &Graph{steps: byID, dependents: dependents, order: order}
	if cyc := g.findCycle(); cyc != nil {
		return nil, fmt.Errorf("dependency cycle: %v", cyc)
	}
	return g, nil
}

// findCycle does a plain DFS cycle check and returns the cycle (as a slice
// of IDs) if one exists, nil otherwise.
func (g *Graph) findCycle() []string {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(g.steps))
	var path []string

	var visit func(id string) []string
	visit = func(id string) []string {
		state[id] = visiting
		path = append(path, id)
		for _, dep := range g.steps[id].Deps {
			switch state[dep] {
			case visiting:
				// found the cycle: path from dep's first occurrence to here
				start := 0
				for i, p := range path {
					if p == dep {
						start = i
						break
					}
				}
				return append(path[start:], dep)
			case unvisited:
				if cyc := visit(dep); cyc != nil {
					return cyc
				}
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		return nil
	}

	for _, id := range g.order {
		if state[id] == unvisited {
			if cyc := visit(id); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

// Run walks the graph once: steps with no unresolved dependencies run
// immediately, concurrently, bounded by maxWorkers (<= 0 means
// unbounded — in practice capped at the step count, since there's never
// more real concurrency available than that). A step whose dependency
// failed or was skipped is itself reported Skipped without running;
// everything not downstream of a failure runs normally regardless of
// where in the graph it sits.
func (g *Graph) Run(ctx context.Context, maxWorkers int) []Result {
	n := len(g.steps)
	if n == 0 {
		return nil
	}
	if maxWorkers <= 0 || maxWorkers > n {
		maxWorkers = n
	}

	var mu sync.Mutex
	remaining := make(map[string]int, n)
	blocked := make(map[string]bool, n)
	results := make(map[string]Result, n)
	resolved := 0

	for id, s := range g.steps {
		remaining[id] = len(s.Deps)
	}

	ready := make(chan string, n)

	// finish and seedOrSkip are mutually recursive and always called with
	// mu held; neither re-locks, so there's no reentrancy hazard despite
	// the recursion.
	var finish func(id string, res Result)
	seedOrSkip := func(id string) {
		if remaining[id] != 0 {
			return
		}
		if blocked[id] {
			finish(id, Result{ID: id, Status: StatusSkipped})
		} else {
			ready <- id
		}
	}
	finish = func(id string, res Result) {
		results[id] = res
		resolved++
		// Capture this call's own count before recursing: seedOrSkip
		// below can trigger nested finish calls (cascading skips), each
		// bumping the shared resolved counter further. Comparing the
		// shared variable after the loop would make every frame on the
		// unwind see resolved==n and all try to close the channel.
		// Exactly one call ever incremented resolved to precisely n, so
		// comparing the frozen local value is what makes the close
		// happen exactly once, regardless of recursion depth.
		myCount := resolved
		for _, dep := range g.dependents[id] {
			if _, already := results[dep]; already {
				continue // dep already resolved via a different branch
			}
			if res.Status == StatusFailed || res.Status == StatusSkipped {
				blocked[dep] = true
			}
			remaining[dep]--
			seedOrSkip(dep)
		}
		if myCount == n {
			close(ready)
		}
	}

	mu.Lock()
	for _, id := range g.order {
		seedOrSkip(id)
	}
	mu.Unlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxWorkers)
	for id := range ready {
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()

			step := g.steps[id]

			// By the time a step is pulled off ready, every Dep
			// already has a Result stored — that's the precondition
			// for being seeded. Safe to read them here.
			runIt := true
			if step.RunIf != nil {
				mu.Lock()
				depResults := make(map[string]Result, len(step.Deps))
				for _, depID := range step.Deps {
					depResults[depID] = results[depID]
				}
				mu.Unlock()
				runIt = step.RunIf(depResults)
			}

			start := time.Now()
			var changed bool
			var err error
			var status Status
			switch {
			case !runIt:
				status = StatusNotNeeded
			case ctx.Err() != nil:
				err = ctx.Err()
				status = StatusFailed
			default:
				changed, err = step.Run(ctx)
				status = StatusOK
				if err != nil {
					status = StatusFailed
				}
			}
			dur := time.Since(start)

			mu.Lock()
			finish(id, Result{ID: id, Status: status, Changed: changed, Err: err, Duration: dur})
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	out := make([]Result, 0, n)
	for _, id := range g.order {
		out = append(out, results[id])
	}
	return out
}

// RunAcrossHosts gives every host its own independent walk of its own
// Graph — one goroutine per host, no barrier between them. graphFor builds
// the per-host Graph (the same step topology, instantiated with host-
// specific Run closures); a slow or failing host never holds up another.
func RunAcrossHosts(ctx context.Context, hosts []string, maxWorkersPerHost int, graphFor func(host string) (*Graph, error)) map[string][]Result {
	out := make(map[string][]Result, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			g, err := graphFor(h)
			if err != nil {
				mu.Lock()
				out[h] = []Result{{ID: "<graph-build>", Status: StatusFailed, Err: err}}
				mu.Unlock()
				return
			}
			res := g.Run(ctx, maxWorkersPerHost)
			mu.Lock()
			out[h] = res
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return out
}

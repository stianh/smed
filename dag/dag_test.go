package dag

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func noop(context.Context) (bool, error) { return false, nil }

func TestNewGraphValidation(t *testing.T) {
	t.Run("duplicate ID", func(t *testing.T) {
		_, err := NewGraph([]Step{
			{ID: "a", Run: noop},
			{ID: "a", Run: noop},
		})
		if err == nil {
			t.Fatal("expected error for duplicate ID")
		}
	})

	t.Run("unknown dependency", func(t *testing.T) {
		_, err := NewGraph([]Step{
			{ID: "a", Deps: []string{"ghost"}, Run: noop},
		})
		if err == nil {
			t.Fatal("expected error for unknown dependency")
		}
	})

	t.Run("missing Run func", func(t *testing.T) {
		_, err := NewGraph([]Step{{ID: "a"}})
		if err == nil {
			t.Fatal("expected error for missing Run func")
		}
	})

	t.Run("direct cycle", func(t *testing.T) {
		_, err := NewGraph([]Step{
			{ID: "a", Deps: []string{"b"}, Run: noop},
			{ID: "b", Deps: []string{"a"}, Run: noop},
		})
		if err == nil {
			t.Fatal("expected error for cycle")
		}
	})

	t.Run("longer cycle", func(t *testing.T) {
		_, err := NewGraph([]Step{
			{ID: "a", Deps: []string{"c"}, Run: noop},
			{ID: "b", Deps: []string{"a"}, Run: noop},
			{ID: "c", Deps: []string{"b"}, Run: noop},
		})
		if err == nil {
			t.Fatal("expected error for longer cycle")
		}
	})

	t.Run("valid diamond", func(t *testing.T) {
		_, err := NewGraph([]Step{
			{ID: "a", Run: noop},
			{ID: "b", Deps: []string{"a"}, Run: noop},
			{ID: "c", Deps: []string{"a"}, Run: noop},
			{ID: "d", Deps: []string{"b", "c"}, Run: noop},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestRunOrdering(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(id string) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			return true, nil
		}
	}

	// a -> b -> d, a -> c -> d: d must run strictly after b and c, which
	// must both run strictly after a.
	g, err := NewGraph([]Step{
		{ID: "a", Run: record("a")},
		{ID: "b", Deps: []string{"a"}, Run: record("b")},
		{ID: "c", Deps: []string{"a"}, Run: record("c")},
		{ID: "d", Deps: []string{"b", "c"}, Run: record("d")},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	results := g.Run(context.Background(), 0)
	for _, r := range results {
		if r.Status != StatusOK {
			t.Fatalf("step %q: want StatusOK, got %v (err=%v)", r.ID, r.Status, r.Err)
		}
		if !r.Changed {
			t.Fatalf("step %q: want Changed=true (record() always reports a change)", r.ID)
		}
	}

	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["a"] >= pos["b"] || pos["a"] >= pos["c"] {
		t.Fatalf("a must run before b and c, got order %v", order)
	}
	if pos["b"] >= pos["d"] || pos["c"] >= pos["d"] {
		t.Fatalf("d must run after both b and c, got order %v", order)
	}
}

func TestIndependentStepsRunConcurrently(t *testing.T) {
	// Two independent steps that each sleep 150ms should finish in ~150ms
	// total, not ~300ms, if they're genuinely running concurrently rather
	// than being serialized by an implicit barrier.
	sleep := func(context.Context) (bool, error) {
		time.Sleep(150 * time.Millisecond)
		return false, nil
	}
	g, err := NewGraph([]Step{
		{ID: "x", Run: sleep},
		{ID: "y", Run: sleep},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	start := time.Now()
	g.Run(context.Background(), 0)
	elapsed := time.Since(start)

	if elapsed > 250*time.Millisecond {
		t.Fatalf("independent steps appear serialized: took %v, want ~150ms", elapsed)
	}
}

func TestWorkerLimitEnforced(t *testing.T) {
	var concurrent int32
	var maxSeen int32
	track := func(context.Context) (bool, error) {
		cur := atomic.AddInt32(&concurrent, 1)
		for {
			max := atomic.LoadInt32(&maxSeen)
			if cur <= max || atomic.CompareAndSwapInt32(&maxSeen, max, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return false, nil
	}

	steps := make([]Step, 0, 10)
	for i := 0; i < 10; i++ {
		steps = append(steps, Step{ID: string(rune('a' + i)), Run: track})
	}
	g, err := NewGraph(steps)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	g.Run(context.Background(), 3)

	if got := atomic.LoadInt32(&maxSeen); got > 3 {
		t.Fatalf("worker limit violated: saw %d concurrent steps, want <= 3", got)
	}
}

func TestFailurePropagationSkipsOnlyDependents(t *testing.T) {
	// Deliberately reports changed=true alongside the error: it did
	// something before failing, and Result should preserve that rather
	// than collapsing it to false just because the step also failed —
	// see the comment on Step.Run in dag.go.
	failing := func(context.Context) (bool, error) { return true, errors.New("boom") }

	var mu sync.Mutex
	ran := map[string]bool{}
	recordRan := func(id string) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) {
			mu.Lock()
			ran[id] = true
			mu.Unlock()
			return false, nil
		}
	}

	// a fails -> b (depends on a) must be skipped, not run.
	// c is unrelated and must run normally regardless of a's failure.
	g, err := NewGraph([]Step{
		{ID: "a", Run: failing},
		{ID: "b", Deps: []string{"a"}, Run: recordRan("b")},
		{ID: "c", Run: recordRan("c")},
		{ID: "d", Deps: []string{"b"}, Run: recordRan("d")}, // transitively blocked by a
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	results := g.Run(context.Background(), 0)
	byID := map[string]Result{}
	for _, r := range results {
		byID[r.ID] = r
	}

	if byID["a"].Status != StatusFailed {
		t.Fatalf("a: want StatusFailed, got %v", byID["a"].Status)
	}
	if !byID["a"].Changed {
		t.Fatal("a: want Changed=true even though it failed — it did something before the error")
	}
	if byID["b"].Status != StatusSkipped {
		t.Fatalf("b: want StatusSkipped (depends on failed a), got %v", byID["b"].Status)
	}
	if byID["b"].Changed {
		t.Fatal("b: want Changed=false — it never ran, so it can't have changed anything")
	}
	if byID["d"].Status != StatusSkipped {
		t.Fatalf("d: want StatusSkipped (transitively depends on failed a), got %v", byID["d"].Status)
	}
	if byID["c"].Status != StatusOK {
		t.Fatalf("c: want StatusOK (unrelated to a), got %v", byID["c"].Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if ran["b"] {
		t.Fatal("b should never have run — it was skipped")
	}
	if ran["d"] {
		t.Fatal("d should never have run — it was skipped")
	}
	if !ran["c"] {
		t.Fatal("c should have run — it's unrelated to the failure")
	}
}

func TestRunAcrossHostsHasNoBarrier(t *testing.T) {
	// hostA is slow, hostB is fast. If hosts were barriered against each
	// other, total wall clock would be forced to wait on hostA before
	// hostB could even be considered "done" in any meaningful sense — but
	// what we're really checking is that the whole call takes ~as long as
	// the slowest host alone, not longer, proving they ran concurrently.
	graphFor := func(host string) (*Graph, error) {
		delay := 10 * time.Millisecond
		if host == "hostA" {
			delay = 200 * time.Millisecond
		}
		return NewGraph([]Step{
			{ID: "step", Run: func(context.Context) (bool, error) {
				time.Sleep(delay)
				return false, nil
			}},
		})
	}

	start := time.Now()
	results := RunAcrossHosts(context.Background(), []string{"hostA", "hostB"}, 0, graphFor)
	elapsed := time.Since(start)

	if elapsed > 300*time.Millisecond {
		t.Fatalf("hosts appear barriered against each other: took %v, want ~200ms", elapsed)
	}
	for host, rs := range results {
		for _, r := range rs {
			if r.Status != StatusOK {
				t.Fatalf("host %s step %q: want StatusOK, got %v (err=%v)", host, r.ID, r.Status, r.Err)
			}
		}
	}
}

func TestRunIfGateRunsWhenDependencyChanged(t *testing.T) {
	var ran bool
	g, err := NewGraph([]Step{
		{ID: "config", Run: func(context.Context) (bool, error) { return true, nil }},
		{ID: "restart", Deps: []string{"config"}, RunIf: IfAnyChanged, Run: func(context.Context) (bool, error) {
			ran = true
			return true, nil
		}},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	byID := map[string]Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["restart"].Status != StatusOK {
		t.Fatalf("restart: want StatusOK, got %v", byID["restart"].Status)
	}
	if !ran {
		t.Fatal("restart's Run should have been called — config reported Changed=true")
	}
}

func TestRunIfGateSkipsWhenNothingChanged(t *testing.T) {
	var ran bool
	g, err := NewGraph([]Step{
		{ID: "config", Run: func(context.Context) (bool, error) { return false, nil }},
		{ID: "restart", Deps: []string{"config"}, RunIf: IfAnyChanged, Run: func(context.Context) (bool, error) {
			ran = true
			return true, nil
		}},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	byID := map[string]Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["restart"].Status != StatusNotNeeded {
		t.Fatalf("restart: want StatusNotNeeded, got %v", byID["restart"].Status)
	}
	if byID["restart"].Changed {
		t.Fatal("restart: want Changed=false — it never ran")
	}
	if ran {
		t.Fatal("restart's Run should never have been called — config reported Changed=false")
	}
}

func TestStatusNotNeededDoesNotBlockDependents(t *testing.T) {
	var laterRan bool
	g, err := NewGraph([]Step{
		{ID: "config", Run: func(context.Context) (bool, error) { return false, nil }},
		{ID: "restart", Deps: []string{"config"}, RunIf: IfAnyChanged, Run: func(context.Context) (bool, error) { return true, nil }},
		{ID: "later", Deps: []string{"restart"}, Run: func(context.Context) (bool, error) {
			laterRan = true
			return true, nil
		}},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	byID := map[string]Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["restart"].Status != StatusNotNeeded {
		t.Fatalf("restart: want StatusNotNeeded, got %v", byID["restart"].Status)
	}
	if byID["later"].Status != StatusOK {
		t.Fatalf("later: want StatusOK (not blocked by restart's NotNeeded), got %v", byID["later"].Status)
	}
	if !laterRan {
		t.Fatal("later's Run should have been called — StatusNotNeeded must not block dependents")
	}
}

func TestHandlerFanInRunsOnceEvenWithMultipleNotifiers(t *testing.T) {
	var restartCount int
	g, err := NewGraph([]Step{
		{ID: "config-a", Run: func(context.Context) (bool, error) { return true, nil }},
		{ID: "config-b", Run: func(context.Context) (bool, error) { return false, nil }},
		{ID: "restart", Deps: []string{"config-a", "config-b"}, RunIf: IfAnyChanged, Run: func(context.Context) (bool, error) {
			restartCount++
			return true, nil
		}},
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)

	byID := map[string]Result{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["restart"].Status != StatusOK {
		t.Fatalf("restart: want StatusOK (config-a changed), got %v", byID["restart"].Status)
	}
	if restartCount != 1 {
		t.Fatalf("restart ran %d times, want exactly 1 — a handler notified by two steps is still one node", restartCount)
	}
}

func TestEmptyGraph(t *testing.T) {
	g, err := NewGraph(nil)
	if err != nil {
		t.Fatalf("NewGraph(nil): %v", err)
	}
	results := g.Run(context.Background(), 0)
	if len(results) != 0 {
		t.Fatalf("expected no results for empty graph, got %v", results)
	}
}

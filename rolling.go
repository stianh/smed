package smed

// Rolling batches with a health check gate (see README.md, "Rolling
// batches are two independent knobs, not one feature", and
// Options.BatchSize/HealthCheck). Two independently useful knobs, not
// one feature:
//
//   - BatchSize alone limits concurrency — smaller blast radius, no
//     availability guarantee. Useful for load/resource reasons (don't
//     hit a shared connection pool or rate limit with every host at
//     once), independent of whether anything stays up throughout.
//   - HealthCheck adds the gate that actually protects availability:
//     don't start the next batch until this one is confirmed healthy.
//     Rolling is batching plus this; it's not a separate mechanism.

import (
	"context"
	"fmt"
	"sync"

	"github.com/stianh/smed/connect"
)

// batchTargets splits targets into sequential groups of size. size<=0
// or size>=len(targets) means "no splitting" — one batch containing
// everything, which is exactly today's behavior when rolling isn't
// engaged at all.
func batchTargets(targets []connect.Target, size int) [][]connect.Target {
	if size <= 0 || size >= len(targets) {
		return [][]connect.Target{targets}
	}
	var batches [][]connect.Target
	for i := 0; i < len(targets); i += size {
		end := i + size
		if end > len(targets) {
			end = len(targets)
		}
		batches = append(batches, targets[i:end])
	}
	return batches
}

// checkBatchHealth runs opts.HealthCheck against every target in batch
// concurrently — the same fan-out shape Apply itself uses, so a batch
// of N hosts pays for one round trip, not N sequential ones. Returns
// the first failure found; which one that is isn't deterministic when
// several hosts fail at once, but the only thing that actually matters
// is that the rollout stops.
func (h *HostSpec) checkBatchHealth(ctx context.Context, batch []connect.Target, opts Options) error {
	type outcome struct {
		host string
		err  error
	}
	outcomes := make([]outcome, len(batch))
	var wg sync.WaitGroup
	for i, target := range batch {
		wg.Add(1)
		go func(i int, target connect.Target) {
			defer wg.Done()
			outcomes[i] = outcome{host: target.Hostname, err: runHealthCheck(ctx, target, opts)}
		}(i, target)
	}
	wg.Wait()

	for _, o := range outcomes {
		if o.err != nil {
			return fmt.Errorf("%s: %w", o.host, o.err)
		}
	}
	return nil
}

// runHealthCheck opens its own short-lived session for target (the
// one applyOne used for this host is already closed by the time a
// batch's results are in hand) and calls opts.HealthCheck through it.
func runHealthCheck(ctx context.Context, target connect.Target, opts Options) error {
	sess, runner, err := openRunner(ctx, target, opts)
	if err != nil {
		return fmt.Errorf("open session for health check: %w", err)
	}
	defer sess.Close()
	return opts.HealthCheck(ctx, target, runner)
}

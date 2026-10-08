package smed

import (
	"testing"

	"github.com/stianh/smed/connect"
)

func targetNames(targets []connect.Target) []string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Hostname
	}
	return names
}

func fiveTargets() []connect.Target {
	return []connect.Target{
		{Hostname: "a"}, {Hostname: "b"}, {Hostname: "c"}, {Hostname: "d"}, {Hostname: "e"},
	}
}

func TestBatchTargetsSplitsIntoFixedSizeGroups(t *testing.T) {
	batches := batchTargets(fiveTargets(), 2)
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3 (2,2,1)", len(batches))
	}
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	for i, b := range batches {
		got := targetNames(b)
		if len(got) != len(want[i]) {
			t.Fatalf("batch %d: got %v, want %v", i, got, want[i])
		}
		for j := range got {
			if got[j] != want[i][j] {
				t.Fatalf("batch %d: got %v, want %v", i, got, want[i])
			}
		}
	}
}

func TestBatchTargetsZeroSizeMeansOneBatch(t *testing.T) {
	batches := batchTargets(fiveTargets(), 0)
	if len(batches) != 1 {
		t.Fatalf("got %d batches, want 1 (the default, unbatched, behavior)", len(batches))
	}
	if len(batches[0]) != 5 {
		t.Fatalf("got %d targets in the single batch, want all 5", len(batches[0]))
	}
}

func TestBatchTargetsSizeLargerThanTargetsMeansOneBatch(t *testing.T) {
	batches := batchTargets(fiveTargets(), 100)
	if len(batches) != 1 || len(batches[0]) != 5 {
		t.Fatalf("got %d batches, want 1 containing all 5", len(batches))
	}
}

func TestBatchTargetsExactMultiple(t *testing.T) {
	// 4 targets, batch size 2 — no uneven remainder to get wrong.
	targets := fiveTargets()[:4]
	batches := batchTargets(targets, 2)
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2", len(batches))
	}
	if len(batches[0]) != 2 || len(batches[1]) != 2 {
		t.Fatalf("got batch sizes %d, %d, want 2, 2", len(batches[0]), len(batches[1]))
	}
}

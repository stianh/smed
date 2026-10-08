package smed

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stianh/smed/dag"
)

func TestResultsFailedDetectsAnyFailedStep(t *testing.T) {
	results := Results{
		"a": {{ID: "step1", Status: dag.StatusOK}},
		"b": {{ID: "step1", Status: dag.StatusOK}, {ID: "step2", Status: dag.StatusFailed, Err: errors.New("boom")}},
	}
	if !results.Failed() {
		t.Fatal("expected Failed()=true — host b has a failed step")
	}
}

func TestResultsFailedFalseWhenAllOK(t *testing.T) {
	results := Results{
		"a": {{ID: "step1", Status: dag.StatusOK}},
		"b": {{ID: "step1", Status: dag.StatusOK}, {ID: "step2", Status: dag.StatusNotNeeded}},
	}
	if results.Failed() {
		t.Fatal("expected Failed()=false — StatusNotNeeded isn't a failure")
	}
}

func TestResultsStringIsSortedByHostname(t *testing.T) {
	results := Results{
		"zebra": {{ID: "step1", Status: dag.StatusOK}},
		"alpha": {{ID: "step1", Status: dag.StatusOK}},
	}
	out := results.String()
	alphaIdx := strings.Index(out, "alpha")
	zebraIdx := strings.Index(out, "zebra")
	if alphaIdx == -1 || zebraIdx == -1 || alphaIdx > zebraIdx {
		t.Fatalf("expected alpha before zebra (sorted), got:\n%s", out)
	}
}

func TestResultsStringIncludesErrorText(t *testing.T) {
	results := Results{
		"a": {{ID: "step1", Status: dag.StatusFailed, Err: errors.New("disk full")}},
	}
	out := results.String()
	if !strings.Contains(out, "disk full") {
		t.Fatalf("expected error text in output, got:\n%s", out)
	}
}

func TestResultsWorksWithPlainFmtPrinting(t *testing.T) {
	// The actual point of Results: fmt.Println(results) (or
	// log.Println, %v, %s, ...) should just work via the Stringer
	// interface, without the caller writing any loop at all.
	results := Results{"a": {{ID: "step1", Status: dag.StatusOK, Changed: true}}}
	got := fmt.Sprintf("%v", results)
	if !strings.Contains(got, "step1") || !strings.Contains(got, "ok") {
		t.Fatalf("got %q, want it to contain step1 and ok", got)
	}
}

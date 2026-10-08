package smed

import (
	"strings"
	"testing"

	"github.com/stianh/smed/module"
)

func TestExplainRequiresAtLeastOneStep(t *testing.T) {
	_, err := Host("web1").Explain()
	if err == nil {
		t.Fatal("expected an error for a HostSpec with no steps")
	}
}

func TestExplainPropagatesValidationErrors(t *testing.T) {
	h := Host("web1")
	h.Notify("restart") // no Run yet — construction error
	h.Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}})

	_, err := h.Explain()
	if err == nil {
		t.Fatal("expected an error: Notify was called before any Run")
	}
}

func TestExplainStepsHaveSequentialDeps(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).
		Run(orderTrackingModule{id: "b", action: module.Create, order: &[]string{}})

	e, err := h.Explain()
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(e.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(e.Steps))
	}
	if len(e.Steps[0].Deps) != 0 {
		t.Fatalf("first step should have no deps, got %v", e.Steps[0].Deps)
	}
	if len(e.Steps[1].Deps) != 1 || e.Steps[1].Deps[0] != e.Steps[0].ID {
		t.Fatalf("second step should depend on the first (%s), got %v", e.Steps[0].ID, e.Steps[1].Deps)
	}
	if e.Steps[0].Kind != "run" || e.Steps[1].Kind != "run" {
		t.Fatalf("expected both steps Kind=run, got %q and %q", e.Steps[0].Kind, e.Steps[1].Kind)
	}
}

func TestExplainHandlerDepsAreItsNotifiers(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "configure", action: module.Update, order: &[]string{}}).
		Notify("restart").
		Handler("restart", orderTrackingModule{id: "restart", action: module.Update, order: &[]string{}})

	e, err := h.Explain()
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	var handler StepExplanation
	for _, s := range e.Steps {
		if s.ID == "restart" {
			handler = s
		}
	}
	if handler.Kind != "handler" {
		t.Fatalf("expected Kind=handler, got %q", handler.Kind)
	}
	if len(handler.Deps) != 1 || handler.Deps[0] != e.Steps[0].ID {
		t.Fatalf("expected restart's dep to be its notifier (%s), got %v", e.Steps[0].ID, handler.Deps)
	}
	if !strings.Contains(handler.Runs, "changes") {
		t.Fatalf("expected Runs to describe the change-gated condition, got %q", handler.Runs)
	}
}

func TestExplainFlagsOrphanHandlerAsNeverRuns(t *testing.T) {
	h := Host("web1").
		Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}}).
		Handler("unused", orderTrackingModule{id: "restart", action: module.Update, order: &[]string{}})

	e, err := h.Explain()
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	var handler StepExplanation
	for _, s := range e.Steps {
		if s.ID == "unused" {
			handler = s
		}
	}
	if len(handler.Deps) != 0 {
		t.Fatalf("expected no deps for an un-notified handler, got %v", handler.Deps)
	}
	if !strings.Contains(handler.Runs, "never") {
		t.Fatalf("expected Runs to flag this handler as never running, got %q", handler.Runs)
	}
}

func TestExplainIncludesOwnsAndReceiptsDependingOnLastStep(t *testing.T) {
	h := Host("web1").
		Run(pathedModule{path: "/etc/nginx/conf.d/a.conf"}).
		Owns("/etc/nginx/conf.d/*").
		Receipts("myapp")

	e, err := h.Explain()
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	lastRegularID := e.Steps[0].ID
	var sawOwns, sawReceipts bool
	for _, s := range e.Steps {
		switch s.ID {
		case "owns:/etc/nginx/conf.d/*":
			sawOwns = true
			if s.Kind != "owns" || len(s.Deps) != 1 || s.Deps[0] != lastRegularID {
				t.Fatalf("owns step: got %+v, want Kind=owns, Deps=[%s]", s, lastRegularID)
			}
		case "receipts:myapp":
			sawReceipts = true
			if s.Kind != "receipts" || len(s.Deps) != 1 || s.Deps[0] != lastRegularID {
				t.Fatalf("receipts step: got %+v, want Kind=receipts, Deps=[%s]", s, lastRegularID)
			}
		}
	}
	if !sawOwns || !sawReceipts {
		t.Fatalf("expected both owns and receipts in explanation, got %+v", e.Steps)
	}
}

func TestExplainStringIncludesTargetAndModuleNames(t *testing.T) {
	h := Tag("web").Run(orderTrackingModule{id: "a", action: module.Create, order: &[]string{}})

	e, err := h.Explain()
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	out := e.String()
	if !strings.Contains(out, `Tag("web")`) {
		t.Fatalf("expected target description in output, got:\n%s", out)
	}
	if !strings.Contains(out, "orderTrackingModule") {
		t.Fatalf("expected module type name in output, got:\n%s", out)
	}
}

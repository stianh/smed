package smed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stianh/smed/dag"
	"github.com/stianh/smed/module"
)

// scriptedRunner plays back one response per call and records every
// command — same pattern used throughout this repo's module tests.
type scriptedRunner struct {
	responses []string
	calls     []string
}

func (s *scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.calls = append(s.calls, cmd)
	if len(s.calls) > len(s.responses) {
		return "", errors.New("scriptedRunner: ran out of scripted responses")
	}
	return s.responses[len(s.calls)-1], nil
}

func runStep(t *testing.T, step dag.Step) dag.Result {
	t.Helper()
	g, err := dag.NewGraph([]dag.Step{step})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	results := g.Run(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 result, got %d", len(results))
	}
	return results[0]
}

func TestOwnsStepDeletesUndeclaredFiles(t *testing.T) {
	r := &scriptedRunner{responses: []string{
		"/etc/nginx/conf.d/a.conf\n/etc/nginx/conf.d/old-app.conf\n", // ls
		"", // rm
	}}
	step := ownsStep("/etc/nginx/conf.d/*", []string{"/etc/nginx/conf.d/a.conf"}, nil, r)

	result := runStep(t, step)
	if result.Status != dag.StatusOK {
		t.Fatalf("Status = %v, want StatusOK (err=%v)", result.Status, result.Err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — old-app.conf should have been deleted")
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 commands (ls, rm), got %d: %v", len(r.calls), r.calls)
	}
	if !strings.Contains(r.calls[1], "'/etc/nginx/conf.d/old-app.conf'") {
		t.Fatalf("delete command %q doesn't mention old-app.conf", r.calls[1])
	}
	if strings.Contains(r.calls[1], "a.conf'") {
		t.Fatalf("delete command %q should not touch a.conf — it's still declared", r.calls[1])
	}
}

func TestOwnsStepNoOpWhenNothingExtra(t *testing.T) {
	r := &scriptedRunner{responses: []string{
		"/etc/nginx/conf.d/a.conf\n", // ls — only what's declared
	}}
	step := ownsStep("/etc/nginx/conf.d/*", []string{"/etc/nginx/conf.d/a.conf"}, nil, r)

	result := runStep(t, step)
	if result.Changed {
		t.Fatal("expected Changed=false — nothing to delete")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected exactly 1 command (ls only, no rm), got %d: %v", len(r.calls), r.calls)
	}
}

func TestOwnsStepHandlesEmptyGlobExpansion(t *testing.T) {
	// `ls -d pattern 2>/dev/null` on a glob matching nothing prints
	// nothing — must not be mistaken for a single empty-string path.
	r := &scriptedRunner{responses: []string{""}}
	step := ownsStep("/etc/nginx/conf.d/*", nil, nil, r)

	result := runStep(t, step)
	if result.Changed {
		t.Fatal("expected Changed=false for an empty glob expansion")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected no delete command, got %v", r.calls)
	}
}

func TestReceiptStepDeletesWhatsNoLongerDeclared(t *testing.T) {
	existing, _ := json.Marshal(map[string][]string{
		"myapp": {"/etc/myapp/a.conf", "/etc/myapp/gone.conf"},
	})
	r := &scriptedRunner{responses: []string{
		string(existing), // cat receipts.json
		"",               // rm
		"",               // write updated receipts.json
	}}
	step := receiptStep("myapp", []string{"/etc/myapp/a.conf"}, nil, r)

	result := runStep(t, step)
	if result.Status != dag.StatusOK {
		t.Fatalf("Status = %v, want StatusOK (err=%v)", result.Status, result.Err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — gone.conf should have been deleted")
	}
	if !strings.Contains(r.calls[1], "'/etc/myapp/gone.conf'") {
		t.Fatalf("delete command %q doesn't mention gone.conf", r.calls[1])
	}
}

func TestReceiptStepHandlesMissingReceiptFileGracefully(t *testing.T) {
	// No history yet — a pure cache degrading gracefully, not an error.
	r := &scriptedRunner{responses: []string{
		"", // cat fails / empty, no error surfaced by `2>/dev/null`
		"", // write
	}}
	step := receiptStep("myapp", []string{"/etc/myapp/a.conf"}, nil, r)

	result := runStep(t, step)
	if result.Status != dag.StatusOK {
		t.Fatalf("Status = %v, want StatusOK (err=%v)", result.Status, result.Err)
	}
	if result.Changed {
		t.Fatal("expected Changed=false — nothing remembered yet, nothing to delete")
	}
}

func TestReceiptStepPreservesOtherKeys(t *testing.T) {
	existing, _ := json.Marshal(map[string][]string{
		"myapp":       {"/etc/myapp/a.conf"},
		"other-chain": {"/etc/other/b.conf"},
	})
	var written string
	r := &capturingWriteRunner{responses: []string{string(existing), ""}, capture: &written}
	step := receiptStep("myapp", []string{"/etc/myapp/a.conf"}, nil, r)

	runStep(t, step)

	// Decode what actually got written (the second command, base64'd
	// into the write) and confirm other-chain's entry survived
	// untouched even though this step only ever mentioned "myapp".
	idx := strings.Index(written, "echo ")
	if idx == -1 {
		t.Fatalf("write command %q doesn't look like the expected echo|base64 pattern", written)
	}
	fields := strings.Fields(written[idx:])
	if len(fields) < 2 {
		t.Fatalf("couldn't find the base64 payload in %q", written)
	}
	b64 := strings.Trim(fields[1], "'")
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got map[string][]string
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatalf("unmarshal written receipts: %v", err)
	}
	if len(got["other-chain"]) != 1 || got["other-chain"][0] != "/etc/other/b.conf" {
		t.Fatalf("other-chain's entry was not preserved: %+v", got)
	}
}

type capturingWriteRunner struct {
	responses []string
	calls     int
	capture   *string
}

func (c *capturingWriteRunner) Run(ctx context.Context, cmd string) (string, error) {
	c.calls++
	if c.calls == 2 { // the write command, after the cat
		*c.capture = cmd
	}
	if c.calls > len(c.responses) {
		return "", errors.New("capturingWriteRunner: ran out of responses")
	}
	return c.responses[c.calls-1], nil
}

func TestReconciliationStepsAssignsPathsToOwnsOverReceipts(t *testing.T) {
	h := Host("web1").
		Run(pathedModule{path: "/etc/nginx/conf.d/a.conf"}).
		Run(pathedModule{path: "/etc/myapp/scattered.conf"}).
		Owns("/etc/nginx/conf.d/*").
		Receipts("myapp")

	steps := h.reconciliationSteps(noopRunner{}, nil)
	if len(steps) != 2 {
		t.Fatalf("expected 2 reconciliation steps (owns + receipts), got %d", len(steps))
	}

	var sawOwns, sawReceipts bool
	for _, s := range steps {
		switch s.ID {
		case "owns:/etc/nginx/conf.d/*":
			sawOwns = true
		case "receipts:myapp":
			sawReceipts = true
		}
	}
	if !sawOwns || !sawReceipts {
		t.Fatalf("expected both owns and receipts steps, got IDs: %v", stepIDs(steps))
	}
}

func TestReconciliationStepsNilWhenNeitherCalled(t *testing.T) {
	h := Host("web1").Run(pathedModule{path: "/etc/nginx/conf.d/a.conf"})
	if steps := h.reconciliationSteps(noopRunner{}, nil); steps != nil {
		t.Fatalf("expected nil, got %v", steps)
	}
}

func TestPlanOwnsStepReportsDeleteWithoutDeleting(t *testing.T) {
	r := &scriptedRunner{responses: []string{
		"/etc/nginx/conf.d/a.conf\n/etc/nginx/conf.d/old-app.conf\n", // ls
	}}
	var out module.Change
	step := planOwnsStep("owns:/etc/nginx/conf.d/*", "/etc/nginx/conf.d/*", []string{"/etc/nginx/conf.d/a.conf"}, nil, r, &out)

	result := runStep(t, step)
	if result.Status != dag.StatusOK {
		t.Fatalf("Status = %v, want StatusOK (err=%v)", result.Status, result.Err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — old-app.conf would be deleted")
	}
	if out.Action != module.Delete || !strings.Contains(out.Description, "old-app.conf") {
		t.Fatalf("got out=%+v, want Action=Delete mentioning old-app.conf", out)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected only the ls command (no rm), got %d: %v", len(r.calls), r.calls)
	}
}

func TestPlanOwnsStepNoOpWhenNothingExtra(t *testing.T) {
	r := &scriptedRunner{responses: []string{
		"/etc/nginx/conf.d/a.conf\n", // ls — only what's declared
	}}
	var out module.Change
	step := planOwnsStep("owns:/etc/nginx/conf.d/*", "/etc/nginx/conf.d/*", []string{"/etc/nginx/conf.d/a.conf"}, nil, r, &out)

	result := runStep(t, step)
	if result.Changed {
		t.Fatal("expected Changed=false — nothing to delete")
	}
	if out.Action != module.NoOp {
		t.Fatalf("got out.Action=%v, want NoOp", out.Action)
	}
}

func TestPlanReceiptStepReportsDeleteWithoutWritingBack(t *testing.T) {
	existing, _ := json.Marshal(map[string][]string{
		"myapp": {"/etc/myapp/a.conf", "/etc/myapp/gone.conf"},
	})
	r := &scriptedRunner{responses: []string{
		string(existing), // cat receipts.json — the only command this should ever issue
	}}
	var out module.Change
	step := planReceiptStep("receipts:myapp", "myapp", []string{"/etc/myapp/a.conf"}, nil, r, &out)

	result := runStep(t, step)
	if result.Status != dag.StatusOK {
		t.Fatalf("Status = %v, want StatusOK (err=%v)", result.Status, result.Err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — gone.conf would be deleted")
	}
	if out.Action != module.Delete || !strings.Contains(out.Description, "gone.conf") {
		t.Fatalf("got out=%+v, want Action=Delete mentioning gone.conf", out)
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected only the cat command (no rm, no write-back), got %d: %v", len(r.calls), r.calls)
	}
}

func TestPlanReceiptStepHandlesMissingReceiptFileGracefully(t *testing.T) {
	r := &scriptedRunner{responses: []string{""}}
	var out module.Change
	step := planReceiptStep("receipts:myapp", "myapp", []string{"/etc/myapp/a.conf"}, nil, r, &out)

	result := runStep(t, step)
	if result.Changed {
		t.Fatal("expected Changed=false — nothing remembered yet, nothing to delete")
	}
	if out.Action != module.NoOp {
		t.Fatalf("got out.Action=%v, want NoOp", out.Action)
	}
}

func TestPlanReconciliationStepsBuildsOwnsAndReceiptsPreviews(t *testing.T) {
	h := Host("web1").
		Run(pathedModule{path: "/etc/nginx/conf.d/a.conf"}).
		Run(pathedModule{path: "/etc/myapp/scattered.conf"}).
		Owns("/etc/nginx/conf.d/*").
		Receipts("myapp")

	changes := map[string]*module.Change{}
	steps := h.planReconciliationSteps(noopRunner{}, nil, changes)
	if len(steps) != 2 {
		t.Fatalf("expected 2 reconciliation steps (owns + receipts), got %d", len(steps))
	}

	var sawOwns, sawReceipts bool
	for _, s := range steps {
		switch s.ID {
		case "owns:/etc/nginx/conf.d/*":
			sawOwns = true
		case "receipts:myapp":
			sawReceipts = true
		}
	}
	if !sawOwns || !sawReceipts {
		t.Fatalf("expected both owns and receipts preview steps, got IDs: %v", stepIDs(steps))
	}
	if _, ok := changes["owns:/etc/nginx/conf.d/*"]; !ok {
		t.Fatal("expected changes to have an entry for the owns step")
	}
	if _, ok := changes["receipts:myapp"]; !ok {
		t.Fatal("expected changes to have an entry for the receipts step")
	}
}

func TestPlanReconciliationStepsNilWhenNeitherCalled(t *testing.T) {
	h := Host("web1").Run(pathedModule{path: "/etc/nginx/conf.d/a.conf"})
	if steps := h.planReconciliationSteps(noopRunner{}, nil, map[string]*module.Change{}); steps != nil {
		t.Fatalf("expected nil, got %v", steps)
	}
}

func stepIDs(steps []dag.Step) []string {
	ids := make([]string, len(steps))
	for i, s := range steps {
		ids[i] = s.ID
	}
	return ids
}

// pathedModule is a minimal module.Module + module.Pathed for testing
// reconciliation wiring without any real filesystem or shell at all.
type pathedModule struct{ path string }

func (m pathedModule) Desired() module.State { return nil }
func (m pathedModule) Read(ctx context.Context, r module.Runner) (module.State, error) {
	return nil, nil
}
func (m pathedModule) Diff(desired, current module.State) (module.Change, error) {
	return module.Change{Action: module.NoOp}, nil
}
func (m pathedModule) Apply(ctx context.Context, r module.Runner, change module.Change) (module.Result, error) {
	return module.Result{}, nil
}
func (m pathedModule) Verify(ctx context.Context, r module.Runner, desired module.State) error {
	return nil
}
func (m pathedModule) FilePath() string { return m.path }

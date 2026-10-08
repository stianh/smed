package ufw

import (
	"context"
	"strings"
	"testing"

	"github.com/stianh/smed/module"
)

// realStatusOutput is `ufw status` captured verbatim from a real host
// (db-t-01) that's actually running the config this package was built
// against — not a hand-constructed fixture. Trailing spaces on some
// lines are real; ufw pads columns.
const realStatusOutput = `Status: active

To                         Action      From
--                         ------      ----
22/tcp (OpenSSH)           ALLOW IN    Anywhere
5432/tcp                   ALLOW IN    10.10.0.0/16
22/tcp (OpenSSH (v6))      ALLOW IN    Anywhere (v6)             `

const realVerboseOutput = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp (OpenSSH)           ALLOW IN    Anywhere
5432/tcp                   ALLOW IN    10.10.0.0/16
22/tcp (OpenSSH (v6))      ALLOW IN    Anywhere (v6)             `

type scriptedRunner struct{ out string }

func (s scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	return s.out, nil
}

type capturingRunner struct {
	out        string
	gotCommand string
}

func (c *capturingRunner) Run(ctx context.Context, cmd string) (string, error) {
	c.gotCommand = cmd
	return c.out, nil
}

func TestRuleReadFindsExistingAppProfileRule(t *testing.T) {
	r := Rule{Allow: true, App: "OpenSSH"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.(ruleState).Exists {
		t.Fatal("expected Exists=true for the already-present OpenSSH rule")
	}
}

func TestRuleReadFindsExistingPortRule(t *testing.T) {
	r := Rule{Allow: true, Port: "5432", Proto: "tcp", From: "10.10.0.0/16"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.(ruleState).Exists {
		t.Fatal("expected Exists=true for the already-present 5432/tcp rule")
	}
}

func TestRuleReadMissesRuleWithDifferentSource(t *testing.T) {
	// Same port, different CIDR — must not match.
	r := Rule{Allow: true, Port: "5432", Proto: "tcp", From: "192.168.1.0/24"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(ruleState).Exists {
		t.Fatal("expected Exists=false — the real rule is from a different CIDR")
	}
}

func TestRuleReadMissesAbsentRule(t *testing.T) {
	r := Rule{Allow: true, Port: "9999", Proto: "tcp"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(ruleState).Exists {
		t.Fatal("expected Exists=false for a port with no rule at all")
	}
}

func TestRuleReadFindsFromToStyleRuleWithNoDirectionSuffix(t *testing.T) {
	// Captured for real against db-t-01: a rule added via
	// "ufw allow from X to any port Y proto Z" (what Apply uses
	// whenever From is set) displays as plain "ALLOW" with no " IN"
	// suffix — unlike the plain port/proto form, which does print
	// "ALLOW IN". A matcher requiring the literal "ALLOW IN" string
	// would never recognize a rule this module's own Apply had just
	// created — exactly the bug this regression test is named for.
	const out = `Status: active

To                         Action      From
--                         ------      ----
19999/tcp                  ALLOW       192.0.2.0/24              `

	r := Rule{Allow: true, Port: "19999", Proto: "tcp", From: "192.0.2.0/24"}
	got, err := r.Read(context.Background(), scriptedRunner{out: out})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.(ruleState).Exists {
		t.Fatal("expected Exists=true — this exact line is how ufw really displays a from/to rule")
	}
}

func TestRuleReadDistinguishesAllowFromDeny(t *testing.T) {
	// The real output only has ALLOW IN rules; a DENY rule for the
	// same port must not match just because the port text matches.
	r := Rule{Allow: false, Port: "5432", Proto: "tcp", From: "10.10.0.0/16"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(ruleState).Exists {
		t.Fatal("expected Exists=false — the real rule is ALLOW, not DENY")
	}
}

func TestRuleDiffNoOpWhenExists(t *testing.T) {
	r := Rule{Allow: true, Port: "5432", Proto: "tcp"}
	change, err := r.Diff(r.Desired(), ruleState{Exists: true})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestRuleDiffCreateWhenMissing(t *testing.T) {
	r := Rule{Allow: true, Port: "5432", Proto: "tcp"}
	change, err := r.Diff(r.Desired(), ruleState{Exists: false})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Create {
		t.Fatalf("Action = %v, want Create", change.Action)
	}
}

func TestRuleApplyBuildsPortRuleWithSource(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: true, Port: "5432", Proto: "tcp", From: "10.10.0.0/16"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "ufw allow from '10.10.0.0/16' to any port '5432' proto 'tcp' 2>&1"
	if r.gotCommand != want {
		t.Fatalf("got command %q, want %q", r.gotCommand, want)
	}
}

func TestRuleApplyBuildsPlainPortRule(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: true, Port: "22", Proto: "tcp"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(r.gotCommand, "ufw allow '22/tcp'") {
		t.Fatalf("got command %q, want it to start with \"ufw allow '22/tcp'\"", r.gotCommand)
	}
}

func TestRuleApplyBuildsAppProfileRule(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: true, App: "OpenSSH"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.gotCommand != "ufw allow 'OpenSSH' 2>&1" {
		t.Fatalf("got command %q", r.gotCommand)
	}
}

func TestRuleApplyDeny(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: false, Port: "8080", Proto: "tcp"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(r.gotCommand, "ufw deny") {
		t.Fatalf("got command %q, want it to start with 'ufw deny'", r.gotCommand)
	}
}

func TestDefaultReadParsesRealVerboseOutput(t *testing.T) {
	d := Default{}
	got, err := d.Read(context.Background(), scriptedRunner{out: realVerboseOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := defaultState{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "allow"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDefaultReadParsesInactive(t *testing.T) {
	d := Default{}
	got, err := d.Read(context.Background(), scriptedRunner{out: "Status: inactive\n"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(defaultState).Enabled {
		t.Fatal("expected Enabled=false for Status: inactive")
	}
}

func TestDefaultDiffNoOpWhenMatching(t *testing.T) {
	d := Default{Enabled: true, IncomingPolicy: "deny"}
	current := defaultState{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "allow"}
	change, err := d.Diff(d.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestDefaultDiffIgnoresUnmanagedOutgoingPolicy(t *testing.T) {
	d := Default{Enabled: true, IncomingPolicy: "deny"} // OutgoingPolicy unmanaged
	current := defaultState{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "reject"}
	change, err := d.Diff(d.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (outgoing policy unmanaged)", change.Action)
	}
}

func TestDefaultApplySetsPolicyBeforeEnabling(t *testing.T) {
	r := &capturingRunner{}
	d := Default{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "allow"}
	if _, err := d.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	policyIdx := strings.Index(r.gotCommand, "ufw default 'deny' incoming")
	enableIdx := strings.Index(r.gotCommand, "ufw --force enable")
	if policyIdx == -1 || enableIdx == -1 || policyIdx > enableIdx {
		t.Fatalf("got command %q, want policy set before enabling", r.gotCommand)
	}
}

func TestDefaultApplyDisableUsesForce(t *testing.T) {
	r := &capturingRunner{}
	d := Default{Enabled: false}
	if _, err := d.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(r.gotCommand, "ufw --force disable") {
		t.Fatalf("got command %q, want 'ufw --force disable'", r.gotCommand)
	}
}

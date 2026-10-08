// Package onepassword is smed's default secret.Resolver: 1Password,
// via the local `op` CLI rather than the Go SDK — one less dependency,
// and it's the exact same tool a developer already has authenticated
// on their machine, or a CI runner already has a service-account
// token for. `op` itself picks between a personal session (already
// unlocked — the common case, running smed from a developer machine)
// and OP_SERVICE_ACCOUNT_TOKEN in the environment (the CI case);
// Resolver doesn't need to know which.
//
// This runs on the control side, as a local subprocess — never
// through run.Runner. Resolving a secret is something the machine
// running smed does before it ever talks to a target, not something a
// managed host does on smed's behalf.
package onepassword

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/stianh/smed/secret"
)

// Resolver resolves secret.Refs shaped like "op://vault/item/field"
// via `op read`.
type Resolver struct{}

func (Resolver) Resolve(ctx context.Context, ref secret.Ref) (string, error) {
	if !strings.HasPrefix(string(ref), "op://") {
		return "", fmt.Errorf("onepassword: ref %q does not start with op://", ref)
	}

	cmd := exec.CommandContext(ctx, "op", "read", string(ref))
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("op read %s: %s", ref, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("op read %s: %w", ref, err)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

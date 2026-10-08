package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/stianh/smed/shellquote"
)

// Runner is the same one-method shape module.Runner expects, defined
// again here rather than imported from that package: it avoids a
// cross-package dependency over a single-method interface, and Go
// satisfies it structurally either way — anything satisfying this also
// satisfies module.Runner with zero adapter code, exactly like *Session
// already does.
type Runner interface {
	Run(ctx context.Context, command string) (output string, err error)
}

// Become wraps a Runner so every command runs via sudo as TargetUser
// (root if empty) instead of directly as whoever the connection
// authenticated as. Since *Become itself has the same Run method, it
// satisfies Runner (and so module.Runner) too — a module takes a
// *Become exactly the way it takes a *Session, no changes anywhere else.
//
// Only passwordless (NOPASSWD) sudo is implemented, via `sudo -n`: it
// fails immediately and clearly if NOPASSWD isn't configured, rather
// than hanging waiting for a password that will never arrive over this
// channel. Password-based sudo needs a way to feed stdin to the remote
// process, which this Runner shape doesn't expose — the secret package
// (see README.md, "Secrets") can resolve the password, but there's
// still nowhere to pipe it in. Deferred on purpose, not half-built; see
// README.md's "Explicitly out of scope, or deferred on purpose."
type Become struct {
	Inner      Runner
	TargetUser string // "" means root
}

func (b *Become) Run(ctx context.Context, command string) (string, error) {
	sudo := "sudo -n "
	if b.TargetUser != "" && b.TargetUser != "root" {
		sudo += fmt.Sprintf("-u %s ", shellquote.Quote(b.TargetUser))
	}
	sudo += fmt.Sprintf("sh -c %s", shellquote.Quote(command))

	out, err := b.Inner.Run(ctx, sudo)
	if err != nil && looksLikeNoPasswordlessSudo(out) {
		return out, fmt.Errorf("sudo requires a password (NOPASSWD not configured for this user/command): %w", err)
	}
	return out, err
}

// looksLikeNoPasswordlessSudo recognizes sudo's own wording for "I'd
// need a password and there's no way to prompt for one here" — distinct
// from the command itself failing — so Become.Run can say exactly what
// went wrong instead of a generic "command failed".
func looksLikeNoPasswordlessSudo(out string) bool {
	return strings.Contains(out, "a password is required") ||
		strings.Contains(out, "no tty present") ||
		strings.Contains(out, "a terminal is required")
}

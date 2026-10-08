// Package secret is how a value gets into a module without ever
// sitting in the module's own fields as plaintext. A Ref is just a
// URI — "op://vault/item/field" for the default 1Password backend —
// and a Resolver turns one into its real value, lazily, only when
// something actually needs it.
//
// This package resolves secrets on the control side, before anything
// is sent to a target — never through run.Runner, which is for
// executing on a managed host. A managed host never resolves its own
// secrets; it only ever receives the plaintext smed already fetched,
// embedded in whatever command needed it.
package secret

import "context"

// Ref is an unresolved reference to a secret. The scheme (everything
// before "://") says which backend it's for — "op" for 1Password,
// whatever a given Resolver recognizes for others — but Ref itself
// doesn't parse or validate that; it's just the URI a Resolver knows
// how to read.
type Ref string

// Resolver turns a Ref into its real value. One method, same pattern
// as tailnet.Tailnet and module.Runner: a new secret store is a new
// implementation of this, nothing else in smed needs to change.
type Resolver interface {
	Resolve(ctx context.Context, ref Ref) (string, error)
}

// Lazy wraps a Resolver and a Ref into a closure that only calls
// Resolve when actually invoked. A module field holds this — never a
// Ref or a plaintext string directly — so the real value doesn't exist
// anywhere until the one line of code that needs it calls the
// closure, and there's nothing holding a secret that could
// accidentally end up printed in a Diff description, a log line, or a
// plan artifact: a func value isn't printable the way a string is.
func Lazy(r Resolver, ref Ref) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		return r.Resolve(ctx, ref)
	}
}

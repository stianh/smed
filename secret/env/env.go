// Package env is the second secret.Resolver, existing mainly to prove
// pluggability actually holds: a Ref is just the environment variable
// name, and nothing about secret.Ref, secret.Lazy, or anything that
// consumes a resolved secret needs to know this backend exists.
package env

import (
	"context"
	"fmt"
	"os"

	"github.com/stianh/smed/secret"
)

// Resolver resolves a secret.Ref by reading the environment variable
// named by the ref itself: secret.Ref("MY_API_KEY") reads
// os.Getenv("MY_API_KEY").
type Resolver struct{}

func (Resolver) Resolve(ctx context.Context, ref secret.Ref) (string, error) {
	val, ok := os.LookupEnv(string(ref))
	if !ok {
		return "", fmt.Errorf("env: variable %q is not set", ref)
	}
	return val, nil
}

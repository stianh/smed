package onepassword

import (
	"context"
	"testing"
)

// TestResolveRejectsNonOPRef is the one thing worth unit-testing here
// without a real `op` session: everything else in this package talks
// to a real external CLI and real account state, which doesn't belong
// in an automated test that anyone might run without 1Password
// configured. Live resolution is validated separately, against a real
// reference, without ever printing the value.
func TestResolveRejectsNonOPRef(t *testing.T) {
	_, err := Resolver{}.Resolve(context.Background(), "env://SOMETHING")
	if err == nil {
		t.Fatal("expected an error for a ref that isn't an op:// URI")
	}
}

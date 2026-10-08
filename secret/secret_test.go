package secret

import (
	"context"
	"errors"
	"testing"
)

type fakeResolver struct {
	calls  int
	gotRef Ref
	value  string
	err    error
}

func (f *fakeResolver) Resolve(ctx context.Context, ref Ref) (string, error) {
	f.calls++
	f.gotRef = ref
	return f.value, f.err
}

func TestLazyDoesNotResolveUntilInvoked(t *testing.T) {
	r := &fakeResolver{value: "s3cr3t"}
	fn := Lazy(r, "op://vault/item/field")

	if r.calls != 0 {
		t.Fatalf("Resolve should not have been called yet, got %d calls", r.calls)
	}

	got, err := fn(context.Background())
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("got %q, want %q", got, "s3cr3t")
	}
	if r.calls != 1 {
		t.Fatalf("expected exactly 1 call, got %d", r.calls)
	}
	if r.gotRef != "op://vault/item/field" {
		t.Fatalf("got ref %q, want the exact Ref passed to Lazy", r.gotRef)
	}
}

func TestLazyPropagatesResolverError(t *testing.T) {
	r := &fakeResolver{err: errors.New("vault locked")}
	fn := Lazy(r, "op://vault/item/field")

	_, err := fn(context.Background())
	if err == nil {
		t.Fatal("expected the resolver's error to propagate")
	}
}

func TestLazyCallsResolveAgainOnEachInvocation(t *testing.T) {
	// Lazy defers resolution; it doesn't cache it. Calling the
	// returned func twice resolves twice — correct for a value that
	// might rotate between calls, and simple: no cache invalidation
	// to get wrong.
	r := &fakeResolver{value: "s3cr3t"}
	fn := Lazy(r, "op://vault/item/field")

	fn(context.Background())
	fn(context.Background())

	if r.calls != 2 {
		t.Fatalf("expected 2 calls, got %d", r.calls)
	}
}

package env

import (
	"context"
	"testing"
)

func TestResolveReadsEnvVar(t *testing.T) {
	t.Setenv("SMED_TEST_SECRET", "s3cr3t")

	got, err := Resolver{}.Resolve(context.Background(), "SMED_TEST_SECRET")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("got %q, want %q", got, "s3cr3t")
	}
}

func TestResolveErrorsOnMissingVar(t *testing.T) {
	_, err := Resolver{}.Resolve(context.Background(), "SMED_TEST_DEFINITELY_UNSET")
	if err == nil {
		t.Fatal("expected an error for an unset variable")
	}
}

func TestResolveDistinguishesUnsetFromEmpty(t *testing.T) {
	// A variable explicitly set to "" is a real, deliberate empty
	// value, not "unset" — os.LookupEnv (not os.Getenv) is what makes
	// that distinction possible here.
	t.Setenv("SMED_TEST_EMPTY", "")

	got, err := Resolver{}.Resolve(context.Background(), "SMED_TEST_EMPTY")
	if err != nil {
		t.Fatalf("Resolve: %v, want no error for an explicitly empty value", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

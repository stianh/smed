package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stianh/smed/module"
)

func TestDirectoryCreatesMissingDir(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "nested", "dir")

	d := Directory{Path: path, Mode: "0755"}
	result, err := module.RunModule(context.Background(), d, localRunner{})
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true creating a missing directory")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected a directory")
	}
	if perm := info.Mode().Perm(); perm != 0755 {
		t.Fatalf("mode = %o, want 0755", perm)
	}
}

func TestDirectoryIsIdempotent(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "dir")
	d := Directory{Path: path, Mode: "0755"}
	ctx := context.Background()

	first, err := module.RunModule(ctx, d, localRunner{})
	if err != nil {
		t.Fatalf("first RunModule: %v", err)
	}
	if !first.Changed {
		t.Fatal("first run: expected Changed=true")
	}

	second, err := module.RunModule(ctx, d, localRunner{})
	if err != nil {
		t.Fatalf("second RunModule: %v", err)
	}
	if second.Changed {
		t.Fatal("second run: expected Changed=false")
	}
}

func TestDirectoryAppliesOwnershipToSelf(t *testing.T) {
	user, group := currentUserAndGroup(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "owned")

	d := Directory{Path: path, Owner: user, Group: group}
	if _, err := module.RunModule(context.Background(), d, localRunner{}); err != nil {
		t.Fatalf("RunModule: %v", err) // includes Verify — proves the chown command is well-formed
	}
}

func TestDirectoryDiffDetectsOwnerAndGroupMismatch(t *testing.T) {
	d := Directory{Path: "/irrelevant", Owner: "deploy", Group: "deploy"}

	cases := []struct {
		name    string
		current dirState
		want    module.Action
	}{
		{"owner differs", dirState{Exists: true, Owner: "root", Group: "deploy"}, module.Update},
		{"group differs", dirState{Exists: true, Owner: "deploy", Group: "root"}, module.Update},
		{"both match", dirState{Exists: true, Owner: "deploy", Group: "deploy"}, module.NoOp},
		{"missing entirely", dirState{Exists: false}, module.Create},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			change, err := d.Diff(d.Desired(), tc.current)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if change.Action != tc.want {
				t.Fatalf("Action = %v, want %v", change.Action, tc.want)
			}
		})
	}
}

func TestDirectoryDiffIgnoresUnmanagedAttributes(t *testing.T) {
	d := Directory{Path: "/irrelevant"} // no Owner, Group, or Mode managed
	current := dirState{Exists: true, Owner: "root", Group: "root", Mode: "755"}

	change, err := d.Diff(d.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (nothing managed)", change.Action)
	}
}

func TestDirectoryModeComparisonIgnoresLeadingZero(t *testing.T) {
	d := Directory{Path: "/irrelevant", Mode: "0755"}
	current := dirState{Exists: true, Mode: "755"}

	change, err := d.Diff(d.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (0755 and 755 are the same mode)", change.Action)
	}
}

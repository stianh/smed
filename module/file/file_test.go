package file

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/stianh/smed/module"
)

// currentUserAndGroup returns the real local user/group running the test,
// for the one ownership test that can run without root: chowning a file
// to its own already-correct owner. That proves the generated `chown`
// command is well-formed and executes cleanly; it does NOT prove
// changing ownership to a *different* user, which this plain local
// runner has no privilege for (run.Become covers that for a real
// session; this test harness doesn't use one).
func currentUserAndGroup(t *testing.T) (string, string) {
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot determine current user: %v", err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skipf("cannot determine current group: %v", err)
	}
	return u.Username, g.Name
}

// localRunner runs commands against the real local shell — not a mock of
// shell semantics, the real thing, just over os/exec instead of SSH. The
// commands Content emits are plain POSIX shell either way; this is
// exactly what runs for real against a remote host via *run.Session,
// minus the network.
type localRunner struct{}

func (localRunner) Run(ctx context.Context, cmd string) (string, error) {
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	return string(out), err
}

func TestContentCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nginx.conf")

	c := Content{Path: path, Data: "server {}\n", Mode: "0644"}
	result, err := module.RunModule(context.Background(), c, localRunner{})
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true creating a missing file")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != c.Data {
		t.Fatalf("file content = %q, want %q", got, c.Data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Fatalf("file mode = %o, want 0644", perm)
	}
}

func TestContentIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nginx.conf")
	c := Content{Path: path, Data: "server {}\n", Mode: "0644"}

	ctx := context.Background()
	first, err := module.RunModule(ctx, c, localRunner{})
	if err != nil {
		t.Fatalf("first RunModule: %v", err)
	}
	if !first.Changed {
		t.Fatal("first run: expected Changed=true")
	}

	second, err := module.RunModule(ctx, c, localRunner{})
	if err != nil {
		t.Fatalf("second RunModule: %v", err)
	}
	if second.Changed {
		t.Fatal("second run: expected Changed=false — nothing should differ")
	}
}

func TestContentUpdatesChangedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nginx.conf")
	ctx := context.Background()

	if _, err := module.RunModule(ctx, Content{Path: path, Data: "v1\n"}, localRunner{}); err != nil {
		t.Fatalf("initial RunModule: %v", err)
	}

	result, err := module.RunModule(ctx, Content{Path: path, Data: "v2\n"}, localRunner{})
	if err != nil {
		t.Fatalf("update RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true when content differs")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "v2\n" {
		t.Fatalf("file content = %q, want %q", got, "v2\n")
	}
}

func TestContentHandlesContentWithSpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weird.conf")
	// Quotes, a dollar sign, backticks, newlines — the exact kind of
	// content a naive "interpolate it into the shell command" approach
	// would mangle. Content must survive this intact via base64.
	data := "it's a \"test\" with $vars `and backticks`\nline two\n"

	c := Content{Path: path, Data: data}
	if _, err := module.RunModule(context.Background(), c, localRunner{}); err != nil {
		t.Fatalf("RunModule: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != data {
		t.Fatalf("file content = %q, want %q", got, data)
	}
}

func TestContentModeComparisonIgnoresLeadingZero(t *testing.T) {
	// `stat -c %a` reports "644", never "0644" — Diff must treat those
	// as equal, or every run would report a change and Verify would
	// fail right after every single Apply.
	dir := t.TempDir()
	path := filepath.Join(dir, "mode.conf")
	c := Content{Path: path, Data: "x\n", Mode: "0644"}
	ctx := context.Background()

	if _, err := module.RunModule(ctx, c, localRunner{}); err != nil {
		t.Fatalf("RunModule: %v", err)
	}

	current, err := c.Read(ctx, localRunner{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	change, err := c.Diff(c.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (0644 and 644 are the same mode)", change.Action)
	}
}

func TestContentAppliesOwnershipToSelf(t *testing.T) {
	user, group := currentUserAndGroup(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "owned.conf")

	c := Content{Path: path, Data: "x\n", Owner: user, Group: group}
	result, err := module.RunModule(context.Background(), c, localRunner{})
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true creating a missing file")
	}
	// RunModule already calls Verify internally and would have failed
	// if the chown command were malformed or the result didn't match —
	// reaching here with no error is itself the proof the generated
	// `chown user:group path` command is well-formed and ran cleanly.
}

func TestContentDiffDetectsOwnerAndGroupMismatch(t *testing.T) {
	c := Content{Path: "/irrelevant", Data: "x", Owner: "deploy", Group: "deploy"}

	cases := []struct {
		name    string
		current contentState
		want    module.Action
	}{
		{"owner differs", contentState{Exists: true, Data: "x", Owner: "root", Group: "deploy"}, module.Update},
		{"group differs", contentState{Exists: true, Data: "x", Owner: "deploy", Group: "root"}, module.Update},
		{"both match", contentState{Exists: true, Data: "x", Owner: "deploy", Group: "deploy"}, module.NoOp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			change, err := c.Diff(c.Desired(), tc.current)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if change.Action != tc.want {
				t.Fatalf("Action = %v, want %v", change.Action, tc.want)
			}
		})
	}
}

func TestContentDiffIgnoresUnmanagedOwnerAndGroup(t *testing.T) {
	// Owner/Group left empty on the desired side means "don't manage
	// this attribute" — a real-world file owned by root:root shouldn't
	// be reported as needing a change just because Content didn't ask.
	c := Content{Path: "/irrelevant", Data: "x"} // Owner, Group both ""
	current := contentState{Exists: true, Data: "x", Owner: "root", Group: "root", Mode: "644"}

	change, err := c.Diff(c.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (owner/group unmanaged)", change.Action)
	}
}

func TestContentDiffReportsNoOpForMatchingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "same.conf")
	c := Content{Path: path, Data: "identical\n"}
	ctx := context.Background()
	r := localRunner{}

	if _, err := module.RunModule(ctx, c, r); err != nil {
		t.Fatalf("initial RunModule: %v", err)
	}

	current, err := c.Read(ctx, r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	change, err := c.Diff(c.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

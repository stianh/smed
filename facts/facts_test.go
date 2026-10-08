package facts

import (
	"context"
	"errors"
	"testing"
)

type scriptedRunner struct {
	out string
	err error
}

func (s scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	return s.out, s.err
}

func TestGatherParsesArchAndOSRelease(t *testing.T) {
	r := scriptedRunner{out: "amd64\n" + sep + "\n" +
		`PRETTY_NAME="Ubuntu 24.04.1 LTS"` + "\n" +
		`NAME="Ubuntu"` + "\n" +
		`VERSION_ID="24.04"` + "\n" +
		`VERSION_CODENAME=noble` + "\n" +
		`ID=ubuntu` + "\n" +
		`ID_LIKE=debian` + "\n",
	}

	got, err := Gather(context.Background(), r)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	want := Facts{
		Arch: "amd64",
		OSRelease: OSRelease{
			ID:              "ubuntu",
			VersionID:       "24.04",
			VersionCodename: "noble",
			Name:            "Ubuntu",
			PrettyName:      "Ubuntu 24.04.1 LTS",
		},
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestGatherHandlesUnquotedValues(t *testing.T) {
	// Some distros don't quote simple values at all — VERSION_CODENAME
	// above is already an example; this checks ID specifically too,
	// since it's the field most composed code will branch on.
	r := scriptedRunner{out: "arm64\n" + sep + "\nID=debian\nVERSION_CODENAME=bookworm\n"}
	got, err := Gather(context.Background(), r)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if got.Arch != "arm64" || got.OSRelease.ID != "debian" || got.OSRelease.VersionCodename != "bookworm" {
		t.Fatalf("got %+v, want arm64/debian/bookworm", got)
	}
}

func TestGatherPropagatesRealFailure(t *testing.T) {
	// Unlike most Read methods elsewhere in this codebase, there's no
	// "normal missing state" for facts — no dpkg, no os-release, is a
	// genuine error, not something to collapse into a zero value.
	r := scriptedRunner{out: "dpkg: command not found\n", err: errors.New("exit status 127")}
	_, err := Gather(context.Background(), r)
	if err == nil {
		t.Fatal("expected an error when the underlying command fails")
	}
}

func TestGatherErrorsOnUnexpectedOutput(t *testing.T) {
	r := scriptedRunner{out: "amd64\nno separator here\n"}
	_, err := Gather(context.Background(), r)
	if err == nil {
		t.Fatal("expected an error when the separator is missing from output")
	}
}

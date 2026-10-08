// Package facts is read-only target introspection: things a composing
// program needs to know *before* it can build the right modules, not
// something with a desired state to converge toward — so this is
// intentionally not a module.Module. The motivating case: building an
// apt repository source line (e.g. Docker's own install instructions)
// needs the target's architecture and distro codename baked into the
// line's text before a file.Content step can even be constructed.
//
// This is the smallest useful slice of what Ansible calls "facts", not
// a general-purpose gathering system — add more here only when a real
// composition actually needs it, same as every module in this repo.
package facts

import (
	"context"
	"fmt"
	"strings"
)

// Runner is the same one-method shape module.Runner expects, defined
// again here rather than imported — avoids a cross-package dependency
// over a single-method interface, and Go satisfies it structurally
// either way, exactly like run.Session already does for module.Runner.
type Runner interface {
	Run(ctx context.Context, command string) (output string, err error)
}

// OSRelease is the subset of /etc/os-release this package parses —
// present on effectively every modern Linux distro, not apt-specific,
// even though Arch (DpkgArch) is.
type OSRelease struct {
	ID              string // "ubuntu"
	VersionID       string // "24.04"
	VersionCodename string // "noble"
	Name            string // "Ubuntu"
	PrettyName      string // "Ubuntu 24.04.1 LTS"
}

// Facts is everything Gather collects in one round trip.
type Facts struct {
	// Arch is dpkg's own architecture name ("amd64", "arm64", ...), not
	// uname's ("x86_64", "aarch64", ...) — deliberately: the only
	// reason this exists is to build apt source lines, which use
	// dpkg's naming, so returning uname's instead would just be wrong
	// for the one thing this is for.
	Arch      string
	OSRelease OSRelease
}

const sep = "---SMED-SEP---"

// Gather reads architecture and /etc/os-release in one round trip. A
// real failure here (no dpkg, no /etc/os-release) is a genuine error —
// unlike most Read methods in this codebase, there's no sensible
// "normal missing state" to collapse it into; a target facts can't be
// gathered from isn't one smed can act on correctly either.
func Gather(ctx context.Context, r Runner) (Facts, error) {
	cmd := fmt.Sprintf("dpkg --print-architecture 2>&1; echo %s; cat /etc/os-release 2>&1", sep)
	out, err := r.Run(ctx, cmd)
	if err != nil {
		return Facts{}, fmt.Errorf("gather facts: %w", err)
	}

	parts := strings.SplitN(out, sep, 2)
	if len(parts) != 2 {
		return Facts{}, fmt.Errorf("gather facts: unexpected output %q", out)
	}
	arch := strings.TrimSpace(parts[0])
	kv := parseOSRelease(parts[1])

	return Facts{
		Arch: arch,
		OSRelease: OSRelease{
			ID:              kv["ID"],
			VersionID:       kv["VERSION_ID"],
			VersionCodename: kv["VERSION_CODENAME"],
			Name:            kv["NAME"],
			PrettyName:      kv["PRETTY_NAME"],
		},
	}, nil
}

// parseOSRelease handles the KEY=VALUE shell-ish format of
// /etc/os-release: values are sometimes double-quoted, sometimes not,
// and this strips at most one matching pair of quotes — not a general
// shell-quoting parser, just enough for this one well-known file format.
func parseOSRelease(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		out[key] = strings.Trim(value, `"`)
	}
	return out
}

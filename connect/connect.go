// Package connect answers two questions Tailscale deliberately doesn't:
// *who* to SSH in as, and how to merge non-Tailscale hosts into the same
// inventory model. See README.md, "Tailscale is the backend, not a
// plugin."
package connect

import (
	"fmt"
	"strings"

	"github.com/stianh/smed/tailnet"
)

// Rule is one entry in the connection-rule table. Exactly one of MatchHost
// or MatchTag should be set. Rules are checked in two passes — all
// MatchHost rules first, then all MatchTag rules in order — so a
// host-specific exception always wins over a tag-level default regardless
// of where either happens to sit in the slice.
type Rule struct {
	MatchHost string
	MatchTag  string
	User      string
}

// Target is one thing smed can connect to, regardless of which inventory
// source it came from. Exactly one of Peer or Addr is set, carrying enough
// to dial it; Hostname and Tags are what connection rules match against,
// uniformly, regardless of which source a Target came from.
type Target struct {
	Hostname string
	Tags     []string
	Peer     *tailnet.Peer // set if this target came from Tailscale
	Addr     string        // set ("host:port") if this target is a StaticHost
}

// StaticHost is a non-Tailscale target, declared by hand. It folds into the
// same Target/rule-matching model as Tailscale peers rather than being a
// second inventory system.
type StaticHost struct {
	Hostname string
	Addr     string // host:port
	Tags     []string
}

// Merge combines both inventory sources into one tagged list: Tailscale
// peers (tags stripped of their "tag:" prefix — tag:web becomes the web
// you select with smed.Tag("web"), see README.md, "Tailscale is the
// backend, not a plugin") and hand-declared static hosts. Connection
// rules and -tag style filtering apply to the result uniformly; neither
// cares which source a Target came from.
func Merge(peers []tailnet.Peer, static []StaticHost) []Target {
	var out []Target
	for _, p := range peers {
		p := p
		out = append(out, Target{Hostname: p.Hostname, Tags: stripTagPrefix(p.Tags), Peer: &p})
	}
	for _, h := range static {
		out = append(out, Target{Hostname: h.Hostname, Tags: h.Tags, Addr: h.Addr})
	}
	return out
}

func stripTagPrefix(tags []string) []string {
	if tags == nil {
		return nil
	}
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = strings.TrimPrefix(t, "tag:")
	}
	return out
}

// Resolve finds the connecting user for a target: host-rule match, else
// tag-rule match (first match in table order wins a tie), else an error.
// override, if non-empty, wins outright over the table — the CLI's -user
// flag is exactly that override, one level more specific than even a
// host-rule match, because it's an explicit operator decision made right
// now rather than a standing table entry.
func Resolve(rules []Rule, target Target, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	for _, r := range rules {
		if r.MatchHost != "" && r.MatchHost == target.Hostname {
			return r.User, nil
		}
	}
	for _, r := range rules {
		if r.MatchTag == "" {
			continue
		}
		for _, t := range target.Tags {
			if t == r.MatchTag {
				return r.User, nil
			}
		}
	}

	return "", fmt.Errorf("no connection rule matches host %q (tags %v) and no -user override given", target.Hostname, target.Tags)
}

// HasTag reports whether t carries the given tag — used for -tag filtering
// at the CLI, and shares the exact matching semantics Resolve uses.
func (t Target) HasTag(tag string) bool {
	for _, x := range t.Tags {
		if x == tag {
			return true
		}
	}
	return false
}

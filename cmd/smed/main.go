// smed — the installable CLI. Deliberately thin: it never carries your
// connection rules, your static hosts, or anything else that's your
// config, because a globally-installed binary can't contain a separately
// compiled Go program. Your project imports smed as a library (tailnet,
// connect, run) and calls into it from your own main.go; this binary is
// scaffolding and config-free utilities only. See README.md, "smed is
// a library, not a framework with an installed binary that runs your
// config."
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/stianh/smed/tailnet"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch os.Args[1] {
	case "inventory":
		fs := flag.NewFlagSet("inventory", flag.ExitOnError)
		byTag := fs.Bool("by-tag", false, "group by tag instead of listing one row per host")
		fs.Parse(os.Args[2:])

		tn := tailnet.LocalDaemon()
		var err error
		if *byTag {
			err = cmdInventoryByTag(ctx, tn)
		} else {
			err = cmdInventory(ctx, tn)
		}
		if err != nil {
			fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  smed inventory          # real tailnet inventory as a table, no project needed")
	fmt.Fprintln(os.Stderr, "  smed inventory -by-tag  # grouped by tag — what smed.Tag(x) would actually hit")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "there is no 'smed run'/'smed plan'/'smed apply' here on purpose:")
	fmt.Fprintln(os.Stderr, "those need your connection rules and your targets, which live in")
	fmt.Fprintln(os.Stderr, "your own Go project that imports smed — see smed-example.")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// cmdInventory lists real tailnet inventory as a table: every peer
// except the control node itself, which is never part of inventory —
// shown separately, above the table, so "which machine am I running
// this from" stays visible without being mistaken for a target.
// No connection rules needed: this is pure inspection.
func cmdInventory(ctx context.Context, tn tailnet.Tailnet) error {
	self, err := tn.Self(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("self: %s (%s) tags=%s\n\n", self.Hostname, firstIP(self), displayTags(self.Tags))

	peers, err := tn.Peers(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTNAME\tIP\tSTATUS\tTAGS")
	for _, p := range peers {
		status := "offline"
		if p.Online {
			status = "online"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Hostname, firstIP(p), status, displayTags(p.Tags))
	}
	return w.Flush()
}

// cmdInventoryByTag groups the same inventory by tag instead of by
// host — this is the view that actually answers "what would
// smed.Tag(x) hit right now", which the host-centric table makes you
// reconstruct by scanning every row. A host with several tags appears
// under each one, same as Tag() would actually select it either way;
// a host with none shows up under "(untagged)" rather than becoming
// invisible just because this view is organized by tag.
func cmdInventoryByTag(ctx context.Context, tn tailnet.Tailnet) error {
	self, err := tn.Self(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("self: %s (%s) tags=%s\n\n", self.Hostname, firstIP(self), displayTags(self.Tags))

	peers, err := tn.Peers(ctx)
	if err != nil {
		return err
	}

	const untagged = "(untagged)"
	byTag := map[string][]string{}
	for _, p := range peers {
		tags := p.Tags
		if len(tags) == 0 {
			byTag[untagged] = append(byTag[untagged], p.Hostname)
			continue
		}
		for _, t := range tags {
			byTag[strings.TrimPrefix(t, "tag:")] = append(byTag[strings.TrimPrefix(t, "tag:")], p.Hostname)
		}
	}

	tagNames := make([]string, 0, len(byTag))
	for t := range byTag {
		if t != untagged {
			tagNames = append(tagNames, t)
		}
	}
	sort.Strings(tagNames)
	if _, ok := byTag[untagged]; ok {
		tagNames = append(tagNames, untagged) // always last, it's a catch-all, not a real tag
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TAG\tHOSTS")
	for _, t := range tagNames {
		hosts := byTag[t]
		sort.Strings(hosts)
		fmt.Fprintf(w, "%s\t%s\n", t, strings.Join(hosts, ", "))
	}
	return w.Flush()
}

// displayTags strips Tailscale's "tag:" prefix for a cleaner table —
// the same "tag:web" → "web" transform connect.Merge applies for
// grouping (see README.md, "Tailscale is the backend, not a plugin"),
// applied here for display too — and joins with commas instead of
// printing Go's raw slice syntax.
func displayTags(tags []string) string {
	if len(tags) == 0 {
		return "-"
	}
	stripped := make([]string, len(tags))
	for i, t := range tags {
		stripped[i] = strings.TrimPrefix(t, "tag:")
	}
	return strings.Join(stripped, ",")
}

func firstIP(p tailnet.Peer) string {
	if len(p.TailscaleIPs) == 0 {
		return "-"
	}
	return p.TailscaleIPs[0].String()
}

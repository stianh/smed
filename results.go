package smed

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/stianh/smed/dag"
)

// Results is what Apply returns: every matched host's step results. A
// named type instead of a bare map, for one reason — every demo this
// project has written so far hand-rolled the same double loop
// (range hosts, range that host's steps, check Status) just to print
// what happened. Results.String() does that once: fmt.Println(results)
// or log.Println(results) just works, the same way any fmt.Stringer
// does.
type Results map[string][]dag.Result

// String renders Results as a table — HOST, STEP, STATUS, CHANGED,
// ERROR — sorted by hostname, since map iteration order is random and
// a table that reorders itself every run isn't one you can read.
func (r Results) String() string {
	hosts := make([]string, 0, len(r))
	for h := range r {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tSTEP\tSTATUS\tCHANGED\tERROR")
	for _, host := range hosts {
		for _, s := range r[host] {
			errText := ""
			if s.Err != nil {
				errText = s.Err.Error()
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\n", host, s.ID, s.Status, s.Changed, errText)
		}
	}
	w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

// Failed reports whether any step, on any host, came back
// StatusFailed — the same check rolling batches use internally to
// decide whether to stop, exposed here for anything that just wants a
// yes/no without walking the structure itself. A StatusSkipped step is
// never the sole signal of trouble: whatever caused it to be skipped
// already produced a StatusFailed entry on that same host, so checking
// Failed alone is sufficient.
func (r Results) Failed() bool {
	for _, steps := range r {
		for _, s := range steps {
			if s.Status == dag.StatusFailed {
				return true
			}
		}
	}
	return false
}

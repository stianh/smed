package smed

// Deletion without a state file (see README.md, "No state file —
// deletion without one"). Two mechanisms, same shape underneath —
// declared set vs. remembered/observed set, delete the extras — with
// different sources for "what used to be here":
//
//   - Owns reconciles in real time: list the real directory on the
//     target right now, compare against what's declared this run.
//   - Receipts reconcile from memory: a small JSON file smed writes
//     on the host itself, compared the same way, for paths that don't
//     live in one tidy directory worth owning outright.
//
// Both only ever see steps whose Module implements module.Pathed —
// anything else is simply invisible to this, not an error.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stianh/smed/dag"
	"github.com/stianh/smed/module"
	"github.com/stianh/smed/shellquote"
)

// Owns declares that this HostSpec fully owns everything matching
// pattern (a shell glob, e.g. "/etc/nginx/conf.d/*") on the target:
// anything present there that isn't declared by one of this chain's
// own Pathed steps gets deleted. pattern is interpolated into a shell
// command unquoted — it has to be, to actually glob — so it comes
// from your own trusted config, never from anything external.
func (h *HostSpec) Owns(pattern string) *HostSpec {
	h.owns = append(h.owns, pattern)
	return h
}

// Receipts tracks every Pathed step in this chain that isn't already
// covered by an Owns pattern, under key, in a small file smed writes
// on the host (/var/lib/smed/receipts.json) — so a path that's been
// removed from this chain's declaration gets deleted next run, even
// though it doesn't live in any zone worth owning outright. key only
// has to be unique among chains that might target the same host; it's
// how two unrelated chains' tracked sets don't stomp each other.
func (h *HostSpec) Receipts(key string) *HostSpec {
	h.receiptKeys = append(h.receiptKeys, key)
	return h
}

const receiptPath = "/var/lib/smed/receipts.json"

// reconciliationDeps returns the single dependency every reconciliation
// step (Owns or Receipts, mutating or planned) must wait on: the last
// regular step in the chain, since reconciliation has to happen
// strictly after everything that creates or updates a file, never
// before. nil when there are no regular steps to depend on.
func reconciliationDeps(regularSteps []dag.Step) []string {
	if len(regularSteps) == 0 {
		return nil
	}
	return []string{regularSteps[len(regularSteps)-1].ID}
}

// declaredPaths groups every declared step's path (for anything
// implementing module.Pathed) either under whichever Owns pattern
// claims it, or — if none does — into forReceipts, the set every
// Receipts key reconciles against. Shared by reconciliationSteps and
// planReconciliationSteps so a plan and the real Apply it's previewing
// always agree on what "declared" means.
func (h *HostSpec) declaredPaths() (byOwnsPattern map[string][]string, forReceipts []string) {
	byOwnsPattern = make(map[string][]string, len(h.owns))
	for _, s := range h.steps {
		p, ok := s.m.(module.Pathed)
		if !ok {
			continue
		}
		path := p.FilePath()
		owner := ""
		for _, pattern := range h.owns {
			if matched, _ := filepath.Match(pattern, path); matched {
				owner = pattern
				break
			}
		}
		if owner != "" {
			byOwnsPattern[owner] = append(byOwnsPattern[owner], path)
		} else {
			forReceipts = append(forReceipts, path)
		}
	}
	return byOwnsPattern, forReceipts
}

// reconciliationSteps builds the Owns and Receipts dag.Steps for this
// chain, given runner and the already-built regular steps (for
// dependency wiring). Returns nil if neither Owns nor Receipts was
// ever called.
func (h *HostSpec) reconciliationSteps(runner module.Runner, regularSteps []dag.Step) []dag.Step {
	if len(h.owns) == 0 && len(h.receiptKeys) == 0 {
		return nil
	}
	deps := reconciliationDeps(regularSteps)
	byOwnsPattern, forReceipts := h.declaredPaths()

	var out []dag.Step
	for _, pattern := range h.owns {
		out = append(out, ownsStep(pattern, byOwnsPattern[pattern], deps, runner))
	}
	for _, key := range h.receiptKeys {
		out = append(out, receiptStep(key, forReceipts, deps, runner))
	}
	return out
}

// planReconciliationSteps is reconciliationSteps' read-only twin: same
// dependency wiring and the same declaredPaths bookkeeping, but builds
// planOwnsStep/planReceiptStep instead — steps that only ever read,
// reporting what they'd delete via the module.Change this records into
// changes[id], never actually deleting or writing anything. Returns
// nil if neither Owns nor Receipts was ever called.
func (h *HostSpec) planReconciliationSteps(runner module.Runner, regularSteps []dag.Step, changes map[string]*module.Change) []dag.Step {
	if len(h.owns) == 0 && len(h.receiptKeys) == 0 {
		return nil
	}
	deps := reconciliationDeps(regularSteps)
	byOwnsPattern, forReceipts := h.declaredPaths()

	var out []dag.Step
	for _, pattern := range h.owns {
		id := "owns:" + pattern
		c := new(module.Change)
		changes[id] = c
		out = append(out, planOwnsStep(id, pattern, byOwnsPattern[pattern], deps, runner, c))
	}
	for _, key := range h.receiptKeys {
		id := "receipts:" + key
		c := new(module.Change)
		changes[id] = c
		out = append(out, planReceiptStep(id, key, forReceipts, deps, runner, c))
	}
	return out
}

// ownsStep lists pattern on the real target (letting the remote
// shell's own glob expansion decide what matches — the same
// expansion Apply's own commands already rely on, so there's no
// separate glob semantics to keep in sync) and deletes whatever's
// present but not in declared.
func ownsStep(pattern string, declared []string, deps []string, runner module.Runner) dag.Step {
	declaredSet := toSet(declared)
	return dag.Step{
		ID:   "owns:" + pattern,
		Deps: deps,
		Run: func(ctx context.Context) (bool, error) {
			out, err := runner.Run(ctx, fmt.Sprintf("ls -d %s 2>/dev/null", pattern))
			if err != nil {
				return false, fmt.Errorf("owns %s: list: %w", pattern, err)
			}
			toDelete := linesNotIn(out, declaredSet)
			if len(toDelete) == 0 {
				return false, nil
			}
			if err := deletePaths(ctx, runner, toDelete); err != nil {
				return false, fmt.Errorf("owns %s: %w", pattern, err)
			}
			return true, nil
		},
	}
}

// planOwnsStep is ownsStep's read-only twin: lists pattern on the real
// target exactly the same way (that part was always a read), but stops
// there — it reports what it would delete via out instead of calling
// deletePaths.
func planOwnsStep(id, pattern string, declared []string, deps []string, runner module.Runner, out *module.Change) dag.Step {
	declaredSet := toSet(declared)
	return dag.Step{
		ID:   id,
		Deps: deps,
		Run: func(ctx context.Context) (bool, error) {
			lsOut, err := runner.Run(ctx, fmt.Sprintf("ls -d %s 2>/dev/null", pattern))
			if err != nil {
				return false, fmt.Errorf("owns %s: list: %w", pattern, err)
			}
			toDelete := linesNotIn(lsOut, declaredSet)
			if len(toDelete) == 0 {
				*out = module.Change{Action: module.NoOp}
				return false, nil
			}
			*out = module.Change{
				Action:      module.Delete,
				Description: fmt.Sprintf("remove %d untracked path(s): %s", len(toDelete), strings.Join(toDelete, ", ")),
			}
			return true, nil
		},
	}
}

// receiptStep reads this key's remembered paths from the receipt file
// (missing file or missing key: no history yet, nothing to clean up —
// the honest degrade-gracefully behavior a pure cache is supposed to
// have), deletes whatever's remembered but no longer declared, then
// writes the full current declared set back under key, leaving every
// other key in the file untouched.
func receiptStep(key string, declared []string, deps []string, runner module.Runner) dag.Step {
	return dag.Step{
		ID:   "receipts:" + key,
		Deps: deps,
		Run: func(ctx context.Context) (bool, error) {
			raw, _ := runner.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellquote.Quote(receiptPath)))

			receipts := map[string][]string{}
			if strings.TrimSpace(raw) != "" {
				if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
					return false, fmt.Errorf("receipts %s: parse %s: %w", key, receiptPath, err)
				}
			}

			toDelete := notIn(receipts[key], toSet(declared))
			changed := false
			if len(toDelete) > 0 {
				if err := deletePaths(ctx, runner, toDelete); err != nil {
					return false, fmt.Errorf("receipts %s: %w", key, err)
				}
				changed = true
			}

			receipts[key] = declared
			encoded, err := json.Marshal(receipts)
			if err != nil {
				return changed, fmt.Errorf("receipts %s: encode: %w", key, err)
			}
			writeCmd := fmt.Sprintf("mkdir -p %s && echo %s | base64 -d > %s",
				shellquote.Quote(filepath.Dir(receiptPath)),
				shellquote.Quote(base64.StdEncoding.EncodeToString(encoded)),
				shellquote.Quote(receiptPath))
			if _, err := runner.Run(ctx, writeCmd); err != nil {
				return changed, fmt.Errorf("receipts %s: write: %w", key, err)
			}
			return changed, nil
		},
	}
}

// planReceiptStep is receiptStep's read-only twin: reads the receipt
// file (a read receiptStep always did too, before deciding whether to
// write) and reports what it would delete via out — but, unlike
// receiptStep, never deletes anything and never writes the receipts
// file back. A plan has to leave the host's receipt memory exactly as
// it found it; writing it here would make a second, unrelated Plan
// call see a different "remembered" set than Apply would have.
func planReceiptStep(id, key string, declared []string, deps []string, runner module.Runner, out *module.Change) dag.Step {
	return dag.Step{
		ID:   id,
		Deps: deps,
		Run: func(ctx context.Context) (bool, error) {
			raw, _ := runner.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellquote.Quote(receiptPath)))

			receipts := map[string][]string{}
			if strings.TrimSpace(raw) != "" {
				if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
					return false, fmt.Errorf("receipts %s: parse %s: %w", key, receiptPath, err)
				}
			}

			toDelete := notIn(receipts[key], toSet(declared))
			if len(toDelete) == 0 {
				*out = module.Change{Action: module.NoOp}
				return false, nil
			}
			*out = module.Change{
				Action:      module.Delete,
				Description: fmt.Sprintf("remove %d untracked path(s): %s", len(toDelete), strings.Join(toDelete, ", ")),
			}
			return true, nil
		},
	}
}

func deletePaths(ctx context.Context, runner module.Runner, paths []string) error {
	cmd := "rm -rf"
	for _, p := range paths {
		cmd += " " + shellquote.Quote(p)
	}
	_, err := runner.Run(ctx, cmd)
	return err
}

func toSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}

// linesNotIn splits ls's newline-delimited output and returns
// whichever lines aren't in declared.
func linesNotIn(out string, declared map[string]bool) []string {
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || declared[line] {
			continue
		}
		result = append(result, line)
	}
	return result
}

func notIn(remembered []string, declared map[string]bool) []string {
	var result []string
	for _, p := range remembered {
		if !declared[p] {
			result = append(result, p)
		}
	}
	return result
}

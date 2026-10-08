# smed

I manage a small fleet of servers over Tailscale, and wanted a tool that fit that setup without dragging in a YAML templating language, a Python interpreter on every target, or a state file I have to trust is still accurate months later.

Ansible gets one thing right: it actually looks at a machine before it acts on it. Terraform gets a different thing right: it shows you a plan before you commit to it. Neither one does both. Ansible reads the world but executes a linear script with no "here's what I'm about to do" step to review. Terraform plans, but against a state file instead of the world — and the state file drifts, gets corrupted, needs locking, needs `import`/`refresh`/`state mv` ceremony, and becomes its own source of outages. The thing that was supposed to give you confidence becomes the thing you're most afraid of.

**smed reads the world and plans against it.** No interpreter dependency on the target, no state file anywhere. Plan is: go look, then compute a diff, then show it to you. Apply is: do the diff, concurrently, per host. smed is a Go library — not a YAML file, not a DSL standing in for a real language, actual Go — built around Tailscale as the default way to find and reach machines, a real dependency graph instead of a flat task list, and no state file to keep in sync with reality.

This isn't trying to replace Ansible for everyone, win an ecosystem, or get wide adoption. It's a tool for running my own machines, built the way I'd want it built if I didn't have to care what anyone else thinks. Simplicity is the goal, not reach. Where those two pull in different directions, simplicity wins.

## Install

```sh
go get github.com/stianh/smed
```

## The shape of a playbook

A playbook here is just a Go value: a chain of steps built with `smed.Host(...)`/`smed.Tag(...)`, handed to one of three methods depending on what you're asking:

1. **`Explain()`** — no network, not even `Options`. Answers "why does this step exist, what does it depend on, what would make this handler run?" by reading nothing but the chain you just declared. Reach for this first, and whenever a handler isn't firing the way you expected — it'll tell you outright if nothing notifies it.
2. **`Plan()`** / **`Plan().Drift()`** — a live, read-only Read+Diff against the real targets. Answers "what would `Apply` do right now?" and "has anything drifted since I last looked?" with the same mechanism, because smed only ever has one source of truth (the real machine) to ask either question against. Safe to run anytime, including against production, including on a cron schedule — it never calls `Apply`.
3. **`Apply()`** — does it. Same selector, same graph, same per-host concurrency as `Plan`, except this time the diff actually gets executed.

That ordering — explain, then plan or drift, then apply — is the whole workflow. Nothing here needs a fourth concept; Terraform's plan/apply split and drift detection exist as three separate ideas because it has two sources of truth (a state file and reality) that can disagree with each other. smed has one, so "what would change" and "what's already changed" collapse into the same `Plan` call, asked at different times.

## A comprehensive example

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/stianh/smed"
	"github.com/stianh/smed/connect"
	"github.com/stianh/smed/module"
	"github.com/stianh/smed/module/apt"
	"github.com/stianh/smed/module/file"
	"github.com/stianh/smed/module/service"
	"github.com/stianh/smed/module/ufw"
	"github.com/stianh/smed/module/user"
	"github.com/stianh/smed/secret"
	"github.com/stianh/smed/secret/onepassword"
	"github.com/stianh/smed/tailnet"
)

// connectionRules says who to connect as — Tailscale knows what exists
// and how it's tagged, not what Unix account to use. Most specific
// match wins.
var connectionRules = []connect.Rule{
	{MatchHost: "db-p-01", User: "root"}, // mid-bootstrap exception, not provisioned yet
	{MatchTag: "prod", User: "deploy"},
	{MatchTag: "dev", User: "deploy"},
}

const nginxConf = "server { listen 80; server_name _; root /var/www/html; }\n"

// webServers is one playbook: everything tag:web needs. A playbook is
// just a function returning a *smed.HostSpec — composing several of
// them in main is how you structure more than one role.
func webServers(ctx context.Context) (*smed.HostSpec, error) {
	// Resolved once, right here, and handed straight to the one field
	// that needs it — never assigned to a long-lived variable, never
	// logged, never committed. Only the op:// reference below ever
	// touches source control.
	apiKey, err := secret.Lazy(onepassword.Resolver{}, "op://Engineering/web-app/api-key")(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve web-app api key: %w", err)
	}

	return smed.Tag("web").
		Run(apt.Installed{Name: "nginx"}).
		Run(file.Content{Path: "/etc/nginx/conf.d/app.conf", Data: nginxConf, Mode: "0644"}).
		Notify("restart-nginx").
		Run(file.Content{Path: "/etc/myapp/api-key.env", Data: "API_KEY=" + apiKey + "\n", Mode: "0600"}).
		Handler("restart-nginx", service.Unit{Name: "nginx", Active: true, Enabled: true}).
		Owns("/etc/nginx/conf.d/*"), // delete any stray conf file this chain didn't declare
		nil
}

// dbServers is a second playbook, showing Receipts instead of Owns:
// its managed paths (a home-less service account, a firewall rule)
// don't live in one directory worth owning outright.
func dbServers() *smed.HostSpec {
	return smed.Tag("db").
		Run(user.Account{Name: "postgres", System: true, Shell: "/usr/sbin/nologin"}).
		Run(ufw.Rule{Allow: true, Port: "5432", Proto: "tcp", From: "100.64.0.0/10"}). // tailnet CIDR only
		Receipts("db-baseline")
}

// healthy is a placeholder HealthCheck for rolling batches — a real
// one would hit an actual readiness endpoint or metric; this just
// proves the target is still reachable and responsive.
func healthy(ctx context.Context, target connect.Target, r module.Runner) error {
	_, err := r.Run(ctx, "true")
	return err
}

func main() {
	ctx := context.Background()

	web, err := webServers(ctx)
	if err != nil {
		log.Fatal(err)
	}
	playbooks := []*smed.HostSpec{web, dbServers()}

	// Explain first — zero network, just confirms the graph you
	// declared is the graph you meant.
	for _, pb := range playbooks {
		e, err := pb.Explain()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(e)
	}

	opts := smed.Options{
		Tailnet:     tailnet.LocalDaemon(),
		Rules:       connectionRules,
		Become:      true,
		BatchSize:   1, // one host at a time
		HealthCheck: healthy,
	}

	for _, pb := range playbooks {
		plan, err := pb.Plan(ctx, opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(plan)
		if !plan.Drift() {
			continue // nothing to do
		}

		results, err := pb.Apply(ctx, opts)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(results)
	}
}
```

A few things worth calling out:

- **`webServers` and `dbServers` are ordinary functions.** There's no playbook type to learn, no registry to register them in — a project with ten roles is ten functions like these, composed however your `main` wants (sequentially, as above; concurrently; conditionally on a flag). `go build` is the only "playbook runner" that exists.
- **`Notify`/`Handler`** replace Ansible's notify/flush-handlers mechanism with one dependency edge: `restart-nginx` depends on the config-file step and only actually runs if that step changed something. No separate flush phase, because there's no separate phase at all — just a graph.
- **`Owns` vs. `Receipts`** are both deletion-without-a-state-file, for two different shapes of "what used to be here": `Owns` lists a real directory on the target right now and deletes anything present but undeclared; `Receipts` remembers a scattered set of paths in a small JSON file smed writes on the host itself, for things that don't share a directory worth owning outright.
- **The secret never touches source control or a module field as plaintext** — only the `op://` reference does. It's resolved exactly once, inline, and handed straight to the field that needs it.
- **`BatchSize`/`HealthCheck`** opt into the one barrier smed otherwise never imposes: without them, every matched host runs fully concurrently and a slow host never blocks a fast one. Setting either reintroduces sequential batches with a health gate between them, and a failing batch stops the rollout before the next one starts.

## What makes this different

### Tailscale is the backend, not a plugin

Inventory and transport both come from the tailnet. Tags become groups (`tag:web` → the `web` you select with `smed.Tag("web")`), and there's no SSH key to generate, distribute, or rotate — host identity comes from Tailscale's own node identity. Running from a developer machine that's already joined the tailnet and running from a CI box that's never seen it before both work through the same interface (a local-daemon backend talking to `tailscaled`'s LocalAPI, or an ephemeral `tsnet` node joined with an authkey); which one you're in is detected automatically, never a flag you set.

Connecting still needs a second answer Tailscale has no opinion on — what Unix account to run as — which is why `connect.Rule` exists as a small ordered table, most specific match wins (host beats tag beats a global default — the same precedence Ansible's `host_vars`/`group_vars` already use, just expressed as a Go slice literal instead of YAML). Non-Tailscale hosts fold into the exact same model via `connect.StaticHost`, tagged by hand, matched by the same rules — there's no second inventory system for the one box that isn't on the tailnet yet.

### A real dependency graph, not a flat task list

Ansible runs tasks top-to-bottom with an implicit wait after every single one — your fastest host still waits for your slowest. smed builds an actual graph: independent steps run concurrently, each host gets its own graph and its own goroutine, and a failure only blocks what genuinely depended on it, never anything unrelated. There is no implicit barrier anywhere in this — host A can be on step 12 while host B is still on step 3. A barrier only exists where you declare one, which is exactly what `BatchSize`/`HealthCheck` are.

This also changes what a "handler" is. Ansible needs a separate notify/flush-handlers mechanism because its task list has no dependency edges to express "run this after everything that might need it, but only once, and only if something actually changed." A real graph already has all of that: a handler is just a node with incoming edges from whatever notifies it (so it naturally runs once, after all of them), gated to only fire if one of them actually changed something (`dag.Step.RunIf`, with `dag.IfAnyChanged` as the one rule a handler needs). No second mechanism bolted on next to the task list — it's a property of having a graph in the first place.

### No state file — deletion without one

The one thing a state file genuinely buys you is knowing you used to manage something that's no longer declared, so you can clean it up. Two mechanisms replace it, no central file required, both reducing to the same shape — declared set vs. remembered/observed set, delete the extras — with a different source for "what used to be here":

- **`Owns(pattern)`** — real-time: `ls -d <pattern>` on the target right now, compared against whatever this chain's own path-aware steps declare under that pattern. Anything present but undeclared is deleted, strictly after everything that creates or updates a file in the chain.
- **`Receipts(key)`** — memory: every declared path *not* already claimed by an `Owns` pattern gets tracked under `key` in a small JSON file smed writes on the host itself (`/var/lib/smed/receipts.json`), for paths that don't live in one tidy directory worth owning outright. A missing or empty file just means no history yet — the honest degrade-gracefully behavior a pure cache is supposed to have, never an error.

A module opts into either by implementing one method (`FilePath() string`) — anything that doesn't is simply invisible to this, an honest limitation rather than a silent gap. There's no `Absent` yet (the explicit "delete this one specific thing" escape hatch Ansible has) — a real, deliberately deferred gap, not an oversight.

### Secrets: a reference in your repo, not an encrypted blob

Ansible Vault encrypts the secret itself and commits the encrypted bytes to git. That means a shared vault password that has to be distributed to everyone and everything that runs the playbook (and is itself now a secret you have to protect), a diff on a vault file that tells you nothing because the whole point was hiding what changed, and a decrypted value that's just a normal variable afterward — nothing stops it from ending up in a log line once it's in memory.

smed never puts the secret in the repo at all — only a reference to where it actually lives:

```go
apiKey, err := secret.Lazy(onepassword.Resolver{}, "op://Engineering/web-app/api-key")(ctx)
```

That's a URI, not a credential — committing it is no more sensitive than committing a variable name. Access control and audit logging are 1Password's own, which you're almost certainly already managing for other reasons; there's no second password to distribute. `secret.Lazy` wraps a `Resolver` and a `Ref` into a closure, and the value doesn't exist anywhere until that closure is actually called — resolve it inline, right where it's needed, and it's never a long-lived variable that could get logged or printed by accident. Swapping 1Password for a different store (there's also an `env` backend, proving it) is a different one-method implementation of `secret.Resolver`, not a different mechanism.

Getting the resolved value onto the target without it showing up in `ps aux` on that machine is a separate, smaller problem this package doesn't try to solve by itself — two existing Linux mechanisms already cover it: `systemd-creds`/`LoadCredential=` for a running service that needs its own secret at startup, and an env-var-prefixed invocation (`PGPASSWORD=x psql ...`) over a bare command-line argument for a one-off command. No new plumbing was built for either.

## Architecture decisions

The load-bearing choices, condensed — the reasoning any of these could get relitigated from if a real need shows up:

- **Config is Go, not a data file.** No YAML, no templating language for control flow, no `inventory.toml`. The content that would go in a config file here is almost always a flat rule table or a linear chain — nothing a data format expresses that a Go slice literal and a method chain don't already, while a data format costs a parser dependency and a second syntax to keep stable. Compile-time field-name checking is a real win a data file can't match. File templating uses Go's own `text/template`, not a second templating language — porting a Jinja template is exactly the kind of mechanical translation an LLM does well, so there's no ambition to stay compatible with `.j2` assets.
- **The module contract is `Read`/`Diff`/`Apply`/`Verify`, and `Read` never mutates.** That invariant is what makes a plan trustworthy in a way Ansible's `--check` mode never was — `--check` just hopes a module behaves; here, planning literally cannot reach `Apply`, because `Plan` never calls it. `Verify` re-reads after `Apply` and confirms it actually converged, catching a module that silently didn't do what it claimed. There's no generic `Target` type carrying "where" and "what" separately, the way an early sketch of this had — every module already carries its own desired state as plain struct fields, so the only thing it needs from outside is somewhere to run a command (`Runner`, one method), and `run.Session` satisfies that with no adapter.
- **Modules are a plain Go interface — no registry, no plugin RPC, no WASM.** Anything that implements the five methods is a valid module; a module depends on nothing but `Runner`, not on Tailscale or SSH specifics. That means an opinionated module (postgres, nginx vhosts, a specific app) can live in its own separately-versioned repo and plug into the same graph through the same `module.Step` as a built-in — `go.mod` plus GitHub already is the registry, same as for any other Go dependency. What ships in `module/...` here stays deliberately short and nobody's-stack-specific: `file`, `apt`, `service`, `user`, `ufw`, `command`.
- **smed is a library, not a framework with an installed binary that runs your config.** Go is compiled, so an installed binary can't load a separately-written Go program at runtime without a plugin/RPC layer — exactly the indirection the module interface already rejected. So configuration lives in a project that imports smed via `go.mod`, same as any other dependency, and `go run .` is the dispatcher — there's no project-discovery mechanism to invent. The installed `smed` binary (this repo's `cmd/smed`) stays deliberately thin: inspection only (`smed inventory`), nothing that needs config it can't have.
- **`Plan` returns a value, not a file.** An earlier sketch of this imagined a `smed plan -out plan.smd` / `smed apply plan.smd` pair, stamped with host fingerprints and rejected if stale — Terraform's workflow, roughly. It didn't survive contact with the actual implementation: once `Plan` is just a live `Read`+`Diff` that costs the same as `Apply` minus the mutation, re-running it immediately before `Apply` is exactly as cheap as deserializing a file and checking it isn't stale — so there's no persisted plan artifact, and no staleness ceremony to design around.
- **Rolling batches are two independent knobs, not one feature.** `BatchSize` alone only limits concurrency (don't hit a shared connection pool or rate limit with every host at once) — it says nothing about availability. `HealthCheck` is the gate that actually protects it: run once per host after its batch finishes clean, before the next batch starts. `BatchSize` without `HealthCheck` still just limits concurrency; nothing stops a broken batch being followed by another. Both default off, because enabling either deliberately reintroduces the one barrier smed otherwise never imposes between hosts.

## Explicitly out of scope, or deferred on purpose

Central server, agents, a state backend, a module registry, any Ansible compatibility or bridge layer (module bridge, playbook import, `.j2` compatibility — translating an existing Ansible setup is an LLM's job, not smed's), Python anywhere, YAML, ecosystem parity.

Deferred, not rejected — real gaps, stated rather than quietly worked around: explicit `Absent` (deleting one specific declared thing, the way `Owns`/`Receipts` only ever delete the *undeclared*); password-based `sudo` (`run.Become` requires passwordless/NOPASSWD sudo today); a later step depending on a handler's completion through the fluent DSL (only a handler's own notifiers can be its deps right now); no SSH host-key pinning yet (`run` connects with `ssh.InsecureIgnoreHostKey()` — fine over Tailscale's own authenticated WireGuard tunnel for now, not fine the day a non-Tailscale `StaticHost` matters more); and cloud-init bootstrap for brand-new hosts (provisioning a box well enough that it shows up already tailnet-joined and correctly tagged before smed ever talks to it — a one-shot action, not part of the regular plan/apply loop).

## Where it stands

Real today, validated against a real fleet over Tailscale — not just unit tests: inventory and transport (both backends), connection rules, sudo escalation, the full `Host`/`Tag` DSL with handlers, `Owns`/`Receipts`, rolling batches, `Plan`/`Drift`, `Explain`, the secrets package (1Password + env backends), and six modules (`file`, `apt`, `service`, `user`, `ufw`, `command`). The core transport thesis — a pushed Go binary over one multiplexed connection beats a Python-per-task round trip — was confirmed early with a small benchmark (~270-290x on a 15-host × 25-command fixture) before any of the rest of this got built on top of it; that harness lives in a separate `smed-bench` project, not here.

## Is this for you?

Probably not, and that's fine. This fits how I run my own machines; it's shared because sharing costs nothing, not because it's trying to become your tool too. No roadmap aimed at use cases I don't have, no promise it stays stable for anyone but me.

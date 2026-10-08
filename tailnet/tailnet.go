// Package tailnet is smed's default inventory source and transport.
//
// Getting onto the tailnet has two shapes (see README.md, "Tailscale
// is the backend, not a plugin"): a developer machine that's already
// joined it, and a CI/controller joining fresh. Those are two backends
// behind one interface, selected automatically — never a user-facing
// mode. This file is the first one: the local-daemon backend, for the
// common case where tailscaled is already running on this machine.
package tailnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"

	"tailscale.com/client/local"
)

// Peer is one other tailnet member, as smed's inventory sees it.
type Peer struct {
	Hostname     string
	DNSName      string
	TailscaleIPs []netip.Addr
	Tags         []string
	Online       bool
}

// Tailnet is how smed discovers hosts and reaches them.
type Tailnet interface {
	// Peers returns every other member of the tailnet. The control node
	// itself is never included, regardless of backend.
	Peers(ctx context.Context) ([]Peer, error)
	// Self returns this machine's own tailnet identity — e.g. to
	// deliberately target ourselves for a connectivity check. Never
	// returned by Peers.
	Self(ctx context.Context) (Peer, error)
	// Dial opens a TCP connection to the given peer on the given port.
	Dial(ctx context.Context, p Peer, port string) (net.Conn, error)
}

// LocalDaemon returns a Tailnet backed by this machine's own running
// tailscaled, reached over its local Unix socket. This is the common case: a
// developer's machine that's already on the tailnet, so no join step and no
// authkey are needed.
func LocalDaemon() Tailnet {
	return &localDaemon{client: &local.Client{}}
}

type localDaemon struct {
	client *local.Client
}

func (d *localDaemon) Peers(ctx context.Context) ([]Peer, error) {
	status, err := d.client.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailnet status: %w", err)
	}
	if status.Self == nil {
		return nil, fmt.Errorf("tailnet status: no self node (not logged in / not running?)")
	}

	var peers []Peer
	for key, ps := range status.Peer {
		if key == status.Self.PublicKey {
			continue // never inventory the control node itself
		}
		var tags []string
		if ps.Tags != nil {
			tags = ps.Tags.AsSlice()
		}
		peers = append(peers, Peer{
			Hostname:     ps.HostName,
			DNSName:      ps.DNSName,
			TailscaleIPs: ps.TailscaleIPs,
			Tags:         tags,
			Online:       ps.Online,
		})
	}
	// status.Peer is a map, so iteration order is random — callers
	// printing or displaying inventory shouldn't get a different order
	// every run just because that's how this happened to be built.
	slices.SortFunc(peers, func(a, b Peer) int { return strings.Compare(a.Hostname, b.Hostname) })
	return peers, nil
}

func (d *localDaemon) Dial(ctx context.Context, p Peer, port string) (net.Conn, error) {
	if len(p.TailscaleIPs) == 0 {
		return nil, fmt.Errorf("peer %q has no tailscale IP", p.Hostname)
	}
	// The local daemon already owns the OS routes to every tailnet peer
	// (via the tailscale0/utun interface), so a plain net.Dial reaches
	// them — no special dialer needed here. That's a property of *this*
	// backend only: a future tsnet backend (no daemon, no OS route) will
	// need its own Dial that goes through tsnet's userspace netstack.
	addr := net.JoinHostPort(p.TailscaleIPs[0].String(), port)
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", addr)
}

func (d *localDaemon) Self(ctx context.Context) (Peer, error) {
	status, err := d.client.Status(ctx)
	if err != nil {
		return Peer{}, fmt.Errorf("tailnet status: %w", err)
	}
	if status.Self == nil {
		return Peer{}, fmt.Errorf("tailnet status: no self node (not logged in / not running?)")
	}
	var tags []string
	if status.Self.Tags != nil {
		tags = status.Self.Tags.AsSlice()
	}
	return Peer{
		Hostname:     status.Self.HostName,
		DNSName:      status.Self.DNSName,
		TailscaleIPs: status.Self.TailscaleIPs,
		Tags:         tags,
		Online:       status.Self.Online,
	}, nil
}

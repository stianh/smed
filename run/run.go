// Package run is the part of smed that actually dials a target and
// executes something over SSH. It's a library, not a CLI: both smed's own
// scaffolding tool and a user's own project (which imports smed as its
// engine — see README.md, "smed is a library, not a framework with an
// installed binary that runs your config") call into this the same way.
package run

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/stianh/smed/connect"
	"github.com/stianh/smed/tailnet"
)

// Result is one target's outcome.
type Result struct {
	Target   connect.Target
	User     string
	Output   string
	Err      error
	Duration time.Duration
}

// Options configures a fan-out run.
type Options struct {
	Tailnet tailnet.Tailnet
	Rules   []connect.Rule
	// User, if non-empty, overrides connection-rule resolution for every
	// target — the highest-precedence override, same as the CLI's -user.
	User string
	Port string // default "22" if empty
}

// Command runs command on every target concurrently — one goroutine per
// target, no barrier between them, each target's result independent of
// the others'. This is smed's execution model (see README.md, "A real
// dependency graph, not a flat task list") applied to a flat list of
// targets; dag.Graph/HostSpec.Apply reuse the same no-barrier-between-
// hosts property for a real DAG of steps, rather than replacing it.
func Command(ctx context.Context, targets []connect.Target, opts Options, command string) ([]Result, error) {
	auth, err := AgentAuth()
	if err != nil {
		return nil, err
	}
	port := opts.Port
	if port == "" {
		port = "22"
	}

	results := make([]Result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t connect.Target) {
			defer wg.Done()
			t0 := time.Now()

			user, err := connect.Resolve(opts.Rules, t, opts.User)
			if err != nil {
				results[i] = Result{Target: t, Err: err, Duration: time.Since(t0)}
				return
			}

			out, err := one(ctx, opts.Tailnet, t, user, port, auth, command)
			results[i] = Result{Target: t, User: user, Output: out, Err: err, Duration: time.Since(t0)}
		}(i, t)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Target.Hostname < results[j].Target.Hostname })
	return results, nil
}

// one dials target (over the tailnet, or plain TCP for a static host),
// runs exactly one command, and tears the connection down. Fine for a
// fan-out across many different targets (Command's case); wasteful for
// several commands against the *same* target, which is what Session is
// for.
func one(ctx context.Context, tn tailnet.Tailnet, target connect.Target, user, port string, auth ssh.AuthMethod, command string) (string, error) {
	client, err := dial(ctx, tn, target, user, port, auth)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return runSession(client, command)
}

func dial(ctx context.Context, tn tailnet.Tailnet, target connect.Target, user, port string, auth ssh.AuthMethod) (*ssh.Client, error) {
	var conn net.Conn
	var err error
	switch {
	case target.Peer != nil:
		conn, err = tn.Dial(ctx, *target.Peer, port)
	case target.Addr != "":
		var d net.Dialer
		conn, err = d.DialContext(ctx, "tcp", target.Addr)
	default:
		return nil, fmt.Errorf("target %q has neither a tailnet peer nor a static address", target.Hostname)
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // no host-key pinning yet — see README.md's deferred list
		Timeout:         10 * time.Second,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, target.Hostname, config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

func runSession(client *ssh.Client, command string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new ssh session: %w", err)
	}
	defer session.Close()

	out, err := session.CombinedOutput(command)
	if err != nil {
		return string(out), fmt.Errorf("command failed: %w", err)
	}
	return string(out), nil
}

// Session is one persistent connection to a target: one SSH handshake,
// reused for as many commands as needed via separate sessions on the same
// connection. This is what a Module's Read/Apply/Verify sequence should
// run against — several round trips to the same host, not a fresh dial
// each time the way Command's fan-out needs.
type Session struct {
	client *ssh.Client
}

// Open dials target once and returns a Session for running multiple
// commands against it.
func Open(ctx context.Context, tn tailnet.Tailnet, target connect.Target, user, port string) (*Session, error) {
	auth, err := AgentAuth()
	if err != nil {
		return nil, err
	}
	if port == "" {
		port = "22"
	}
	client, err := dial(ctx, tn, target, user, port, auth)
	if err != nil {
		return nil, err
	}
	return &Session{client: client}, nil
}

// Run executes command over the session's existing connection. Matches
// the shape module.Runner expects — a *Session satisfies it directly,
// with no adapter needed.
func (s *Session) Run(ctx context.Context, command string) (string, error) {
	return runSession(s.client, command)
}

func (s *Session) Close() error {
	return s.client.Close()
}

// AgentAuth authenticates via the running ssh-agent rather than a key file
// on disk — the normal path for a real environment, not a special case.
func AgentAuth() (ssh.AuthMethod, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK not set (no ssh-agent running)")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial ssh-agent: %w", err)
	}
	ag := agent.NewClient(conn)
	return ssh.PublicKeysCallback(ag.Signers), nil
}

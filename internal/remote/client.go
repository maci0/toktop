// Package remote attaches toktop to engines on other hosts over one
// long-lived ssh connection: target parsing, host key handling, discovery of
// listening inference ports and periodic vitals sampling.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// audit builds the process logger for the lines this package writes. A var so
// a test can point it at a handler it can read; every call site is rare
// (one connect, one drop), so building it per call costs nothing.
var audit = logcfg.Logger

// runTimeout bounds one remote command (discovery or vitals poll). Var so
// tests can shrink it, like bannerTimeout and the keepalive pacing below.
var runTimeout = 15 * time.Second

// sessionOpenTimeout bounds the channel-open round trip on its own, well
// under runTimeout. A channel open is a single request that a healthy peer
// answers in milliseconds; runTimeout is the budget for a whole command and
// the runtimes of the script inside it.
var sessionOpenTimeout = 3 * time.Second

// Client is one long-lived ssh connection carrying everything toktop needs
// from a remote host: command sessions for discovery and vitals, plus direct
// TCP channels relayed onto local listeners for engine traffic. No ssh
// binary, no local port-forward races: Forward binds its listeners before
// returning, so backends can attach immediately.
type Client struct {
	Target Target

	conn    *ssh.Client
	closed  chan struct{}
	closeMu sync.Mutex // guards the close-once below
	errMu   sync.Mutex
	err     error

	mu        sync.Mutex
	listeners []net.Listener
	forwards  map[int]int // remote port -> local port already bound for it
	stopped   bool
	// relays is the set of local connections a relay is currently piping. A
	// piped pair parks in Read on both ends, and neither copy has a deadline,
	// so an engine that accepts a connection and then stalls holds a file
	// descriptor, two copy goroutines and an ssh channel until its own peer
	// goes away. Keeping the set lets teardown close them instead: closing
	// one end unblocks both copies, since the other direction's read returns
	// on that side's close.
	relays      map[net.Conn]struct{}
	relayDone   sync.WaitGroup
	relayActive chan struct{} // caps concurrent relayed connections

	connectedAt   time.Time     // when the handshake finished, for the drop line's uptime
	keepaliveDone chan struct{} // closed when the keepalive goroutine exits

	// forwardWarnMu guards forwardWarnAt, which holds the instant of the last
	// audited failure per forwarded port. A remote engine that is down for the
	// length of a run then reports at forwardWarnInterval instead of once per
	// dashboard poll.
	forwardWarnMu sync.Mutex
	forwardWarnAt map[int]time.Time
}

// Done fires when the connection drops for any reason, including Close.
func (c *Client) Done() <-chan struct{} { return c.closed }

// Err reports why the connection dropped; nil while alive.
func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *Client) setErr(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		err = nil // deliberate shutdown is not a loss
	}
	c.errMu.Lock()
	c.err = err
	c.errMu.Unlock()
}

// currentUser is the login name an ssh:// target without a user is opened
// with. USER and USERNAME are environment input like any other, so a value
// that would fail validTargetField is skipped rather than handed to the
// transport: sshd would reject it as an authentication failure, naming the
// password, long after the real cause. The passwd database is the fallback,
// and an empty result leaves ssh to its own default.
var currentUser = func() string {
	for _, name := range []string{"USER", "USERNAME"} {
		if u := os.Getenv(name); u != "" && validTargetField(u) == nil {
			return u
		}
	}
	if u, err := user.Current(); err == nil {
		if name := basenameLogin(u.Username); validTargetField(name) == nil {
			return name
		}
	}
	return ""
}

// basenameLogin strips a Windows DOMAIN\user or user/user prefix so the
// default ssh username is the account name, not the qualified form
// os/user.Current reports on domain-joined machines.
func basenameLogin(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// dialTimeout bounds TCP connect and DNS. Connect opens the socket itself
// rather than through ssh.Dial, so ssh.ClientConfig.Timeout never applies;
// without this a blackholed host hangs until the caller cancels. Var so
// tests can shrink it.
var dialTimeout = 8 * time.Second

// bannerTimeout bounds the wait for the remote sshd's version banner after
// TCP connect. ClientConfig.Timeout does not apply here (it only covers
// Dial's own connect), so without this a host that accepts the connection
// and then goes silent would stall Connect forever; x/crypto/ssh's
// handshake consults neither our context nor any deadline. Var so tests can
// shrink it.
var bannerTimeout = 15 * time.Second

// handshakeConn enforces bannerTimeout until the remote sshd's version
// banner is complete (its terminator, a newline, has been read), then lifts
// it permanently: interactive password auth inside NewClientConn may
// legitimately take as long as the user needs. Lifting on any byte instead
// would let a host that trickles part of the banner and stalls hang Connect
// forever - x/crypto/ssh consults neither our context nor any deadline of its
// own during the version exchange.
type handshakeConn struct {
	net.Conn
	lift  sync.Once
	sawNL bool
}

func (c *handshakeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if !c.sawNL {
		// The server identification string is the first thing a compliant
		// sshd sends and always ends in LF (RFC 4253); x/crypto/ssh itself
		// refuses banners without one, so this fires exactly once.
		if bytes.IndexByte(b[:n], '\n') >= 0 {
			c.sawNL = true
			c.lift.Do(func() { c.Conn.SetDeadline(time.Time{}) })
		}
	}
	return n, err
}

// newClientConnCtx runs the SSH transport handshake under ctx. x/crypto/ssh
// consults neither a context nor a deadline of its own, and handshakeConn
// lifts its deadline the moment the version banner arrives (interactive
// password auth may then take as long as the user needs). A peer that sends
// the banner and then stalls, or stalls mid key exchange, would otherwise
// block here forever: nothing observes the cancel from a signal handler, and
// the dashboard becomes unkillable by Ctrl-C. Closing the conn unblocks the
// handshake's read, which then reports the closed-conn error.
func newClientConnCtx(ctx context.Context, c net.Conn, addr string, cfg *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	type result struct {
		conn  ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		err   error
	}
	// Buffered: after a cancel the goroutine still delivers, and nobody is
	// left reading.
	done := make(chan result, 1)
	go func() {
		cc, chans, reqs, err := ssh.NewClientConn(c, addr, cfg)
		done <- result{cc, chans, reqs, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.chans, r.reqs, r.err
	case <-ctx.Done():
		c.Close()
		// The handshake may have completed in the same instant; drop the
		// connection rather than leak its goroutines and fds.
		go func() {
			if r := <-done; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, nil, nil, ctx.Err()
	}
}

// Connect establishes an authenticated connection to t. Credentials are tried
// in order: explicit key file, config/default keys, agent, then a password
// (TOKTOP_SSH_PASSWORD first, else an interactive prompt when stdin is a
// TTY). Host keys are trust-on-first-use with change detection.
//
// The failure is folded to a plain error with the home directory rewritten to
// "~": a refused key, an unreadable store or a changed host key all report
// the path they worked on, and that path names the account. The caller
// prints the message; nothing branches on its type.
func Connect(ctx context.Context, t Target) (*Client, error) {
	c, err := dial(ctx, t)
	if err != nil {
		// The reason reaches stderr, where the alt screen hides it: a refused
		// key, an unreachable host and an auth failure all look the same on a
		// dashboard with no engines on it. The audit line is the record that
		// outlives the frame.
		audit().Warn("toktop: ssh connect failed",
			"target", logcfg.RedactedField(t.LogHost(), 256),
			"port", t.Port,
			"error", logcfg.RedactedField(t.RedactUser(err.Error()), 256))
		return nil, errors.New(core.RedactHome(err.Error()))
	}
	audit().Info("toktop: ssh connected",
		"target", logcfg.RedactedField(t.LogHost(), 256),
		"port", t.Port,
		"dial", time.Since(c.connectedAt).Round(time.Millisecond))
	return c, nil
}

func dial(ctx context.Context, t Target) (*Client, error) {
	methods, cleanup, err := t.authMethods()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	pw := &passwordSource{}
	methods = append(methods, pw.authCallbacks(t)...)

	hk, err := tofu()
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            t.userOr(currentUser()),
		Auth:            methods,
		HostKeyCallback: hk,
	}
	// Library defaults still offer ssh-rsa (SHA-1) and DSA host keys, and
	// hmac-sha1-96, for old servers. SupportedAlgorithms is the same
	// preference list without those, and already leads with
	// mlkem768x25519-sha256. Using the library's set (not a pinned string
	// list) keeps new host-key and KEX algorithms as the package adds them.
	algs := ssh.SupportedAlgorithms()
	cfg.KeyExchanges = algs.KeyExchanges
	cfg.Ciphers = algs.Ciphers
	cfg.MACs = algs.MACs
	cfg.HostKeyAlgorithms = algs.HostKeys

	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	d := net.Dialer{Timeout: dialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	hc := &handshakeConn{Conn: nc}
	if err := hc.SetDeadline(time.Now().Add(bannerTimeout)); err != nil {
		nc.Close()
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	cc, chans, reqs, err := newClientConnCtx(ctx, hc, addr, cfg)
	if err != nil {
		nc.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("ssh %s: %w", t.UserHost(), authHint(err))
	}
	if err := hc.SetDeadline(time.Time{}); err != nil {
		cc.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.UserHost(), err)
	}
	c := &Client{Target: t, conn: ssh.NewClient(cc, chans, reqs), closed: make(chan struct{}),
		keepaliveDone: make(chan struct{}), connectedAt: time.Now(),
		relays: map[net.Conn]struct{}{}, relayActive: make(chan struct{}, maxConcurrentRelays)}
	go c.watchClose()
	go c.keepalive()
	return c, nil
}

// userOr falls back to the given default when no explicit user was resolved.
func (t Target) userOr(def string) string {
	if t.User != "" {
		return t.User
	}
	return def
}

// authHint annotates auth failures with what to try next.
func authHint(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "unable to authenticate") {
		if strings.Contains(msg, "password") { // a password was tried and refused too
			return fmt.Errorf("%w (all credentials rejected: wrong password, or your public key is not installed on this host)", err)
		}
		return fmt.Errorf("%w (keys rejected: install your public key on the host, load an agent, or answer the password prompt)", err)
	}
	return err
}

// keepaliveEvery and keepaliveReplies pace the liveness probe; vars so
// tests can shrink them. forwardDialTimeout bounds opening a tunneled TCP
// channel: Accept is unbounded (the listener lives as long as the client),
// but a hung Dial would pin a goroutine and the accepted fd per incoming
// connection until the ssh conn itself dies.
var (
	keepaliveEvery     = 15 * time.Second
	keepaliveReplies   = 5 * time.Second
	forwardDialTimeout = 8 * time.Second
)

// keepaliveMisses is how many unanswered probes close the connection. Three
// unanswered probes turn silent network death into a closed session within
// about a minute, which is long enough to ride out a dropped packet or a brief
// stall and short enough that a dead peer does not sit on a frozen dashboard.
const keepaliveMisses = 3

// keepalive turns silent network death into a closed connection within about
// a minute (keepaliveMisses unanswered probes) instead of a hung session. Each
// probe races a bounded wait: SendRequest alone would block forever on a peer
// that stops replying without closing TCP, so the miss counter would never
// advance.
func (c *Client) keepalive() {
	defer close(c.keepaliveDone)
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}
		if c.probe(keepaliveReplies) {
			misses = 0
			continue
		}
		misses++
		if misses >= keepaliveMisses {
			// watchClose records the drop; this says why, since an unanswered
			// keepalive and a peer that closed the socket need different
			// investigations and the wire error alone cannot tell them apart.
			audit().Warn("toktop: ssh peer stopped answering keepalives",
				"target", logcfg.RedactedField(c.Target.LogHost(), 256),
				"misses", misses,
				"probe_every", keepaliveEvery)
			c.conn.Close() // unblocks any probe still awaiting a reply
			return
		}
	}
}

// probe sends one global request and reports whether the peer answered in
// good health within wait. At most one probe goroutine can be stranded per
// miss window; conn.Close() releases it.
func (c *Client) probe(wait time.Duration) bool {
	type ack struct{ ok bool }
	ch := make(chan ack, 1)
	go func() {
		_, _, err := c.conn.SendRequest("keepalive@toktop", true, nil)
		ch <- ack{err == nil}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case a := <-ch:
		return a.ok
	case <-timer.C:
		return false
	case <-c.closed:
		return false
	}
}

const stderrBufferBytes = 4096

// stderrDrainGrace bounds how long a cancelled or timed-out Run waits for the
// command's own Wait to join the stderr copier. Well under runTimeout, so the
// wait can never turn a bounded command into an unbounded one.
const stderrDrainGrace = time.Second

// forwardBindAttempts bounds the rebind loop that steers a kernel-chosen
// ephemeral port away from the forwarded set.
const forwardBindAttempts = 8

// maxConcurrentRelays caps how many forwarded connections one client pipes at
// once. A loopback port handed to a client is not a secret, so nothing bounds
// how many connections can arrive; each costs a file descriptor, two copy
// goroutines and an ssh channel, and a peer that holds the socket open without
// speaking holds all three until teardown. Past the cap a connection is
// accepted and closed, which the local client sees as a refused connection.
const maxConcurrentRelays = 64

// stderrBuf collects a remote command's stderr. x/crypto/ssh copies it from
// a background goroutine that is only drained when Session.Output's Wait
// finishes, so on the timeout and cancellation paths below that goroutine can
// still be writing while this side reads. A bare bytes.Buffer has no internal
// locking: reading it concurrently with a Write is a data race.
type stderrBuf struct {
	mu  sync.Mutex
	buf [stderrBufferBytes]byte
	n   int
}

func (b *stderrBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if n >= len(b.buf) {
		b.n = copy(b.buf[:], p[n-len(b.buf):])
	} else {
		if overflow := b.n + n - len(b.buf); overflow > 0 {
			b.n = copy(b.buf[:], b.buf[overflow:b.n])
		}
		b.n += copy(b.buf[b.n:], p)
	}
	return n, nil
}

func (b *stderrBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf[:b.n])
}

// Run executes script in the remote login shell and returns stdout. On
// failure the error carries the tail of stderr so problems are diagnosable.
func (c *Client) Run(ctx context.Context, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	sess, err := c.openSession(ctx)
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", connLost(err))
	}
	// Wait does not close the channel. A vitals poll that leaves one
	// session open per cycle will eventually hit the server's MaxSessions
	// and freeze remote sampling for the rest of the dashboard.
	defer sess.Close()
	var stderr stderrBuf
	sess.Stderr = &stderr

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, oerr := sess.Output(script)
		done <- result{string(out), oerr}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return r.out, fmt.Errorf("remote command failed: %w%s", r.err, stderrTail(stderr.String()))
		}
		return r.out, nil
	case <-ctx.Done():
		sess.Close()
		// Wait (inside Output) joins the goroutine that copies the remote
		// stderr, so the tail below is only complete once Output has
		// returned. Closing the session is what makes it return; the grace
		// bounds the wait so a wedged channel cannot hold Run past its own
		// deadline, and costs nothing when the join is prompt.
		select {
		case <-done:
		case <-time.After(stderrDrainGrace):
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("remote command timed out: %w%s", ctx.Err(), stderrTail(stderr.String()))
		}
		return "", ctx.Err()
	}
}

// openSession opens a command channel under its own deadline. The two ways it
// can fail need different teardown:
//
//   - The caller's context is done. That is shutdown, and Close owns the
//     connection. Returning without waiting leaves the parked NewSession to be
//     released when Close closes the conn, which is the only thing that can
//     release it anyway; a reaper closes whatever it yields.
//   - sessionOpenTimeout expired with the caller still live. A peer that does
//     not answer a channel open at all is not slow, it is wedged (sshd out of
//     MaxSessions, a child that will not reap). Nothing recovers that, and the
//     parked NewSession holds a server-side session until the conn dies, so
//     the conn is closed here. That is deliberately not the per-command
//     runTimeout: a vitals poll that stalls must not take every forwarded
//     engine port down with it.
func (c *Client) openSession(ctx context.Context) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The outcome travels back on a channel rather than through captured
	// variables: a shared sess/err pair is still being written by the parked
	// open when the timeout or cancel arms, and reading it there races the
	// write. The channel is buffered so the goroutine never blocks on a
	// receiver that has already walked away.
	type openResult struct {
		sess *ssh.Session
		err  error
	}
	done := make(chan openResult, 1)
	go func() {
		sess, err := c.conn.NewSession()
		done <- openResult{sess: sess, err: err}
	}()
	timer := time.NewTimer(sessionOpenTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		if err := ctx.Err(); err != nil {
			if r.sess != nil {
				r.sess.Close()
			}
			return nil, err
		}
		return r.sess, r.err
	case <-timer.C:
		// A peer that never answers a channel open is wedged, not slow, and
		// the close below ends the connection. That is the difference between
		// a network blip and a host to investigate, so it is recorded before
		// the error reaches the caller that will drop the target.
		audit().Warn("toktop: ssh channel open unanswered",
			"target", logcfg.RedactedField(c.Target.LogHost(), 256),
			"wait", sessionOpenTimeout)
		c.conn.Close()
		// conn.Close is what releases the parked open; the session it may
		// still hand back belongs to the connection that just went away.
		r := <-done
		if r.sess != nil {
			r.sess.Close()
		}
		return nil, fmt.Errorf("ssh channel open unanswered after %s: %w", sessionOpenTimeout, context.DeadlineExceeded)
	case <-ctx.Done():
		// Shutdown: Close owns the connection and the channel with it, so
		// there is nothing to wait for here. Reap in the background rather
		// than reading the pair inline: the open is still parked, and Close
		// is what releases it. A session that lands after the drop is closed.
		go func() {
			if r := <-done; r.sess != nil {
				r.sess.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// stderrTailClusters bounds how much of a peer's stderr is quoted into a
// connection error. Counted in grapheme clusters, like every other retained
// field: the last line of a Python traceback is the part the operator needs,
// and a cut that splits an emoji or drops a combining mark mangles the one
// line that was readable.
const stderrTailClusters = 300

func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = core.SanitizeText(s)
	if s == "" {
		return ""
	}
	s = core.TailClusters(s, stderrTailClusters)
	if s == "" {
		return ""
	}
	return ": " + s
}

// connLost classifies session-open failures so callers can tell a dead
// connection from a transient hiccup. The original cause stays wrapped: the
// classification label is for branching, not a replacement for detail.
func connLost(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "connection lost") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "closed") {
		return fmt.Errorf("ssh connection lost: %w", err)
	}
	return err
}

// Forward binds a local listener per remote port and pipes every accepted
// connection through an ssh TCP channel. The returned map (remote port ->
// bound local port) is usable the moment this returns.
//
// A port already forwarded keeps the listener it has and reports the same
// local port, so forwarding the same set twice returns the same map and binds
// nothing new. A second listener for a port would relay to the same place but
// under an address nothing knows, and every caller holding the first map would
// leave it bound and relaying for the life of the connection.
func (c *Client) Forward(rports []int) (map[int]int, error) {
	out := make(map[int]int, len(rports))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, net.ErrClosed
	}
	var lastErr error
	seen := make(map[int]bool, len(rports))
	for _, rp := range rports {
		if seen[rp] {
			continue // a duplicate would bind a second listener no map entry reaches
		}
		seen[rp] = true
		if local, ok := c.forwards[rp]; ok {
			out[rp] = local
			continue
		}
		l, err := listenEphemeralAvoiding(rports, out)
		if err != nil {
			lastErr = err
			continue
		}
		if c.forwards == nil {
			c.forwards = make(map[int]int, len(rports))
		}
		local := l.Addr().(*net.TCPAddr).Port
		c.forwards[rp] = local
		out[rp] = local
		c.listeners = append(c.listeners, l)
		go c.relay(l, rp)
	}
	if len(out) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("no local ports available for forwarding: %w", lastErr)
		}
		return nil, fmt.Errorf("no local ports available for forwarding")
	}
	return out, nil
}

// listenEphemeralAvoiding binds a kernel-chosen loopback port that collides
// with no remote port still to be forwarded and none already mapped. The
// ephemeral range overlaps the ports an inference host serves on, so an
// unchecked bind can return (say) 45000 for the 40000 forward and silently
// point one engine at another's relay. Rebind until the pick is clear.
func listenEphemeralAvoiding(rports []int, taken map[int]int) (net.Listener, error) {
	for range forwardBindAttempts {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		clash := taken[port] != 0
		for _, rp := range rports {
			if rp == port {
				clash = true
				break
			}
		}
		if !clash {
			return l, nil
		}
		// The bind succeeded and is being given up. A close that fails leaks
		// the descriptor this loop is about to add another of, so it ends the
		// attempt instead of retrying over the leak.
		if cerr := l.Close(); cerr != nil {
			return nil, fmt.Errorf("cannot release loopback port %d: %w", port, cerr)
		}
	}
	return nil, fmt.Errorf("no loopback port free of the forwarded set after %d attempts", forwardBindAttempts)
}

func (c *Client) relay(l net.Listener, rport int) {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(rport))
	for {
		local, err := l.Accept()
		if err != nil {
			return // listener closed by Close()
		}
		if !c.acquireRelay(local) {
			local.Close()
			continue
		}
		go func(local net.Conn) {
			defer c.releaseRelay(local)
			defer local.Close()
			ctx, cancel := context.WithTimeout(context.Background(), forwardDialTimeout)
			defer cancel()
			remote, derr := c.conn.DialContext(ctx, "tcp", target)
			if derr != nil {
				// A forward that cannot reach the remote engine closes the
				// local socket with the reason lost, so the dashboard shows
				// the local engine refusing a connection it never made. The
				// rate limit keeps a permanently down engine from writing one
				// line per poll.
				c.auditForwardFailure(rport, derr)
				return
			}
			defer remote.Close()
			piped := make(chan struct{}, 2)
			go func() {
				_, cerr := io.Copy(remote, local)
				if cerr != nil {
					c.auditForwardFailure(rport, cerr)
				}
				piped <- struct{}{}
			}()
			go func() {
				_, cerr := io.Copy(local, remote)
				if cerr != nil {
					c.auditForwardFailure(rport, cerr)
				}
				piped <- struct{}{}
			}()
			<-piped
		}(local)
	}
}

// forwardWarnInterval is the shortest gap between two forwarded-port failure
// lines for the same port. The relay is entered once per local connection, and
// the dashboard opens one per poll, so an unthrottled line would be one per
// poll for as long as the remote engine stayed down.
const forwardWarnInterval = time.Minute

// auditForwardFailure records that a forwarded port could not be piped, at
// most once per forwardWarnInterval per port. The port and the remote target
// name what failed; the cause is the transport's own error, because "dial
// tcp: connection refused" and "connection lost" call for different fixes and
// the local dashboard reports both as a refused connection.
//
// The gap is measured on the monotonic reading time.Now carries, not on unix
// nanoseconds. A wall-clock difference is wrong on either side of a step: a
// backward NTP correction or a laptop resuming from sleep makes now-last
// negative, which is under the interval, and the throttle then suppresses
// every further line until real time passes the value it lost, so a remote
// engine that stayed down for an hour after the step said nothing at all.
func (c *Client) auditForwardFailure(rport int, err error) {
	now := time.Now()
	c.forwardWarnMu.Lock()
	if last, seen := c.forwardWarnAt[rport]; seen && now.Sub(last) < forwardWarnInterval {
		c.forwardWarnMu.Unlock()
		return
	}
	if c.forwardWarnAt == nil {
		c.forwardWarnAt = make(map[int]time.Time)
	}
	c.forwardWarnAt[rport] = now
	c.forwardWarnMu.Unlock()
	audit().Warn("toktop: ssh forward failed",
		"target", logcfg.RedactedField(c.Target.LogHost(), 256),
		"port", c.Target.Port,
		"forwarded_port", rport,
		"error", logcfg.RedactedField(c.Target.RedactUser(err.Error()), 256))
}

// acquireRelay records local as a connection this client is piping, or refuses
// it when the cap is full or the client is already stopping. The slot and the
// WaitGroup are taken under the same lock closeRelays uses to snapshot the
// set, so a teardown cannot miss a connection that has passed the cap check
// and not yet been registered.
func (c *Client) acquireRelay(local net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	if c.relayActive == nil {
		c.relayActive = make(chan struct{}, maxConcurrentRelays)
	}
	if c.relays == nil {
		c.relays = map[net.Conn]struct{}{}
	}
	select {
	case c.relayActive <- struct{}{}:
	default:
		return false
	}
	c.relays[local] = struct{}{}
	c.relayDone.Add(1)
	return true
}

func (c *Client) releaseRelay(local net.Conn) {
	c.mu.Lock()
	delete(c.relays, local)
	gate := c.relayActive
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	c.relayDone.Done()
}

// closeRelays unblocks and joins every connection a relay is piping. Closing
// one end of the pair releases both copy directions, so the wait is bounded by
// the close rather than by a peer that may never speak again.
func (c *Client) closeRelays() {
	c.mu.Lock()
	conns := make([]net.Conn, 0, len(c.relays))
	for l := range c.relays {
		conns = append(conns, l)
	}
	c.relays = map[net.Conn]struct{}{}
	c.mu.Unlock()
	for _, l := range conns {
		l.Close()
	}
	c.relayDone.Wait()
}

// closeListeners reclaims every local forward listener (and thereby its relay
// goroutine) and every connection one is piping. Idempotent; safe alongside
// Forward and Close.
func (c *Client) closeListeners() {
	c.mu.Lock()
	c.stopped = true
	for _, l := range c.listeners {
		l.Close()
	}
	c.listeners = nil
	c.forwards = nil
	c.mu.Unlock()
	c.closeRelays()
}

// Close tears down relays and the connection. Safe more than once, and
// complete even after the connection died on its own: listeners are reclaimed
// either way, here or by watchClose. It waits for the keepalive goroutine so
// no client goroutine outlives the call (and none can observe pacing changes
// made after teardown).
func (c *Client) Close() {
	c.closeMu.Lock()
	select {
	case <-c.closed:
	default:
		c.setErr(nil)
		close(c.closed)
	}
	c.closeMu.Unlock()

	c.closeListeners()

	c.conn.Close()
	<-c.keepaliveDone
}

// watchClose marks abnormal termination when the underlying connection dies
// outside Close(): Done must fire for any drop, deliberate shutdowns are
// already marked by Close before they reach this point. Call once at Connect
// time.
func (c *Client) watchClose() {
	werr := c.conn.Conn.Wait() // returns when the connection is torn down
	// Reclaim the forward listeners here rather than leaving it to a caller:
	// after an unattended drop nothing else tears this client down, and
	// listeners left bound would hold an fd and a relay goroutine apiece for
	// the rest of the process while serving connections that can never
	// succeed. Runs before Done fires so a woken observer sees them gone.
	c.closeListeners()
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	select {
	case <-c.closed: // deliberate shutdown, not a loss
		return
	default:
	}
	// Record the loss before Done fires: everything woken by Done must see
	// the reason in Err instead of racing this assignment.
	if werr != nil {
		c.setErr(fmt.Errorf("ssh connection lost: %w", werr))
	} else {
		c.setErr(fmt.Errorf("ssh connection lost"))
	}
	// The drop is the end of every number this host contributed: the forwarded
	// engines stop answering and the vitals loop gives up. Up to here the only
	// record was one stderr line under the alt screen.
	audit().Error("toktop: ssh connection lost",
		"target", logcfg.RedactedField(c.Target.LogHost(), 256),
		"port", c.Target.Port,
		"uptime", time.Since(c.connectedAt).Round(time.Second),
		"error", logcfg.RedactedField(c.Target.RedactUser(c.Err().Error()), 256))
	close(c.closed)
}

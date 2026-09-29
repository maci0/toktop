// Package remote attaches toktop to engines on other hosts over one
// long-lived ssh connection: target parsing, host key handling, discovery of
// listening inference ports and periodic vitals sampling.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// This file holds the connection itself: the Client state, the handshake and
// the keepalive that keeps it up, and the teardown every drop goes through.
// Running a command over it is session.go, and the forwarded local listeners
// are forward.go.

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

	// user is the login the session was opened with, which is not always
	// Target.User: a target written without one (ssh://box) is opened as the
	// operator's own account. Kept because the peer's own words about its home
	// have to be folded by the account they name, and the account is only
	// known here, once the transport has resolved it.
	user string

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
	login := t.userOr(currentUser())
	cfg := &ssh.ClientConfig{
		User:            login,
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
	c := &Client{Target: t, user: login, conn: ssh.NewClient(cc, chans, reqs), closed: make(chan struct{}),
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
// tests can shrink them.
var (
	keepaliveEvery   = 15 * time.Second
	keepaliveReplies = 5 * time.Second
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

// Close tears down relays and the connection. Safe more than once, and
// complete even after the connection died on its own: listeners are reclaimed
// either way, here or by watchClose. It waits for the keepalive goroutine so
// no client goroutine outlives the call (and none can observe pacing changes
// made after teardown).
//
// The connection goes down before the relays are joined, not after. A relay
// parked in a dial through the ssh transport holds its slot until that dial
// returns, and the only thing that makes it return is the connection closing:
// sequenced the other way, Close waits out forwardDialTimeout on a client it
// is holding the one release for.
func (c *Client) Close() {
	c.closeMu.Lock()
	select {
	case <-c.closed:
	default:
		c.setErr(nil)
		close(c.closed)
	}
	c.closeMu.Unlock()

	c.conn.Close()

	c.closeListeners()

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

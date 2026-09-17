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
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/maci0/toktop/internal/core"
)

// runTimeout bounds one remote command (discovery or vitals poll). Var so
// tests can shrink it, like bannerTimeout and the keepalive pacing below.
var runTimeout = 15 * time.Second

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
	stopped   bool

	keepaliveDone chan struct{} // closed when the keepalive goroutine exits
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

var currentUser = func() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return basenameLogin(u.Username)
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

// dialTimeout bounds TCP connect and DNS. ssh.ClientConfig.Timeout is only
// consulted by ssh.Dial; Connect opens the socket itself then calls
// NewClientConn, so without this a blackholed host hangs until the caller
// cancels. Var so tests can shrink it.
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

// Connect establishes an authenticated connection to t. Credentials are tried
// in order: explicit key file, config/default keys, agent, then a password
// (TOKTOP_SSH_PASSWORD first, else an interactive prompt when stdin is a
// TTY). Host keys are trust-on-first-use with change detection.
func Connect(ctx context.Context, t Target) (*Client, error) {
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
		Timeout:         dialTimeout, // unused by NewClientConn; the Dialer below carries it
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
	cc, chans, reqs, err := ssh.NewClientConn(hc, addr, cfg)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.userHost(), authHint(err))
	}
	if err := hc.SetDeadline(time.Time{}); err != nil {
		cc.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.userHost(), err)
	}
	c := &Client{Target: t, conn: ssh.NewClient(cc, chans, reqs), closed: make(chan struct{}),
		keepaliveDone: make(chan struct{})}
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

// keepalive turns silent network death into a closed connection within about
// a minute (3 unanswered probes) instead of a hung session. Each probe races
// a bounded wait: SendRequest alone would block forever on a peer that stops
// replying without closing TCP, so the miss counter would never advance.
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
		if misses >= 3 {
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("remote command timed out: %w%s", ctx.Err(), stderrTail(stderr.String()))
		}
		return "", ctx.Err()
	}
}

func (c *Client) openSession(ctx context.Context) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var (
		sess *ssh.Session
		err  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sess, err = c.conn.NewSession()
	}()
	select {
	case <-done:
		if ctx.Err() != nil {
			if sess != nil {
				sess.Close()
			}
			return nil, ctx.Err()
		}
		return sess, err
	case <-ctx.Done():
		c.conn.Close()
		<-done
		if sess != nil {
			sess.Close()
		}
		return nil, ctx.Err()
	}
}

func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = core.SanitizeText(s)
	if s == "" {
		return ""
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
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
func (c *Client) Forward(rports []int) (map[int]int, error) {
	out := make(map[int]int, len(rports))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, net.ErrClosed
	}
	for _, rp := range rports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		out[rp] = l.Addr().(*net.TCPAddr).Port
		c.listeners = append(c.listeners, l)
		go c.relay(l, rp)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no local ports available for forwarding")
	}
	return out, nil
}

func (c *Client) relay(l net.Listener, rport int) {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(rport))
	for {
		local, err := l.Accept()
		if err != nil {
			return // listener closed by Close()
		}
		go func(local net.Conn) {
			defer local.Close()
			ctx, cancel := context.WithTimeout(context.Background(), forwardDialTimeout)
			defer cancel()
			remote, derr := c.conn.DialContext(ctx, "tcp", target)
			if derr != nil {
				return
			}
			defer remote.Close()
			piped := make(chan struct{}, 2)
			go func() { io.Copy(remote, local); piped <- struct{}{} }()
			go func() { io.Copy(local, remote); piped <- struct{}{} }()
			<-piped
		}(local)
	}
}

// closeListeners reclaims every local forward listener (and thereby its relay
// goroutine). Idempotent; safe alongside Forward and Close.
func (c *Client) closeListeners() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	for _, l := range c.listeners {
		l.Close()
	}
	c.listeners = nil
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
		close(c.closed)
	}
	c.closeMu.Unlock()

	c.closeListeners()

	c.setErr(nil)
	c.conn.Close()
	<-c.keepaliveDone
}

// watchClose marks abnormal termination when the underlying connection dies
// outside Close(): Done must fire for any drop, deliberate shutdowns are
// already marked by Close before they reach this point. Call once at Connect
// time.
func (c *Client) watchClose() {
	c.conn.Conn.Wait() // returns when the connection is torn down
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
	c.setErr(fmt.Errorf("ssh connection lost"))
	close(c.closed)
}

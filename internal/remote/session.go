package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/maci0/toktop/internal/core"
	"github.com/maci0/toktop/internal/logcfg"
)

// Running a command over the connection: the session open and its deadlines,
// the stderr the peer produced, and the folds applied to the error text before
// it leaves the client. Forwarding a remote port is forward.go.

const stderrBufferBytes = 4096

// stderrDrainGrace bounds how long a cancelled or timed-out Run waits for the
// command's own Wait to join the stderr copier. Well under runTimeout, so the
// wait can never turn a bounded command into an unbounded one.
const stderrDrainGrace = time.Second

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

// stdoutCap bounds what a peer can make this process hold. The peer answers
// the exec channel, not toktop: the scripts toktop sends are fixed, but the
// bytes that come back are whatever the far end chose to write, for the whole
// of runTimeout. Without a cap a host the operator points --target at
// streams stdout at line rate until the operator's dashboard process is
// killed, and the per-line cut the discovery sweep applies lands after the
// whole thing has already been buffered.
//
// The value leaves headroom over a real sweep: procScanScript emits at most
// procs.CmdlinePrefix bytes of command line per process, so 16 MiB covers a
// host with thousands of processes before the cap can touch a healthy one.
const stdoutCap = 16 << 20

// errStdoutOverflow reports a peer that wrote past stdoutCap. It is distinct
// from a command failure because the output it interrupted is incomplete, and
// a caller that parsed the prefix anyway would report a short process table
// as the real one.
var errStdoutOverflow = errors.New("remote command produced more output than the cap allows")

// stdoutBuf collects a remote command's stdout up to stdoutCap. Past the cap
// Write fails instead of growing: the failure is what stops x/crypto/ssh's
// copy loop, so an over-long answer is a loud error rather than a truncated
// list that reads as a complete one. The prefix collected so far is still
// returned, because it is what makes the overflow diagnosable.
type stdoutBuf struct {
	mu   sync.Mutex
	buf  []byte
	n    int
	over bool
}

func (b *stdoutBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n+len(p) > stdoutCap {
		if room := stdoutCap - b.n; room > 0 {
			b.buf = append(b.buf, p[:room]...)
			b.n = stdoutCap
		}
		b.over = true
		return 0, errStdoutOverflow
	}
	b.buf = append(b.buf, p...)
	b.n += len(p)
	return len(p), nil
}

func (b *stdoutBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Overflowed reports whether the peer wrote past the cap.
func (b *stdoutBuf) Overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over
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
	// Run rather than Output, so stdout lands in the cap above. Output
	// assigns a plain bytes.Buffer to Stdout with no bound, which is the
	// peer-controlled growth this replaces.
	var stdout stdoutBuf
	sess.Stdout = &stdout

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		done <- result{sess.Run(script)}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			if stdout.Overflowed() {
				return stdout.String(), c.redactPeerHome(fmt.Errorf("remote command: %w", errStdoutOverflow))
			}
			return stdout.String(), c.redactPeerHome(fmt.Errorf("remote command failed: %w%s", r.err, stderrTail(stderr.String())))
		}
		return stdout.String(), nil
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
			return "", c.redactPeerHome(fmt.Errorf("remote command timed out: %w%s", ctx.Err(), stderrTail(stderr.String())))
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

// redactPeerHome folds the remote account's home directory out of a peer's
// own words before they leave Run. Everything built from what a remote script
// printed is quoted into the frame, the --json report and the audit log, and
// those outlive the run: a vitals script failing in the peer's login shell
// reports the path of the account it ran as, which core.RedactHome cannot
// touch because the local account is a different one.
func (c *Client) redactPeerHome(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(core.RedactUserHome(c.account(), err.Error()))
}

// account is the login the peer's shell runs as, for the fold in
// redactPeerHome. The resolved login is the account whose home the peer's own
// errors name; Target.User alone would name none of them for a target written
// as a bare host, which is the spelling in the README.
func (c *Client) account() string {
	if c.user != "" {
		return c.user
	}
	return c.Target.User
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
	// SingleLine, not SanitizeText: a peer's stderr reaches the operator's
	// terminal through the connection error, and the newlines it keeps would
	// let a host the operator chose to watch forge lines that read as
	// toktop's own. Folding to one line is the same collapse logcfg.Field
	// applies to every audit line carrying the same text.
	s = core.SingleLine(s)
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

package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/maci0/toktop/internal/logcfg"
)

// Port forwarding over the connection: the local listeners bound for the
// remote ports, the relays piping each accepted connection through an ssh
// channel, and the caps and throttles that keep one from running away.
// Connecting the client itself is client.go; running a command is session.go.

// forwardDialTimeout bounds opening a tunneled TCP channel: Accept is
// unbounded (the listener lives as long as the client), but a hung Dial would
// pin a goroutine and the accepted fd per incoming connection until the ssh
// conn itself dies.
//
// forwardAcceptRetry paces the relay after an Accept that failed for a
// reason other than Close. Without it a descriptor-exhausted process
// would spin on the failed call, and with it the port keeps answering
// once the descriptors come back.
var (
	forwardDialTimeout = 8 * time.Second
	forwardAcceptRetry = 100 * time.Millisecond
)

// forwardBindAttempts bounds the rebind loop that steers a kernel-chosen
// ephemeral port away from the forwarded set.
const forwardBindAttempts = 8

// maxConcurrentRelays caps how many forwarded connections one client pipes at
// once. A loopback port handed to a client is not a secret, so nothing bounds
// how many connections can arrive; each costs a file descriptor, two copy
// goroutines and an ssh channel, and a peer that holds the socket open without
// speaking holds all three until teardown. Past the cap the accepted
// connection is closed without being piped, so the local client sees the
// connection dropped with nothing read.
const maxConcurrentRelays = 64

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
			// Close is not the only way Accept returns. A descriptor
			// exhausted at accept time, or a connection the kernel aborted,
			// ends the goroutine just as surely, and the listener stays in
			// the client's set: Forward keeps reporting the port as bound
			// while nothing accepts on it, so every dashboard poll times out
			// with no line anywhere saying why.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			c.auditForwardFailure(rport, err)
			time.Sleep(forwardAcceptRetry)
			continue
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
			piped := make(chan error, 2)
			go func() {
				_, cerr := io.Copy(remote, local)
				piped <- cerr
			}()
			go func() {
				_, cerr := io.Copy(local, remote)
				piped <- cerr
			}()
			// Only the copy that ends first names the forward. Its peer is
			// stopped by the half-closes below and reports the error those
			// closes caused, which is not a failure of the forward.
			first := <-piped
			if !halfClose(local) || !halfClose(remote) {
				// No half-close on this conn type, so the peer copy stays
				// parked in io.Copy until the deferred Closes run, which is
				// after this function returns. Close here instead, so the
				// join below waits on a copy that is already finishing.
				local.Close()
				remote.Close()
			}
			<-piped
			if first != nil {
				c.auditForwardFailure(rport, first)
			}
		}(local)
	}
}

// halfClose shuts down the write side of a relayed conn so the copy running in
// the other direction sees EOF and returns. Reports whether the conn supports
// it: a conn without CloseWrite (a test double, a wrapped type) has to be
// closed outright to unblock its peer copy.
func halfClose(conn net.Conn) bool {
	hw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return false
	}
	return hw.CloseWrite() == nil
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

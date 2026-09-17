package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// fakePublicKey derives a deterministic ed25519-backed ssh.PublicKey.
func fakePublicKey(label string) ssh.PublicKey {
	seed := sha256.Sum256([]byte("toktop-test:" + label))
	pub, _, err := ed25519.GenerateKey(bytes.NewReader(seed[:]))
	if err != nil {
		panic(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		panic(err)
	}
	return pk
}

// testSSHServer is an in-process sshd covering exactly what the client uses:
// publickey/password/keyboard-interactive auth, exec sessions and
// direct-tcpip channels. This keeps the whole remote path testable without
// spawning an external ssh binary anywhere.
type testSSHServer struct {
	lis      net.Listener
	hostKey  ssh.Signer
	password string // empty => publickey-only

	mu    sync.Mutex
	conns []net.Conn
}

func newTestSSHServer(t *testing.T, password string, port int) *testSSHServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("session handler shells out to sh")
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privAny := any(priv)
	signer, err := ssh.NewSignerFromKey(privAny)
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHServer{hostKey: signer, password: password}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			if password != "" {
				return nil, fmt.Errorf("publickey disabled") // force password paths when configured
			}
			return &ssh.Permissions{}, nil
		},
	}
	if password != "" {
		cfg.PasswordCallback = func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) != password {
				return nil, fmt.Errorf("wrong password")
			}
			return &ssh.Permissions{}, nil
		}
		cfg.KeyboardInteractiveCallback = func(_ ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			ans, err := challenge("auth", "", []string{"Password:"}, []bool{false})
			if err != nil || len(ans) != 1 || ans[0] != password {
				return nil, fmt.Errorf("ki denied")
			}
			return &ssh.Permissions{}, nil
		}
	}
	cfg.AddHostKey(signer)

	var lis net.Listener
	if port > 0 {
		lis, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	} else {
		lis, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	s.lis = lis
	go s.accept(cfg)
	return s
}

func (s *testSSHServer) Port() int {
	return s.lis.Addr().(*net.TCPAddr).Port
}

func (s *testSSHServer) Close() {
	s.lis.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
}

func (s *testSSHServer) accept(cfg *ssh.ServerConfig) {
	for {
		nc, err := s.lis.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
		if err != nil {
			nc.Close()
			continue
		}
		go s.handleConn(conn, chans, reqs)
	}
}

func (s *testSSHServer) handleConn(conn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	defer conn.Close()
	go func() { // drain global requests; keepalive gets automatic replies
		for range reqs {
		}
	}()
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			ch, creqs, err := nch.Accept()
			if err != nil {
				continue
			}
			go s.serveSession(ch, creqs)
		case "direct-tcpip":
			s.serveDirect(nch)
		default:
			nch.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (s *testSSHServer) serveSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			var p struct{ Command string }
			if ssh.Unmarshal(req.Payload, &p) != nil {
				req.Reply(false, nil)
				continue
			}
			// Reply before running, the way real sshd does: x/crypto
			// attaches its stdout/stderr copiers only once this reply
			// arrives, and a late reply would mean no streaming at all
			// while the command runs.
			req.Reply(true, nil)
			cmd := exec.Command("sh", "-c", p.Command)
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			err := cmd.Run()
			status := uint32(0)
			if err != nil {
				if ee, ok := errors.AsType[*exec.ExitError](err); ok {
					status = uint32(ee.ExitCode())
				} else {
					status = 127
				}
			}
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			return // one command per session
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (s *testSSHServer) serveDirect(nch ssh.NewChannel) {
	var p struct {
		Addr string
		Port uint32
		Orig string
		OPrt uint32
	}
	if ssh.Unmarshal(nch.ExtraData(), &p) != nil {
		nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	upstream, err := net.Dial("tcp", net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port))))
	if err != nil {
		nch.Reject(ssh.ConnectionFailed, "dial failed")
		return
	}
	ch, _, err := nch.Accept()
	if err != nil {
		upstream.Close()
		return
	}
	go func() {
		io.Copy(ch, upstream)
		ch.CloseWrite()
	}()
	go func() {
		io.Copy(upstream, ch)
		upstream.Close()
	}()
}

func TestCurrentUserPrefersUSERThenUSERNAME(t *testing.T) {
	t.Setenv("USER", "from-user")
	t.Setenv("USERNAME", "from-username")
	if got := currentUser(); got != "from-user" {
		t.Errorf("currentUser = %q, want from-user", got)
	}
	t.Setenv("USER", "")
	if got := currentUser(); got != "from-username" {
		t.Errorf("currentUser = %q, want from-username", got)
	}
}

func TestBasenameLoginStripsWindowsDomain(t *testing.T) {
	cases := map[string]string{
		"alice":           "alice",
		`CORP\alice`:      "alice",
		"CORP/alice":      "alice",
		`CORP\unit\alice`: "alice",
	}
	for in, want := range cases {
		if got := basenameLogin(in); got != want {
			t.Errorf("basenameLogin(%q) = %q, want %q", in, got, want)
		}
	}
}

func testTarget(t *testing.T, port int) Target {
	t.Helper()
	t.Setenv("USER", "tester") // currentUser() reads it; scoped to this test
	// A key of its own: without one the client offers no auth method at all
	// on a machine with an empty ~/.ssh and no agent, and with one it would
	// be authenticating with the developer's real key. The test server
	// accepts any public key, so only its presence matters.
	return Target{User: "tester", Host: "127.0.0.1", Port: port, KeyFile: testKeyFile(t)}
}

// testKeyFile writes a throwaway private key and returns its path.
func testKeyFile(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withKnownHosts(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := knownHostsPath
	knownHostsPath = func() string { return dir + "/known_hosts" }
	t.Cleanup(func() { knownHostsPath = old })
}

func withFastKeepalive(t *testing.T) {
	t.Helper()
	oldEvery, oldWait := keepaliveEvery, keepaliveReplies
	keepaliveEvery, keepaliveReplies = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { keepaliveEvery, keepaliveReplies = oldEvery, oldWait })
}

func TestClientConnectRunForward(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()
	if err := cli.Err(); err != nil {
		t.Fatalf("fresh connection reported a loss: %v", err)
	}

	out, err := cli.Run(t.Context(), "echo hello-remote")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out); got != "hello-remote" {
		t.Errorf("run output = %q", got)
	}
	// Each Run must close its session; leaving them open would eventually
	// refuse new channels. A few dozen back-to-back commands is well past
	// a typical MaxSessions without being slow.
	for i := range 32 {
		if _, err := cli.Run(t.Context(), "true"); err != nil {
			t.Fatalf("run %d after prior sessions: %v", i, err)
		}
	}

	// Relay an engine-ish HTTP-less TCP service: raw echo.
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	rport := up.Addr().(*net.TCPAddr).Port
	fwd, err := cli.Forward([]int{rport})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	lc, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(fwd[rport]))
	if err != nil {
		t.Fatalf("relay dial: %v", err)
	}
	fmt.Fprint(lc, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(lc, buf); err != nil {
		t.Fatalf("relay roundtrip read: %v", err)
	}
	lc.Close()
	if string(buf) != "ping" {
		t.Errorf("relay roundtrip = %q", buf)
	}

	// Close must be idempotent, fire Done, and stay a non-loss: Err must
	// not grow a reason for a shutdown the operator asked for.
	done := cli.Done()
	cli.Close()
	cli.Close()
	select {
	case <-done:
	default:
		t.Error("Done should be closed after Close")
	}
	if got := cli.Err(); got != nil {
		t.Errorf("deliberate Close recorded a loss: %v", got)
	}
}

// The keepalive goroutine must exit promptly when the client is torn down,
// not keep ticking on a dead connection for its full miss window.
func TestKeepaliveStopsOnClose(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	done := cli.keepaliveDone
	cli.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive goroutine still running 2s after Close")
	}
}

// newSilentSSHServer completes handshakes and then goes dark: channels and
// global requests are drained but never serviced, TCP stays open. It models
// a blackholed route or a hung remote sshd, the failure mode keepalive must
// detect.
func newSilentSSHServer(t *testing.T) *testSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHServer{lis: lis, hostKey: signer}
	go func() {
		for {
			nc, err := lis.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, nc)
			s.mu.Unlock()
			go func(nc net.Conn) {
				conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				defer conn.Close()
				go func() { // accept no channels
					for range chans {
					}
				}()
				for range reqs { // reply to no global requests
				}
				// hold TCP until the client gives up on us
			}(nc)
		}
	}()
	t.Cleanup(s.Close)
	return s
}

// A peer that stops replying without closing TCP must be force-closed by
// keepalive after three unanswered probes; Done must fire.
func TestKeepaliveDetectsSilentPeer(t *testing.T) {
	withKnownHosts(t)
	withFastKeepalive(t)

	srv := newSilentSSHServer(t)
	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	select {
	case <-cli.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("silent peer not detected within probe window")
	}
	// The drop must carry its reason: Done alone says only that it ended.
	if got := cli.Err(); got == nil || !strings.Contains(got.Error(), "lost") {
		t.Fatalf("silent peer drop reported %v, want the loss reason", got)
	}
}

// stallingHost listens on an ephemeral port and accepts one connection,
// writing banner first when non-empty, then holds it open without sending
// anything more. The port is the Connect target.
func stallingHost(t *testing.T, banner string) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := lis.Accept()
		if err == nil {
			if banner != "" {
				fmt.Fprint(c, banner)
			}
			accepted <- c // hold TCP open
		}
	}()
	t.Cleanup(func() {
		select {
		case c := <-accepted:
			c.Close()
		default:
		}
	})
	return lis.Addr().(*net.TCPAddr).Port
}

// A host that never answers SYN must fail Connect at dialTimeout, not hang
// until the caller cancels. ssh.ClientConfig.Timeout does not apply to the
// DialContext+NewClientConn path; the Dialer has to carry the bound itself.
func TestConnectFailsOnUnreachableHost(t *testing.T) {
	withKnownHosts(t)
	old := dialTimeout
	dialTimeout = 50 * time.Millisecond
	t.Cleanup(func() { dialTimeout = old })

	start := time.Now()
	// TEST-NET-1 is reserved documentation space; a blackhole hangs until
	// the dial deadline, an ICMP unreachable fails immediately. Either is
	// a correct failure as long as it is not unbounded.
	_, err := Connect(t.Context(), Target{
		User: "tester", Host: "192.0.2.1", Port: 22, KeyFile: testKeyFile(t),
	})
	if err == nil {
		t.Fatal("unreachable host must fail Connect")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("unreachable host took %v to fail, want ~dialTimeout", d)
	}
}

// A host that completes TCP accept but never sends its ssh banner must fail
// Connect promptly via the handshake deadline, not hang forever.
func TestConnectFailsOnSilentHost(t *testing.T) {
	withKnownHosts(t)
	old := bannerTimeout
	bannerTimeout = 50 * time.Millisecond
	t.Cleanup(func() { bannerTimeout = old })

	port := stallingHost(t, "")

	start := time.Now()
	_, err := Connect(t.Context(), testTarget(t, port))
	if err == nil {
		t.Fatal("silent host must fail Connect")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("silent host took %v to fail, want ~bannerTimeout", d)
	}
}

// A host that sends part of its banner and then stalls must also be cut by
// the handshake deadline: the bound may lift only once the banner terminator
// arrived. A peer trickling one byte and going dark would otherwise hang
// Connect forever - x/crypto/ssh consults neither our context nor any
// deadline of its own during the version exchange.
func TestConnectFailsOnPartialBanner(t *testing.T) {
	withKnownHosts(t)
	old := bannerTimeout
	bannerTimeout = 50 * time.Millisecond
	t.Cleanup(func() { bannerTimeout = old })

	port := stallingHost(t, "SSH-2.0-trickle") // no newline, then silence

	start := time.Now()
	_, err := Connect(t.Context(), testTarget(t, port))
	if err == nil {
		t.Fatal("partial-banner host must fail Connect")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("partial-banner host took %v to fail, want ~bannerTimeout", d)
	}
}

func TestClientRunBoundsSessionOpen(t *testing.T) {
	for _, cancelCall := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelCall), func(t *testing.T) {
			withKnownHosts(t)
			old := runTimeout
			runTimeout = 100 * time.Millisecond
			t.Cleanup(func() { runTimeout = old })
			srv := newSilentSSHServer(t)
			cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelCall {
				runTimeout = time.Minute
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			done := make(chan error, 1)
			go func() {
				_, err := cli.Run(ctx, "true")
				done <- err
			}()
			select {
			case err := <-done:
				want := context.DeadlineExceeded
				if cancelCall {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("Run error = %v, want %v", err, want)
				}
			case <-time.After(2 * time.Second):
				cli.Close()
				<-done
				t.Fatal("session open ignored timeout or cancellation")
			}
		})
	}
}

func TestClientRunFailureCarriesStderr(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()
	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	_, err = cli.Run(t.Context(), "i=0; while [ \"$i\" -lt 1000 ]; do echo diagnostic >&2; i=$((i+1)); done; echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected failure")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("boom")) {
		t.Errorf("error should carry stderr tail: %v", err)
	}
}

// A remote command exceeding runTimeout must fail promptly with a timeout
// notice; whatever stderr arrived before the cut rides along so a hung
// command is diagnosable from the error alone. The command keeps writing
// stderr across the deadline on purpose: the ssh package copies it from a
// background goroutine that outlives Output here, and reading the tail must
// stay synchronized with that writer (race detector coverage).
func TestClientRunTimesOutCarriesStderr(t *testing.T) {
	withKnownHosts(t)
	old := runTimeout
	runTimeout = 300 * time.Millisecond
	t.Cleanup(func() { runTimeout = old })

	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()
	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	start := time.Now()
	_, err = cli.Run(t.Context(), "yes stalled >&2 & sleep 5")
	if err == nil {
		t.Fatal("long command should hit runTimeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want timeout notice", err)
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("err should carry the streamed stderr: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout took %s, deadline not applied", elapsed)
	}
}

func TestClientPasswordAuth(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "secret", 0) // publickey rejected
	defer srv.Close()
	t.Setenv("TOKTOP_SSH_PASSWORD", "secret")

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("password connect: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Run(t.Context(), "true"); err != nil {
		t.Fatalf("run over password auth: %v", err)
	}
}

func TestClientWrongPasswordFails(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "secret", 0)
	defer srv.Close()
	t.Setenv("TOKTOP_SSH_PASSWORD", "wrong")

	_, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if !strings.Contains(err.Error(), "all credentials rejected") {
		t.Errorf("expected rejection hint, got: %v", err)
	}
}

func TestClientHostKeyChangeRefused(t *testing.T) {
	withKnownHosts(t)
	srv := newTestSSHServer(t, "", 0)
	port := srv.Port()
	cli, err := Connect(t.Context(), testTarget(t, port))
	if err != nil {
		t.Fatal(err)
	}
	cli.Close()
	srv.Close()

	// Same address, different host key.
	srv2 := newTestSSHServer(t, "", port)
	defer srv2.Close()
	_, err = Connect(t.Context(), testTarget(t, port))
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed host key must be refused, got: %v", err)
	}
}

// An abnormal drop must reclaim the forward listeners. After an unattended
// loss nobody calls Close (the attach-site watcher only reports it), so
// listeners left bound would hold an fd and a relay goroutine apiece for the
// rest of the dashboard while serving connections that can never succeed.
func TestDropReclaimsForwardListeners(t *testing.T) {
	withKnownHosts(t)
	withFastKeepalive(t)

	srv := newSilentSSHServer(t)
	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close() // no-op after the drop; must stay safe

	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	rport := up.Addr().(*net.TCPAddr).Port
	fwd, err := cli.Forward([]int{rport})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	laddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(fwd[rport]))

	select {
	case <-cli.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("silent peer not detected within probe window")
	}

	cli.mu.Lock()
	left := len(cli.listeners)
	cli.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d forward listener(s) still tracked after the drop", left)
	}
	if c, derr := net.DialTimeout("tcp", laddr, time.Second); derr == nil {
		c.Close()
		t.Fatal("forward listener still accepting after the connection dropped")
	}
	if got := cli.Err(); got == nil || !strings.Contains(got.Error(), "lost") {
		t.Fatalf("drop reported %v, want the loss reason", got)
	}
}

// A peer that never accepts tunneled channels must fail the local connection
// at forwardDialTimeout instead of pinning a goroutine and the accepted fd
// until keepalive finally kills the ssh conn.
func TestForwardDialTimesOutOnSilentPeer(t *testing.T) {
	withKnownHosts(t)
	old := forwardDialTimeout
	forwardDialTimeout = 100 * time.Millisecond
	t.Cleanup(func() { forwardDialTimeout = old })

	srv := newSilentSSHServer(t)
	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	fwd, err := cli.Forward([]int{1})
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	laddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(fwd[1]))
	start := time.Now()
	c, err := net.Dial("tcp", laddr)
	if err != nil {
		t.Fatalf("local forward dial: %v", err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err == nil {
		t.Fatal("hung remote Dial left the local connection open")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("local connection closed after %s, want ~forwardDialTimeout", elapsed)
	}
}

// connLost tags connection-death errors so callers can distinguish a dead
// tunnel from a transient hiccup; the original cause must stay unwrappable.
func TestConnLostClassification(t *testing.T) {
	cases := []struct {
		msg  string
		lost bool
	}{
		{"EOF", true},
		{"ssh: session closed", true},
		{"connection lost mid-read", true},
		{"dial tcp: connection refused", false},
		{"", false},
	}
	for _, c := range cases {
		in := errors.New(c.msg)
		got := connLost(in)
		if has := strings.Contains(got.Error(), "ssh connection lost"); has != c.lost {
			t.Errorf("connLost(%q) tagged=%v, want %v", c.msg, has, c.lost)
		}
		if !errors.Is(got, in) {
			t.Errorf("connLost(%q) dropped the wrapped cause", c.msg)
		}
	}
}

func TestStderrBufRetainsBoundedTail(t *testing.T) {
	var b stderrBuf
	var all string
	for _, size := range []int{0, 100, 4090, 32, 8192, 1, 0} {
		p := bytes.Repeat([]byte{byte('a' + size%26)}, size)
		n, err := b.Write(p)
		if n != len(p) || err != nil {
			t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(p))
		}
		all += string(p)
		want := all[max(0, len(all)-4096):]
		if got := b.String(); got != want {
			t.Fatalf("retained stderr length = %d, want tail length %d", len(got), len(want))
		}
	}
}

func TestStderrTailTruncatesToTail(t *testing.T) {
	if got := stderrTail("   \n "); got != "" {
		t.Errorf("blank stderr = %q, want empty", got)
	}
	if got := stderrTail("short"); got != ": short" {
		t.Errorf("short stderr = %q", got)
	}
	got := stderrTail(strings.Repeat("x", 400))
	want := ": " + strings.Repeat("x", 300)
	if got != want {
		t.Errorf("tail length = %d, want %d (300-byte cap)", len(got), len(want))
	}
}

func TestStderrTailCutsOnUTF8Boundary(t *testing.T) {
	// "é" is two bytes. A 301-byte string whose first byte is the lead of
	// that é would, sliced at len-300, start on the continuation byte and
	// yield invalid UTF-8 in the error line.
	in := "é" + strings.Repeat("x", 299)
	got := stderrTail(in)
	if !utf8.ValidString(got) {
		t.Errorf("stderrTail produced invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, ": ") {
		t.Errorf("stderrTail = %q, want ': ' prefix", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("stderrTail left a replacement character from a split é: %q", got)
	}
}

func TestStderrTailStripsTerminalInjection(t *testing.T) {
	got := stderrTail("ok\x1b]52;c;QUJD\x07tail")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Errorf("stderrTail retained escape bytes: %q", got)
	}
	if got != ": oktail" {
		t.Errorf("stderrTail = %q, want visible text kept", got)
	}
}

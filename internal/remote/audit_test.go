package remote

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is the bytes.Buffer the audit logger writes through, with the
// lock a plain one lacks. The client's own goroutines keep logging after a
// drop (watchClose records the loss, keepalive the misses) while the test is
// already reading the lines, so an unsynchronized buffer is a data race the
// detector rightly reports. Nothing here runs in parallel, so one mutex is
// enough.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// captureAudit points the package logger at a buffer and restores it after,
// so the lines an operator would find in a journal can be asserted. Same
// package-var convention as withKnownHosts; nothing here runs in parallel.
func captureAudit(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := audit
	audit = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	t.Cleanup(func() { audit = old })
	return buf
}

func linesWith(buf *syncBuffer, sub string) []string {
	var out []string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// A remote that stops answering has to say so in the audit log, not only on
// the frame that happens to be drawn: the alt screen takes the dashboard's own
// notice with it. One line when the outage starts and one when it ends, so a
// host that is dark for a day does not write a line every poll.
func TestVitalsOutageIsAuditedOnceEachWay(t *testing.T) {
	withKnownHosts(t)
	logs := captureAudit(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	s := &Stats{Client: cli}
	s.poll(t.Context())
	if s.err != "" {
		t.Fatalf("successful poll recorded a failure: %q", s.err)
	}
	logs.Reset() // the connect line is asserted elsewhere

	cli.Close()
	for range 3 {
		s.poll(t.Context())
	}
	fails := linesWith(logs, "remote vitals poll failed")
	if len(fails) != 1 {
		t.Fatalf("audited failures = %d, want 1 for three failed polls: %v", len(fails), fails)
	}
	if !strings.Contains(fails[0], "tester@127.0.0.1") {
		t.Errorf("failure line does not name the target: %q", fails[0])
	}
	if !strings.Contains(fails[0], "level=WARN") {
		t.Errorf("failure logged below warn: %q", fails[0])
	}

	// Recovery needs a peer that answers, so the same Stats polls again over a
	// fresh connection: the failure count in the line is what says how many
	// polls the host was dark for.
	s.Client = cli2(t, srv)
	s.poll(t.Context())
	if s.err != "" {
		t.Fatalf("poll over the new connection failed: %q", s.err)
	}
	recovered := linesWith(logs, "remote vitals poll recovered")
	if len(recovered) != 1 {
		t.Fatalf("recovery lines = %d, want 1: %v", len(recovered), recovered)
	}
	if !strings.Contains(recovered[0], "failed_polls=3") {
		t.Errorf("recovery line does not carry the failure count: %q", recovered[0])
	}
}

// cli2 attaches a second client to the test server and closes it with the
// test, so a recovery can be observed on a connection that answers.
func cli2(t *testing.T, srv *testSSHServer) *Client {
	t.Helper()
	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(cli.Close)
	return cli
}

// The drop ends every number a remote host contributed, and the reason has to
// name the peer: an unanswered keepalive and a closed socket are different
// investigations.
func TestConnectionDropIsAudited(t *testing.T) {
	withKnownHosts(t)
	logs := captureAudit(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := linesWith(logs, "ssh connected"); len(got) != 1 {
		t.Fatalf("connect audit lines = %d, want 1: %v", len(got), got)
	}
	srv.Close() // the peer goes away, the client does not

	select {
	case <-cli.Done():
	case <-t.Context().Done():
		t.Fatal("connection drop never fired Done")
	}
	drops := linesWith(logs, "ssh connection lost")
	if len(drops) != 1 {
		t.Fatalf("drop audit lines = %d, want 1: %v", len(drops), drops)
	}
	if !strings.Contains(drops[0], "level=ERROR") || !strings.Contains(drops[0], "tester@127.0.0.1") {
		t.Errorf("drop line = %q, want an error naming the target", drops[0])
	}
	cli.Close()
}

// A refused credential is the security-relevant event in this package, and
// the message the operator saw is gone with the alt screen.
func TestConnectFailureIsAudited(t *testing.T) {
	withKnownHosts(t)
	logs := captureAudit(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	tgt := testTarget(t, srv.Port())
	tgt.KeyFile = t.TempDir() + "/absent_key" // no method the server accepts
	if _, err := Connect(t.Context(), tgt); err == nil {
		t.Fatal("connect with an absent key file succeeded")
	}
	fails := linesWith(logs, "ssh connect failed")
	if len(fails) != 1 {
		t.Fatalf("connect failure audit lines = %d, want 1: %v", len(fails), fails)
	}
	if !strings.Contains(fails[0], "level=WARN") || !strings.Contains(fails[0], "tester@127.0.0.1") {
		t.Errorf("connect failure line = %q, want a warn naming the target", fails[0])
	}
}

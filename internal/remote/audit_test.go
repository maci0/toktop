package remote

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// namesTarget reports whether an audit line names the peer it is about and
// leaves the account out of it. testTarget is the target every audit test
// connects with, so "tester" is the account a line must not carry: these
// lines outlive the run and get quoted into bug reports, and a login names a
// person on the host.
func namesTarget(line string) bool {
	return strings.Contains(line, "target=127.0.0.1") && !strings.Contains(line, "tester@")
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
	if !namesTarget(fails[0]) {
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

// The outage length is a duration, and the clock that measures it is a wall
// clock on a real run. An NTP correction or a laptop resuming from sleep moves
// it backwards while the host is dark, and the raw subtraction then recovers
// with "outage=-2h0m0s": a negative outage, which reads as a broken clock
// rather than as a host that was dark for the length it was dark.
func TestVitalsOutageIsNotNegativeAfterAClockStep(t *testing.T) {
	withKnownHosts(t)
	logs := captureAudit(t)
	srv := newTestSSHServer(t, "", 0)
	defer srv.Close()

	cli, err := Connect(t.Context(), testTarget(t, srv.Port()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	s := &Stats{Client: cli}
	s.SetNow(func() time.Time { return now })
	s.poll(t.Context())
	if s.err != "" {
		t.Fatalf("successful poll recorded a failure: %q", s.err)
	}
	logs.Reset() // the connect line is asserted elsewhere

	cli.Close()
	s.poll(t.Context())
	if s.err == "" {
		t.Fatal("failed poll recorded no reason")
	}

	// The clock steps back two hours while the host stays dark, then the
	// recovery is observed over a connection that answers.
	now = now.Add(-2 * time.Hour)
	s.Client = cli2(t, srv)
	s.poll(t.Context())
	if s.err != "" {
		t.Fatalf("poll over the new connection failed: %q", s.err)
	}
	recovered := linesWith(logs, "remote vitals poll recovered")
	if len(recovered) != 1 {
		t.Fatalf("recovery lines = %d, want 1: %v", len(recovered), recovered)
	}
	if !strings.Contains(recovered[0], "outage=0s") {
		t.Errorf("recovery after a backward step audited a negative outage: %q", recovered[0])
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
	if !strings.Contains(drops[0], "level=ERROR") || !namesTarget(drops[0]) {
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
	if !strings.Contains(fails[0], "level=WARN") || !namesTarget(fails[0]) {
		t.Errorf("connect failure line = %q, want a warn naming the target", fails[0])
	}
}

// A default key that is there and will not load is skipped with a line saying
// so, and that line's reason is where a path under the operator's home can
// sneak back in: a read failure names the file it failed on, and the file is
// ~/.ssh/id_rsa. The audit copy outlives the run and gets pasted into issues,
// so the home has to be folded out of the reason the way it already is out of
// the key attribute beside it. A name that is not there at all stays silent:
// most machines have no key under one of these three, and a line per absent
// name would bury the ones that matter.
func TestUnusableDefaultKeyIsAuditedWithoutTheHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads this one on Windows
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	disableAgent(t)
	logs := captureAudit(t)

	if _, _, err := (Target{Host: "box"}).authMethods(); err != nil {
		t.Fatalf("authMethods: %v", err)
	}
	warns := linesWith(logs, "default ssh key unusable")
	if len(warns) != 1 {
		t.Fatalf("audit lines for one unusable default key = %d, want 1: %v", len(warns), warns)
	}
	if strings.Contains(warns[0], home) {
		t.Errorf("audit line carries the home directory: %q", warns[0])
	}
	// The two names that are absent must not have produced a line of their own.
	if got := linesWith(logs, "id_rsa"); len(got) != 0 {
		t.Errorf("an absent default key was audited: %v", got)
	}
}

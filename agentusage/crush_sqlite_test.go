// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

//go:build sqlite

package agentusage

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// crushDB writes a database shaped like crush's own, with the sessions given.
// Each value is {completion_tokens, prompt_tokens, updated_at}.
// skipIfCrushAbove skips when a crush database sits in an ancestor of dir.
// crushDBPath walks up to the project root, and a t.TempDir tree's ancestors
// end at the system temp directory, so a crush install anywhere above it (a
// /tmp/.crush left by another run) is what the walk finds. A test asserting
// "nothing above this tree" cannot be right in that state, and reading the
// stray database instead of the tree would be a different assertion entirely.
func skipIfCrushAbove(t *testing.T, dir string) {
	t.Helper()
	for cur := filepath.Dir(dir); ; {
		path := filepath.Join(cur, crushDBRel)
		if _, err := os.Stat(path); err == nil {
			t.Skipf("a crush database exists above the test tree at %s", path)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return
		}
		cur = parent
	}
}

func crushDB(t *testing.T, dir string, sessions map[string][3]int64) {
	t.Helper()
	path := filepath.Join(dir, ".crush", "crush.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// One transaction for the whole fixture. In autocommit each INSERT is its
	// own durable commit, so 2000 autocommit inserts are 2000 fsyncs, which
	// pushed TestCrushSinceQueryCanUseUpdatedAtIndex past the suite timeout.
	// The table the tests query is the same either way.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Committed below, so the deferred rollback is the error path and its
	// error is not the one worth reporting.
	defer func() { _ = tx.Rollback() }()
	for id, v := range sessions {
		if _, err := tx.Exec(
			`INSERT INTO sessions (id, completion_tokens, prompt_tokens, updated_at) VALUES (?, ?, ?, ?)`,
			id, v[0], v[1], v[2]); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func putCrushSession(t *testing.T, dir, id string, output, input, updatedAt int64) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, ".crush", "crush.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO sessions (id, completion_tokens, prompt_tokens, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET completion_tokens = excluded.completion_tokens,
		     prompt_tokens = excluded.prompt_tokens,
		     updated_at = excluded.updated_at`,
		id, output, input, updatedAt); err != nil {
		t.Fatal(err)
	}
}

// What crush spent after attach is what it wrote while the watcher ran: rows
// from before it belong to whatever ran earlier.
func TestCrushSourceCountsOnlyThisReview(t *testing.T) {
	dir := t.TempDir()
	since := time.Now()
	crushDB(t, dir, map[string][3]int64{
		"earlier": {5000, 8000, since.Add(-time.Hour).UnixMilli()},
		"during":  {1200, 400, since.Add(time.Minute).UnixMilli()},
		"also":    {300, 100, since.Add(2 * time.Minute).UnixMilli()},
	})

	out, in, ok := crushSessionSum([]string{dir}, since)
	if !ok || out != 1500 {
		t.Fatalf("sessions output %d (ok=%v), want 1500 output tokens", out, ok)
	}
	if in != 500 {
		t.Fatalf("input %d, want 500 prompt tokens written after attach", in)
	}
}

// The rows carry two units: crush writes milliseconds, and the table's own
// update trigger writes seconds. Both must count, or a watcher reads zero.
func TestCrushSourceAcceptsSecondsAndMilliseconds(t *testing.T) {
	dir := t.TempDir()
	since := time.Now()
	crushDB(t, dir, map[string][3]int64{
		"millis":      {10, 0, since.Add(time.Minute).UnixMilli()},
		"seconds":     {20, 0, since.Add(time.Minute).Unix()},
		"old seconds": {40, 0, since.Add(-time.Hour).Unix()},
		"old millis":  {80, 0, since.Add(-time.Hour).UnixMilli()},
	})
	if out, _, ok := crushSessionSum([]string{dir}, since); !ok || out != 30 {
		t.Fatalf("sessions output %d (ok=%v), want the 30 written after attach in either unit", out, ok)
	}
}

// A row whose id is NULL is a row the schema permits: SQLite lets any PRIMARY
// KEY other than INTEGER PRIMARY KEY hold NULL unless it is declared NOT NULL,
// which crush's sessions table does not. The other two columns are already read
// as nullable, so this one NULL must not be what fails the statement and leaves
// the store reporting nothing for as long as the row survives.
func TestCrushSourceSkipsSessionWithoutID(t *testing.T) {
	dir := t.TempDir()
	since := time.Now()
	crushDB(t, dir, map[string][3]int64{"s": {40, 0, since.Add(time.Minute).UnixMilli()}})
	execSQL(t, filepath.Join(dir, ".crush", "crush.db"),
		`INSERT INTO sessions (id, completion_tokens, prompt_tokens, updated_at) VALUES (NULL, 900, 700, ?)`,
		since.Add(time.Minute).UnixMilli())
	out, in, ok := crushSessionSum([]string{dir}, since)
	if !ok {
		t.Fatal("a session row with no id failed the whole read of the store")
	}
	if out != 40 || in != 0 {
		t.Fatalf("output %d input %d, want the 40 of the identified session", out, in)
	}
}

// crush resolves the project root, so a worktree under the project reads the
// project's database rather than reporting nothing.
func TestCrushSourceFindsTheProjectDatabase(t *testing.T) {
	root := t.TempDir()
	since := time.Now()
	crushDB(t, root, map[string][3]int64{"s": {42, 0, since.Add(time.Second).UnixMilli()}})
	sub := filepath.Join(root, "worktrees", "sec-review")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, _, ok := crushSessionSum([]string{sub}, since); !ok || out != 42 {
		t.Fatalf("sessions output %d (ok=%v), want the project database's 42", out, ok)
	}
}

// Two spellings of one directory are one database, not two readings of it.
func TestCrushSourceCountsOneDatabaseOnce(t *testing.T) {
	dir := t.TempDir()
	since := time.Now()
	crushDB(t, dir, map[string][3]int64{"s": {100, 0, since.Add(time.Second).UnixMilli()}})
	if out, _, _ := crushSessionSum([]string{dir, dir + string(filepath.Separator)}, since); out != 100 {
		t.Fatalf("output %d, want 100 counted once", out)
	}
}

// A tree crush has never run in reports nothing, which is not an error.
func TestCrushSourceWithoutADatabase(t *testing.T) {
	dir := t.TempDir()
	skipIfCrushAbove(t, dir)
	got, ok := (crushDBSource{}).sessions([]string{dir}, time.Now())
	if !ok || len(got) != 0 {
		t.Fatalf("sessions %+v (ok=%v) from a tree with no crush database", got, ok)
	}
	// The walk climbs to a project root, so a store a parent of the temporary
	// tree happens to hold is a legitimate find and asserting on it would
	// make the test a statement about the machine rather than about the code.
	// What is the code's is the tree itself: nothing under it is read.
	for path := range got {
		if withinTree(path, dir) {
			t.Fatalf("read a store out of a tree that has none: %s", path)
		}
	}
}

// withinTree reports whether path is dir or sits under it.
func withinTree(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Watch routes crush through this source, which is what makes the counts
// reach a caller without it knowing where they came from. Tokens already in
// the database at attach belong to a previous run, the same rule the file
// adapters apply, so the session is written after Watch.
func TestWatchUsesTheCrushSource(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{})
	w := Watch("crush", dir, time.Now())
	if w == nil {
		t.Fatal("crush is readable in this build, so Watch must return a watcher")
	}
	putCrushSession(t, dir, "s", 7, 3, time.Now().Add(time.Second).UnixMilli())
	got := w.Poll()
	if got.Output != 7 || got.Input != 3 {
		t.Fatalf("watcher read %+v, want 7 output and 3 prompt tokens", got)
	}
	if !Supported("crush") {
		t.Fatal("Supported must agree that crush is readable here")
	}
}

// completion_tokens is cumulative for a session's life. A session that
// already had tokens when the watcher attached must contribute only what it
// adds afterwards, or a continued crush dumps its history into this attach.
func TestCrushWatchCountsOnlyGrowthAfterAttach(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{
		"s": {5000, 2000, time.Now().Add(-time.Hour).UnixMilli()},
	})
	w := Watch("crush", dir, time.Now())
	if w == nil {
		t.Fatal("crush is readable in this build, so Watch must return a watcher")
	}
	w.poll(nil)
	if got := w.Sample(); got.Output != 0 || got.Input != 0 {
		t.Fatalf("counted tokens from before attach: %+v", got)
	}
	putCrushSession(t, dir, "s", 5100, 2300, time.Now().Add(time.Second).UnixMilli())
	w.poll(nil)
	got := w.Sample()
	if got.Output != 100 {
		t.Fatalf("output %d, want the 100 generated after attach", got.Output)
	}
	if got.Input != 300 {
		t.Fatalf("input %d, want the 300 prompt tokens billed after attach", got.Input)
	}
}

// Two corrupt sessions whose totals pass the ceiling must read as enormous,
// the way a transcript with absurd counts does, and not as nothing. Refusing
// the reading instead loses every poll: the store is re-read from the attach
// instant each time, so the same total comes back and the agent is blind for
// as long as the rows stand.
func TestCrushWatchSaturatesCorruptSessionTotals(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("the ceiling does not fit in an int on this platform")
	}
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{})
	since := time.Now()
	w := Watch("crush", dir, since)
	if w == nil {
		t.Fatal("crush is readable in this build, so Watch must return a watcher")
	}
	at := since.Add(time.Second).UnixMilli()
	putCrushSession(t, dir, "a", maxSaneTokens, 0, at)
	putCrushSession(t, dir, "b", maxSaneTokens, 0, at)
	got := w.Poll()
	if got.Output != maxSaneTokens {
		t.Fatalf("output %d, want the ceiling %d rather than a dropped reading", got.Output, maxSaneTokens)
	}
	// A second poll sees the same rows and must report the same level, not
	// fall back to the last sample now that the totals have saturated.
	if next := w.Poll(); next.Output != got.Output {
		t.Fatalf("repeated poll reported %d, want %d", next.Output, got.Output)
	}
}

// A store that cannot be read at attach must not be treated as empty, and it
// must not become an empty baseline either: either way the first successful
// poll counts a continued session's whole history as growth. Snapshot only
// once a real store is attached, and count just what is added after.
func TestCrushWatchRetriesFailedAttachSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".crush", "crush.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := Watch("crush", dir, time.Now())
	if w == nil {
		t.Fatal("crush is readable in this build, so Watch must return a watcher")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	crushDB(t, dir, map[string][3]int64{
		"s": {5000, 0, time.Now().Add(-time.Hour).UnixMilli()},
	})
	w.poll(nil)
	if got := w.Sample().Output; got != 0 {
		t.Fatalf("counted tokens from before a successful attach: %d", got)
	}
	putCrushSession(t, dir, "s", 5100, 0, time.Now().Add(time.Second).UnixMilli())
	w.poll(nil)
	if got := w.Sample().Output; got != 100 {
		t.Fatalf("output %d, want the 100 generated after the baseline landed", got)
	}
}

// A watcher's directory is watched under every spelling it can be recorded
// under, and on macOS that always includes a symlinked one. All of them
// address a single database, whose sessions must be summed once.
func TestCrushSourceSumsOneDatabaseOnce(t *testing.T) {
	dir := t.TempDir()
	since := time.Now()
	crushDB(t, dir, map[string][3]int64{"s": {7, 0, since.Add(time.Second).UnixMilli()}})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out, _, ok := crushSessionSum([]string{dir, link}, since)
	if !ok || out != 7 {
		t.Fatalf("sessions output %d (ok=%v) through two spellings of one database", out, ok)
	}
}

// A crush.db (or .crush directory) that is a symlink out of the project
// must not be followed: the store is writable by the agent, so a planted
// link could otherwise pull in another project's sessions as usage.
func TestCrushDBSymlinkOutsideProjectIsIgnored(t *testing.T) {
	dir := t.TempDir()
	skipIfCrushAbove(t, dir)
	outside := t.TempDir()
	crushDB(t, outside, map[string][3]int64{
		"stolen": {9999, 0, time.Now().UnixMilli()},
	})
	outsidePath := filepath.Join(outside, ".crush", "crush.db")
	if err := os.MkdirAll(filepath.Join(dir, ".crush"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		outsidePath,
		filepath.Join(dir, ".crush", "crush.db"),
	); err != nil {
		t.Skip("symlinks:", err)
	}
	// The claim is about the store behind the link, not about finding none:
	// the walk climbs to a project root, so a store a parent of the
	// temporary tree holds is a legitimate find and asserting on it would
	// make the test a statement about the machine rather than about the code.
	// Following the link shows up as a store inside the tree, because the
	// path is built from the project root rather than from the link target.
	if path := crushDBPath(dir); withinTree(path, dir) {
		t.Fatalf("followed a crush.db symlink out of the project: %s", path)
	}
	// The tree is read successfully and the linked store contributes nothing:
	// the refusal is the symlink being skipped, which a failed read would
	// also produce.
	if got, ok := (crushDBSource{}).sessions([]string{dir}, time.Time{}); !ok {
		t.Fatalf("skipping the symlink must not fail the read (sessions %+v)", got)
	} else if sess, found := got[outsidePath]; found {
		t.Fatalf("read outside crush store via file symlink: %+v", sess)
	}

	dir2 := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, ".crush"), filepath.Join(dir2, ".crush")); err != nil {
		t.Fatal(err)
	}
	if path := crushDBPath(dir2); withinTree(path, dir2) {
		t.Fatalf("followed a .crush directory symlink out of the project: %s", path)
	}
	if got, ok := (crushDBSource{}).sessions([]string{dir2}, time.Time{}); !ok {
		t.Fatalf("skipping the symlink must not fail the read (sessions %+v)", got)
	} else if sess, found := got[outsidePath]; found {
		t.Fatalf("read outside crush store via dir symlink: %+v", sess)
	}
}

// crushSessionSum is the sessions snapshot flattened to totals, for tests
// that care about what was recorded rather than per-session identity. ok is
// the source's own: a tree with no crush database is read successfully and
// contributes nothing, which is not the same as a read that failed.
func crushSessionSum(dirs []string, since time.Time) (output, input int, ok bool) {
	got, ok := (crushDBSource{}).sessions(dirs, since)
	if !ok {
		return 0, 0, false
	}
	var out, in int64
	for _, sess := range got {
		for _, c := range sess {
			out += c.output
			in += c.input
		}
	}
	return int(out), int(in), true
}

// Prompt can grow while completion stays put (a follow-up that only billed
// context). Watch must still report Sample.Input, not treat the session as idle.
func TestCrushWatchCountsPromptOnlyGrowth(t *testing.T) {
	dir := t.TempDir()
	crushDB(t, dir, map[string][3]int64{
		"s": {40, 100, time.Now().Add(-time.Hour).UnixMilli()},
	})
	w := Watch("crush", dir, time.Now())
	if w == nil {
		t.Fatal("crush is readable in this build, so Watch must return a watcher")
	}
	putCrushSession(t, dir, "s", 40, 180, time.Now().Add(time.Second).UnixMilli())
	got := w.Poll()
	if got.Output != 0 {
		t.Fatalf("output %d, want 0: completion did not grow", got.Output)
	}
	if got.Input != 80 {
		t.Fatalf("input %d, want the 80 prompt tokens billed after attach", got.Input)
	}
	if got.Empty() {
		t.Fatal("prompt-only growth must not look like an empty sample")
	}
}

// The since predicate must not wrap updated_at: a function around the
// column would make an index on it unusable.
func TestCrushSinceQueryDoesNotWrapUpdatedAt(t *testing.T) {
	if strings.Contains(crushSessionsSinceQuery, "CASE") || strings.Contains(crushSessionsSinceQuery, "* 1000") {
		t.Fatal("since predicate wraps updated_at; an index on it cannot be used")
	}
}

func TestCrushWatchCountsGrowthWithinAttachSecond(t *testing.T) {
	dir := t.TempDir()
	since := time.Date(2026, time.September, 18, 12, 0, 0, 500*int(time.Millisecond), time.UTC)
	crushDB(t, dir, map[string][3]int64{
		"s": {5000, 2000, since.Unix()},
	})
	w := Watch("crush", dir, since)
	if w == nil {
		t.Fatal("crush watcher is unavailable")
	}
	if got := w.Poll(); !got.Empty() {
		t.Fatalf("counted pre-attach usage: %+v", got)
	}
	updated := since.Add(250 * time.Millisecond)
	putCrushSession(t, dir, "s", 5100, 2300, updated.Unix())
	putCrushSession(t, dir, "new", 20, 40, updated.Unix())
	got := w.Poll()
	if got.Output != 120 || got.Input != 340 {
		t.Fatalf("usage %+v, want 120 output and 340 input tokens", got)
	}
	if next := w.Poll(); next.Output != got.Output || next.Input != got.Input {
		t.Fatalf("repeated poll changed usage: %+v, want %+v", next, got)
	}
}

func TestCrushSourceSinceBoundaryInSeconds(t *testing.T) {
	dir := t.TempDir()
	since := time.Unix(1_700_000_000, 500*int64(time.Millisecond))
	crushDB(t, dir, map[string][3]int64{
		"same-second": {10, 0, 1_700_000_000},
		"next-second": {20, 0, 1_700_000_001},
		"millis":      {40, 0, since.UnixMilli()},
	})
	out, _, ok := crushSessionSum([]string{dir}, since)
	if !ok || out != 70 {
		t.Fatalf("output %d (ok=%v), want 70 (same-second + next-second + millis)", out, ok)
	}
}

func TestCrushSinceQueryCanUseUpdatedAtIndex(t *testing.T) {
	dir := t.TempDir()
	sessions := make(map[string][3]int64, 2000)
	base := time.Unix(1_700_000_000, 0)
	for i := range 2000 {
		sessions[fmt.Sprintf("s-%04d", i)] = [3]int64{
			1, 0, base.Add(time.Duration(i) * time.Second).UnixMilli(),
		}
	}
	crushDB(t, dir, sessions)
	path := filepath.Join(dir, ".crush", "crush.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE INDEX idx_sessions_updated_at ON sessions(updated_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}
	since := base.Add(1500 * time.Second)
	ms := since.UnixMilli()
	plan := explainQueryPlan(t, db, crushSessionsSinceQuery, ms, crushMillisCutoff, since.Unix())
	if !strings.Contains(plan, "idx_sessions_updated_at") {
		t.Fatalf("sargable predicate did not use updated_at index:\n%s", plan)
	}
	// Every row of this store is read on some poll, so a branch that scans
	// reads the whole table no matter how well the other branches do. A plan
	// can also name the index and still scan: SQLite plans each branch of a
	// disjunction or a compound separately, and a branch it cannot index is a
	// full scan that the index name elsewhere in the plan does not excuse.
	for _, step := range strings.Split(plan, "\n") {
		if strings.HasPrefix(step, "SCAN sessions") {
			t.Fatalf("a branch of the since predicate scans the table:\n%s", plan)
		}
	}
}

// A store that exists and will not read is polled several times a second, so
// an unlatched audit line is terminal spam for as long as the dashboard runs.
func TestStoreReadFailureIsLatchedPerOutage(t *testing.T) {
	var lines bytes.Buffer
	restore := swapAudit(slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer restore()

	forgetStoreRead("opencode", "/tmp/store")
	auditStoreRead("opencode", "/tmp/store", errors.New("disk image is malformed"))
	auditStoreRead("opencode", "/tmp/store", errors.New("disk image is malformed"))
	if got := strings.Count(lines.String(), "agent usage store read failed"); got != 1 {
		t.Fatalf("repeat failures of one store logged %d lines, want 1", got)
	}

	lines.Reset()
	noteStoreReadOK("opencode", "/tmp/store")
	auditStoreRead("opencode", "/tmp/store", errors.New("disk image is malformed"))
	if got := strings.Count(lines.String(), "agent usage store read failed"); got != 1 {
		t.Fatalf("a failure after recovery logged %d lines, want 1", got)
	}
	forgetStoreRead("opencode", "/tmp/store")
}

// The latch a failure writes is the one the table keeps, so concurrent
// failures of one store log one line even while other stores churn the table
// past its cap. A caller that released the lock between finding the latch and
// writing it could write to an entry the eviction sweep had already dropped,
// and the next failure of that store would read as a new outage.
func TestStoreReadLatchSurvivesEvictionChurn(t *testing.T) {
	var lines lockedBuffer
	restore := swapAudit(slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer restore()

	forgetStoreRead("crush", "/tmp/churned")
	defer forgetStoreRead("crush", "/tmp/churned")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				auditStoreRead("crush", "/tmp/churned", errors.New("disk image is malformed"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range maxStoreReads * 2 {
			storeReadState.Lock()
			storeReadForLocked(fmt.Sprintf("crush\x00/tmp/churn-%d", i))
			storeReadState.Unlock()
		}
	}()
	wg.Wait()

	if got := strings.Count(lines.String(), "agent usage store read failed"); got != 1 {
		t.Fatalf("one store failing under churn logged %d lines, want 1", got)
	}
}

// lockedBuffer collects audit lines from several goroutines at once, which a
// plain bytes.Buffer cannot do.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// forgetStoreRead drops one store's latch, so a test does not inherit the
// state of an earlier one under the shared table.
func forgetStoreRead(agent, path string) {
	key := storeReadKey(agent, path)
	storeReadState.Lock()
	defer storeReadState.Unlock()
	delete(storeReadState.states, key)
	if i := slices.Index(storeReadState.order, key); i >= 0 {
		storeReadState.order = slices.Delete(storeReadState.order, i, i+1)
	}
}

// A project-local store is keyed by the directory the agent ran in, so the key
// set is every project ever seen rather than every project on disk. The table
// has to be capped, or a dashboard left running across a churn of worktrees
// keeps a row for each one forever.
func TestStoreReadLatchTableIsCapped(t *testing.T) {
	forgetStoreRead("crush", "/tmp/capped")
	defer forgetStoreRead("crush", "/tmp/capped")

	for i := range maxStoreReads + 16 {
		storeReadState.Lock()
		storeReadForLocked(storeReadKey("crush", fmt.Sprintf("/tmp/capped/%d", i)))
		storeReadState.Unlock()
	}
	storeReadState.Lock()
	n, order := len(storeReadState.states), len(storeReadState.order)
	storeReadState.Unlock()
	if n > maxStoreReads || order > maxStoreReads {
		t.Fatalf("latch table holds %d entries and %d order rows, want at most %d of each",
			n, order, maxStoreReads)
	}
	if n != order {
		t.Errorf("the map holds %d entries and the order %d rows; every key needs a row to be evictable", n, order)
	}
	// The oldest keys are the ones that fall out, so the cap evicts a
	// forgotten project rather than a store that is still being read.
	noteStoreReadOK("crush", "/tmp/capped/0")
	storeReadState.Lock()
	_, present := storeReadState.states["crush"+"\x00"+"/tmp/capped/0"]
	storeReadState.Unlock()
	if present {
		t.Error("the oldest key survived the cap; eviction must drop from the front of the order")
	}
}

package ingest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maci0/toktop/internal/core"
)

// docs/openapi.yaml is the machine-readable form of the agent feed contract
// the README states in prose. A generator pointed at it builds a client from
// what the handlers do, so a path, a method or an answer the handlers can
// give and the file does not name is a client that cannot handle the server
// in front of it. These tests drive the real server and hold the file to that.

// openapiSection is one path's block of docs/openapi.yaml, from its own
// heading to the next path or to the components section.
func openapiSection(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	heading := "  " + path + ":"
	start := -1
	for i, l := range lines {
		if l == heading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("docs/openapi.yaml documents no path %s", path)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "  /") || lines[i] == "components:" {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// openapiCodes lists the response status codes one path's block declares, the
// way they are written there: a quoted three-digit key indented under an
// operation's responses.
var openapiCodeRE = regexp.MustCompile(`(?m)^ +"([0-9]{3})":`)

// openapiMethods lists the operations one path's block declares, which sit at
// the same indent under the path heading and nowhere else in it.
var openapiMethodRE = regexp.MustCompile(`(?m)^    ([a-z]+):$`)

func openapiCodes(section string) []string {
	matches := openapiCodeRE.FindAllStringSubmatch(section, -1)
	codes := make([]string, 0, len(matches))
	for _, m := range matches {
		codes = append(codes, m[1])
	}
	return codes
}

// The route table is the single source for what is served, so every entry in
// it has to be an operation the spec names, and no path may appear in the
// spec that the table does not serve.
func TestOpenAPIDocumentsEveryServedEndpoint(t *testing.T) {
	for _, e := range ingestEndpoints {
		section := openapiSection(t, e.path)
		methods := openapiMethodRE.FindAllStringSubmatch(section, -1)
		if len(methods) == 0 {
			t.Errorf("%s: docs/openapi.yaml declares no operation", e.path)
			continue
		}
		for _, m := range methods {
			if !e.serves(strings.ToUpper(m[1])) {
				t.Errorf("%s: docs/openapi.yaml documents %s, which the endpoint does not serve (it serves %s)",
					e.path, strings.ToUpper(m[1]), e.allow())
			}
		}
		for _, method := range e.methods {
			if !strings.Contains(section, "\n    "+strings.ToLower(method)+":") {
				t.Errorf("%s %s: docs/openapi.yaml documents no such operation", e.path, method)
			}
		}
	}
}

// Every answer the handlers actually give has to be declared, per operation.
// The statuses are collected by driving the server, not read off the handler,
// so an answer nobody can produce and an answer the spec omits both fail.
func TestOpenAPIDocumentsEveryAnswerTheServerGives(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	events := "http://" + s.Addr() + eventsPath
	health := "http://" + s.Addr() + healthPath

	postAnswers, healthAnswers := map[int]bool{}, map[int]bool{}
	note := func(answers map[int]bool, code int) { answers[code] = true }

	note(postAnswers, post(t, events, `{"agent":"coder","output_tokens":1}`))
	note(postAnswers, post(t, events, `{"agent":"coder","ts":"not-a-stamp"}`))
	note(postAnswers, post(t, events, `{"note":"`+strings.Repeat("x", maxEventBody)+`"}`))

	req, err := http.NewRequest(http.MethodPost, events, strings.NewReader(`{"agent":"coder"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://example.invalid")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	note(postAnswers, resp.StatusCode)

	put, err := http.NewRequest(http.MethodPut, events, nil)
	if err != nil {
		t.Fatal(err)
	}
	putResp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	note(postAnswers, putResp.StatusCode)

	// The Host guard runs before routing, so a rebound request is refused the
	// same way on a path that is not the event one. A client generated from
	// the spec asks /healthz too, so both operations have to name the answer.
	note(postAnswers, rebound(t, http.MethodPost, events))
	note(healthAnswers, rebound(t, http.MethodGet, health))
	note(healthAnswers, rebound(t, http.MethodHead, health))

	// A body that stops mid-stream: the idle bound reaps it with a 408. The
	// bound is a field of the server that reaps it, so this needs its own
	// server rather than a shorter window on the one the rest of this test
	// drives.
	stall := startIngestBody(t, &memRecorder{}, time.Minute, 150*time.Millisecond)
	conn := startPost(t, stall.Addr())
	sendChunk(t, conn, `{"agent":"slow"`)
	stalled := readResponse(t, conn, 5*time.Second)
	conn.Close()
	if !strings.HasPrefix(stalled, "HTTP/1.1 408") {
		t.Fatalf("stalled body answer = %q, want 408", stalled)
	}
	note(postAnswers, http.StatusRequestTimeout)

	// A full eventSlots channel refuses the POST and the probe alike, which is
	// the one 503 both paths give.
	defer swapVar(t, &eventSlots, make(chan struct{}, 1))()
	eventSlots <- struct{}{}
	note(postAnswers, post(t, events, `{"agent":"coder"}`))
	note(healthAnswers, post(t, health, ""))
	<-eventSlots

	note(healthAnswers, http.StatusOK)
	note(healthAnswers, post(t, health, ""))

	postCodes := openapiCodes(openapiSection(t, eventsPath))
	for code := range postAnswers {
		if !containsCode(postCodes, code) {
			t.Errorf("POST %s can answer %d, which docs/openapi.yaml does not declare (it declares %s)",
				eventsPath, code, strings.Join(postCodes, " "))
		}
	}
	healthSection := openapiSection(t, healthPath)
	getSection, headSection := splitOperations(t, healthSection)
	for code := range healthAnswers {
		if !containsCode(openapiCodes(getSection), code) {
			t.Errorf("GET %s can answer %d, which docs/openapi.yaml does not declare (it declares %s)",
				healthPath, code, strings.Join(openapiCodes(getSection), " "))
		}
		// A HEAD carries the GET's answer and no body, so it can only give the
		// statuses the GET gives.
		if !containsCode(openapiCodes(headSection), code) {
			t.Errorf("HEAD %s can answer %d, which docs/openapi.yaml does not declare (it declares %s)",
				healthPath, code, strings.Join(openapiCodes(headSection), " "))
		}
	}
}

// Every maxLength the Event schema declares is the constant the handler bounds
// that field with, and one of them (id) is refused where the rest are clamped.
// A cap that drifts in either file is a client generated against a bound the
// server does not hold, so both halves are pinned here: the number, and which
// side of it the server refuses on.
func TestOpenAPIEventCapsMatchTheBounds(t *testing.T) {
	declared := declaredCaps(t, "Event")
	want := map[string]int{
		"id":         core.AgentIDMax,
		"agent":      core.AgentNameMax,
		"model":      core.AgentModelMax,
		"kind":       core.AgentKindMax,
		"via_engine": core.AgentViaMax,
		"note":       core.AgentNoteMax,
	}
	for field, limit := range want {
		got, ok := declared[field]
		if !ok {
			t.Errorf("the Event schema declares no maxLength for %q, which the handler caps at %d", field, limit)
			continue
		}
		if got != limit {
			t.Errorf("the Event schema caps %q at %d, which the handler bounds at %d", field, got, limit)
		}
	}
	for field := range declared {
		if _, ok := want[field]; !ok {
			t.Errorf("the Event schema caps %q, which the handler does not bound", field)
		}
	}

	// The two rules the caps sit on either side of. wireEventID refuses a
	// past-the-cap id and ClampField drops the rest, so a sender that reads
	// the schema alone has to be told which field is which: a generated
	// client that treats every maxLength as a rejection loses events the
	// server accepts.
	clamped := []string{"agent", "model", "kind", "via_engine", "note"}
	for _, field := range clamped {
		if !strings.Contains(propertySection(t, "Event", field), "clamped, not refused") {
			t.Errorf("the Event schema does not say %q clamps past its cap rather than refusing it", field)
		}
	}
	// The numeric fields clamp out-of-range counts to zero the same way, and
	// the spec's prose carries the ceiling the handler clamps with. The token
	// ceiling is written as a power of two in both the schema and the README;
	// the constant beside it is what makes that spelling checkable.
	props := propertySection(t, "Event", "prompt_tokens")
	if core.MaxEventTokens != 1<<40 || !strings.Contains(props, "2^40") {
		t.Errorf("the Event schema does not name the token ceiling core.MaxEventTokens (%d) spells as 2^40", core.MaxEventTokens)
	}
	if !strings.Contains(propertySection(t, "Event", "span_ms"), strconv.FormatInt(maxSpanMS, 10)) {
		t.Errorf("the Event schema does not name the span bound (%d ms) the handler clamps with", maxSpanMS)
	}
}

// declaredCaps maps each property of one schema to the maxLength it declares.
func declaredCaps(t *testing.T, name string) map[string]int {
	t.Helper()
	caps := map[string]int{}
	for _, prop := range declaredProps(t, name) {
		for _, line := range strings.Split(propertySection(t, name, prop), "\n") {
			v, ok := strings.CutPrefix(strings.TrimSpace(line), "maxLength: ")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("schema %s property %s declares maxLength %q, which is not a number", name, prop, v)
			}
			caps[prop] = n
		}
	}
	return caps
}

// propertySection is one property's block of a schema, from its own key at
// eight spaces to the next key at the same indent.
func propertySection(t *testing.T, schema, prop string) string {
	t.Helper()
	lines := strings.Split(schemaSection(t, schema), "\n")
	heading := "        " + prop + ":"
	start := -1
	for i, l := range lines {
		if l == heading {
			start = i
			continue
		}
		if start >= 0 && strings.HasPrefix(l, "        ") && !strings.HasPrefix(l, "         ") {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		t.Fatalf("schema %s declares no property %q", schema, prop)
	}
	return strings.Join(lines[start:], "\n")
}

// schemaSection is one named block of docs/openapi.yaml's components section,
// from its own heading to the next schema at the same indent.
func schemaSection(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(src), "\n")
	heading := "    " + name + ":"
	start := -1
	for i, l := range lines {
		if l == heading {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("docs/openapi.yaml declares no schema %s", name)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "    ") && !strings.HasPrefix(lines[i], "     ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// openapiProps lists the property names one schema block declares, the way
// they are written there: a bare key at eight spaces, under properties.
var openapiProps = regexp.MustCompile(`(?m)^ {8}([a-z_]+):$`)

// declaredProps returns the properties one schema names, in the order the file
// lists them.
func declaredProps(t *testing.T, name string) []string {
	t.Helper()
	section := schemaSection(t, name)
	_, body, ok := strings.Cut(section, "\n      properties:\n")
	if !ok {
		t.Fatalf("docs/openapi.yaml schema %s declares no properties block", name)
	}
	props := make([]string, 0, 8)
	for _, m := range openapiProps.FindAllStringSubmatch(body, -1) {
		props = append(props, m[1])
	}
	if len(props) == 0 {
		t.Fatalf("docs/openapi.yaml schema %s declares no properties", name)
	}
	return props
}

// The Event schema is what a client generator builds its sender from, and the
// decoder is what actually reads the body. Nothing tied the two together: a
// field added to the struct or dropped from it reached senders as an accepted
// input no document describes, or as a documented one that silently vanishes.
func TestOpenAPIEventSchemaMatchesTheDecoder(t *testing.T) {
	declared := declaredProps(t, "Event")
	for _, name := range wireFields(t) {
		if !slices.Contains(declared, name) {
			t.Errorf("POST /v1/events decodes %q, which the Event schema does not declare; a generated sender cannot send it", name)
		}
	}
	wire := wireFields(t)
	for _, name := range declared {
		if !slices.Contains(wire, name) {
			t.Errorf("the Event schema declares %q, which POST /v1/events does not decode; a generated sender's value is dropped", name)
		}
	}
}

// The same pairing on the other side of the request: the ack the handler writes
// and the Ack schema the file declares. An answer carrying a field the schema
// omits breaks a typed client, and one missing a field it declares leaves a
// client reading a zero it cannot tell from a real count.
func TestOpenAPIAckSchemaMatchesTheAnswer(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	code, body := postBody(t, "http://"+s.Addr()+eventsPath, `{"agent":"coder","output_tokens":7}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST %s answered %d, want 202", eventsPath, code)
	}
	var ack map[string]any
	if err := json.Unmarshal([]byte(body), &ack); err != nil {
		t.Fatalf("ack is not JSON: %v", err)
	}
	declared := declaredProps(t, "Ack")
	for name := range ack {
		if !slices.Contains(declared, name) {
			t.Errorf("the 202 ack carries %q, which the Ack schema does not declare", name)
		}
	}
	for _, name := range declared {
		if _, ok := ack[name]; !ok {
			t.Errorf("the Ack schema declares %q, which the 202 ack does not carry", name)
		}
	}
}

// Every answer is correlated, so a sender can tie one to its audit line, and
// the spec declares X-Request-Id on every status it lists. The header is set
// ahead of routing, so the answers a client handles least, the wrong-method
// and the refused-host ones, are the ones a client would otherwise find
// without it.
func TestEveryAnswerCarriesTheRequestId(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	base := "http://" + s.Addr()
	cases := []struct {
		method, path, host string
	}{
		{http.MethodGet, healthPath, ""},
		{http.MethodGet, eventsPath, ""},
		{http.MethodGet, "/v1/nothing", ""},
		{http.MethodGet, healthPath, "toktop.example.invalid"},
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, base+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.host != "" {
			req.Host = c.host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Request-Id"); got == "" {
			t.Errorf("%s %s answered %d with no X-Request-Id, which docs/openapi.yaml declares on it",
				c.method, c.path, resp.StatusCode)
		}
	}
}

// An operation that declares the X-Request-Id answer header has to accept the
// header as a request parameter too, or a client generated from the spec is
// told its id comes back and given no way to send one. The wrap chain reads
// the header ahead of routing, so the probe and its rejections take one on
// every path; the POST declared it and the probe did not.
func TestRequestIDIsAcceptedWhereItIsEchoed(t *testing.T) {
	for _, e := range ingestEndpoints {
		section := openapiSection(t, e.path)
		for _, op := range splitAllOperations(t, section) {
			if !strings.Contains(op, `X-Request-Id:`) {
				continue
			}
			if !strings.Contains(op, `#/components/parameters/RequestId`) {
				t.Errorf("%s declares the X-Request-Id answer but not the request parameter", opName(e.path, op))
			}
		}
	}
}

// The other half of the same pair, driven rather than read: a sender's own id
// comes back on the probe the way it already comes back on the POST. A probe
// that dropped it would break correlation for the one request a client makes
// continuously, and the spec above promises the echo on both.
func TestProbeEchoesTheRequestID(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, err := http.NewRequest(method, "http://"+s.Addr()+healthPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Request-Id", "probe-7")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Request-Id"); got != "probe-7" {
			t.Errorf("%s /healthz answered X-Request-Id %q, want the request's own", method, got)
		}
	}
}

func containsCode(codes []string, code int) bool {
	for _, c := range codes {
		if c == strconv.Itoa(code) {
			return true
		}
	}
	return false
}

// rebound sends a request addressed to a name that is not this machine, the
// way a page that resolved its own name to 127.0.0.1 does, and returns the
// status the endpoint answers. The listener is loopback-bound, so the Host
// guard is the one that refuses it.
func rebound(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(`{"agent":"coder"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "toktop.example.invalid"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s %s with a foreign Host answered %d, want 403", method, url, resp.StatusCode)
	}
	return resp.StatusCode
}

// splitAllOperations cuts a path block into one string per operation, by the
// method keys the block declares, so a check can run over every operation
// rather than the GET/HEAD pair one path happens to serve.
func splitAllOperations(t *testing.T, section string) []string {
	t.Helper()
	lines := strings.Split(section, "\n")
	start := map[string]int{}
	for i, l := range lines {
		if m := openapiMethodRE.FindStringSubmatch(l); m != nil {
			start[m[1]] = i
		}
	}
	if len(start) == 0 {
		t.Fatalf("no operation found in the section")
	}
	keys := make([]string, 0, len(start))
	for k := range start {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ops := make([]string, 0, len(keys))
	for n, key := range keys {
		end := len(lines)
		if n+1 < len(keys) {
			end = start[keys[n+1]]
		}
		ops = append(ops, strings.Join(lines[start[key]:end], "\n"))
	}
	return ops
}

// opName names one operation for a failure message, by the path it was cut
// from and the method heading it starts with.
func opName(path, op string) string {
	if m := openapiMethodRE.FindStringSubmatch(op); m != nil {
		return path + " " + strings.ToUpper(m[1])
	}
	return path
}

// splitOperations cuts a path block into its GET and HEAD operations, by the
// method keys the same block declares.
func splitOperations(t *testing.T, section string) (get, head string) {
	t.Helper()
	lines := strings.Split(section, "\n")
	start := map[string]int{}
	for i, l := range lines {
		if m := openapiMethodRE.FindStringSubmatch(l); m != nil {
			start[m[1]] = i
		}
	}
	gi, hi := start["get"], start["head"]
	if gi < 0 || hi < 0 {
		t.Fatalf("%s: docs/openapi.yaml declares no get/head pair", healthPath)
	}
	if gi > hi {
		gi, hi = hi, gi
	}
	return strings.Join(lines[gi:hi], "\n"), strings.Join(lines[hi:], "\n")
}

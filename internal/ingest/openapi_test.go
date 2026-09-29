package ingest

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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

	// A body that stops mid-stream: the idle bound reaps it with a 408.
	oldIdle := bodyIdleTimeout
	bodyIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { bodyIdleTimeout = oldIdle })
	conn := startPost(t, s.Addr())
	sendChunk(t, conn, `{"agent":"slow"`)
	stalled := readResponse(t, conn, 5*time.Second)
	conn.Close()
	bodyIdleTimeout = oldIdle
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

func containsCode(codes []string, code int) bool {
	for _, c := range codes {
		if c == strconv.Itoa(code) {
			return true
		}
	}
	return false
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

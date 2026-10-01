package ingest

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every answer a handler gives is a fact about one request, and
// setSecurityHeaders marks them uncacheable ahead of routing so a shared cache
// cannot hand one sender another run's state. docs/openapi.yaml names the
// header on every status it lists, because a client generated from the spec
// decides what to cache from what it can see, and a header the spec omits is
// one it will cache: the acknowledged counts, the degraded 503 and the
// endpoint list on a 404 all read as this run's own answer to the next.
//
// The same chain sets the security headers, which the spec's server
// description states by name. Both halves are pinned here: the values the
// server sets, and that the description still spells each one.
//
// The two refusals net/http makes before a handler runs are the exception, and
// the spec says so on RuntimeRefusal and RuntimeBadRequest: they never reach
// the chain that sets it.
func TestEveryAnswerIsMarkedUncacheable(t *testing.T) {
	s := startIngest(t, &memRecorder{})
	base := "http://" + s.Addr()
	cases := []struct{ method, path string }{
		{http.MethodGet, healthPath},
		{http.MethodHead, healthPath},
		{http.MethodPost, eventsPath},
		{http.MethodGet, eventsPath},    // 405
		{http.MethodGet, "/v1/nothing"}, // 404
	}
	for _, c := range cases {
		req, err := http.NewRequest(c.method, base+c.path, strings.NewReader(`{"agent":"coder"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s answered %d with Cache-Control %q, want no-store",
				c.method, c.path, resp.StatusCode, got)
		}
		// The rest of the set rides along with it on every one of these,
		// refusals included, since the chain sets them ahead of routing.
		set := http.Header{}
		setSecurityHeaders(set)
		for _, name := range securityHeaderNames {
			if got := resp.Header.Get(name); got != set.Get(name) {
				t.Errorf("%s %s answered %d with %s %q, want %q",
					c.method, c.path, resp.StatusCode, name, got, set.Get(name))
			}
		}
	}

	// The runtime refusals never reach the chain, so they carry no
	// Cache-Control and no X-Request-Id. The spec says so on both of them,
	// and the headers appearing here are what would make it wrong.
	for _, request := range []string{
		"POST " + eventsPath + " HTTP/1.1\r\nHost: localhost\r\nX-Request-Id: " +
			strings.Repeat("x", maxHeaderBytes*2) + "\r\nContent-Length: 0\r\n\r\n",
		"NOT-A-REQUEST-LINE\r\n\r\n",
	} {
		head := rawHead(t, s, request)
		if strings.Contains(strings.ToLower(head), "cache-control") {
			t.Errorf("a refusal made before the handler chain carries Cache-Control:\n%s", head)
		}
	}

	// Every header the chain sets is named in the spec's server description
	// with the value it sets, so a value that changes and a header that is
	// dropped both show up as a description that no longer matches the
	// answer. Cache-Control is read off the statuses instead, and has its own
	// component header.
	set := http.Header{}
	setSecurityHeaders(set)
	described := openapiDescription(t)
	for _, name := range securityHeaderNames {
		if !strings.Contains(described, "`"+name+": "+set.Get(name)+"`") {
			t.Errorf("the spec's server description does not state %s: %s", name, set.Get(name))
		}
	}

	// The header is declared on every status the file lists, and it is
	// declared as the value the chain sets rather than as a prose note a
	// client cannot branch on. The runtime refusals are the exception, and are
	// recognised as ones by what they reference rather than by their status
	// code: neither carries X-Request-Id or Cache-Control, because neither
	// reaches the chain that sets them. Both are checked on their own above.
	for _, e := range ingestEndpoints {
		section := openapiSection(t, e.path)
		for _, code := range openapiCodes(section) {
			if isRuntimeRefusal(section, code) {
				continue
			}
			if !declaresHeader(section, code, "CacheControl") {
				t.Errorf("%s declares %s without the Cache-Control answer header", e.path, code)
			}
		}
	}
}

// runtimeRefusals are the two answers net/http gives before the handler chain
// runs: the header budget and the unparseable request line. A status block
// that is a bare $ref to either is one of them.
var runtimeRefusals = []string{
	"#/components/responses/RuntimeRefusal",
	"#/components/responses/RuntimeBadRequest",
}

// isRuntimeRefusal reports whether one status's response block is a $ref to a
// runtime refusal rather than a block the handlers answer.
//
// Keyed off the $ref rather than off the status code, because a status can be
// both: POST /v1/events answers a 400 of its own, naming the field the sender
// got wrong and carrying every header, and it is also where the runtime
// answers a 400 carrying no field and no header at all. Exempting "431", and
// then "400" beside it, would exempt the handler's 400 too and let the spec
// drop the header from an answer every sender does receive.
func isRuntimeRefusal(section, code string) bool {
	block := responseBlock(section, code)
	for _, ref := range runtimeRefusals {
		if strings.Contains(block, ref) {
			return true
		}
	}
	return false
}

// securityHeaderNames are the answers setSecurityHeaders puts on every
// response, minus the two the spec declares on the statuses themselves:
// Cache-Control, which a client branches on and so names per answer, and
// X-Request-Id, which is a per-request correlation id rather than a policy.
var securityHeaderNames = []string{
	"X-Content-Type-Options",
	"X-Frame-Options",
	"Content-Security-Policy",
	"Cross-Origin-Resource-Policy",
	"Referrer-Policy",
}

// openapiDescription is docs/openapi.yaml's info.description, the server
// description every answer's headers are stated on.
func openapiDescription(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(src), "\n  description: |\n")
	if !ok {
		t.Fatal("docs/openapi.yaml declares no info.description block scalar")
	}
	lines := strings.Split(body, "\n")
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !strings.HasPrefix(l, "    ") {
			break
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		t.Fatal("docs/openapi.yaml's info.description is empty")
	}
	// The description is a folded block scalar: a sentence runs across several
	// indented lines, and a header's value is split across two of them where
	// the width runs out. Unwrapping joins those back into the single run a
	// reader sees, which is what the containment below is written against.
	return strings.Join(strings.Fields(strings.Join(out, " ")), " ")
}

// declaresHeader reports whether one status's response block names a component
// answer header. It cuts on the quoted three-digit keys the file writes
// responses under, the way openapiCodes finds them, so a status declared under
// two operations is read once per declaration.
func declaresHeader(section, code, header string) bool {
	return strings.Contains(responseBlock(section, code), "#/components/headers/"+header)
}

// responseBlock is one status's response block of a path's spec section, from
// its quoted three-digit key to the next status or the end of the section, and
// empty for a status the section does not declare. The keys sit at eight
// spaces of indent under an operation's responses, and a nine-space key is a
// header of that status rather than a status of its own.
func responseBlock(section, code string) string {
	lines := strings.Split(section, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == `"`+code+`":` {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if l := lines[i]; strings.HasPrefix(l, "        \"") && !strings.HasPrefix(l, "         \"") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// rawHead writes a request verbatim and returns the header block of the
// answer, for the refusals a client library cannot send and read.
func rawHead(t *testing.T, s *Server, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(readResponse(t, conn, 5*time.Second), "\r\n\r\n")
	return head
}

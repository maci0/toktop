package bearer

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusErrorQuotesABoundedSnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "engine is warming up")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer DrainAndClose(resp.Body)

	err = StatusError(srv.URL+"/v1/models", resp)
	if err == nil {
		t.Fatal("StatusError returned nil for a 503")
	}
	if want := srv.URL + "/v1/models: http 503 Service Unavailable: engine is warming up"; err.Error() != want {
		t.Errorf("StatusError = %q, want %q", err, want)
	}
}

func TestStatusErrorOmitsAnEmptySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer DrainAndClose(resp.Body)

	if got := StatusError(srv.URL, resp).Error(); strings.HasSuffix(got, ": ") {
		t.Errorf("StatusError = %q, want no trailing snippet separator", got)
	}
}

// errBody is a body whose transfer stops partway: the bytes before the
// failure are a fragment rather than what the engine said.
type errBody struct{ err error }

func (b errBody) Read([]byte) (int, error) { return 0, b.err }
func (errBody) Close() error               { return nil }

func TestStatusErrorWrapsATruncatedTransfer(t *testing.T) {
	stop := errors.New("unexpected EOF")
	resp := &http.Response{
		Status:     "502 Bad Gateway",
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(io.MultiReader(strings.NewReader("half a body"), errBody{stop})),
	}
	err := StatusError("http://engine:8000/v1/models", resp)
	if !errors.Is(err, stop) {
		t.Fatalf("StatusError = %v, want it to wrap the read failure", err)
	}
	if !strings.HasPrefix(err.Error(), "http://engine:8000/v1/models: http 502 Bad Gateway: half a body: ") {
		t.Errorf("StatusError = %q, want the fragment quoted in the message", err)
	}
}

// endlessBody answers every read, so a drain of it can only stop at the cap.
type endlessBody struct{ read int }

func (b *endlessBody) Read(p []byte) (int, error) {
	n := len(p)
	b.read += n
	return n, nil
}
func (b *endlessBody) Close() error { return nil }

func TestDrainAndCloseStopsAtTheCap(t *testing.T) {
	body := &endlessBody{}
	DrainAndClose(body)
	if body.read > DrainCap {
		t.Errorf("drain read %d bytes, want at most the cap %d", body.read, DrainCap)
	}
	if body.read < DrainCap {
		t.Errorf("drain read %d bytes, want it to run to the cap %d", body.read, DrainCap)
	}
}

func TestDrainAndCloseFinishesAShortBody(t *testing.T) {
	body := &countingBody{remaining: 32}
	DrainAndClose(body)
	if body.remaining != 0 {
		t.Errorf("%d bytes left unread; a body closed short of EOF is not reused", body.remaining)
	}
	if !body.closed {
		t.Error("DrainAndClose returned without closing the body")
	}
}

type countingBody struct {
	remaining int
	closed    bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	b.remaining -= n
	return n, nil
}
func (b *countingBody) Close() error { b.closed = true; return nil }

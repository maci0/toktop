// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// The response-body handling the engine clients share: reading out the tail of
// a body the caller stopped reading so net/http can reuse the connection, and
// rendering the failure a non-200 response carries. Both the polling and the
// discovery clients need them, and the two spellings they had drifted into one
// another, so one definition of each lives here.

package bearer

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/maci0/toktop/internal/core"
)

// DrainCap bounds the tail DrainAndClose and StatusError throw away so a
// connection can be reused. The bytes go to io.Discard, so this bounds time
// rather than memory: an endpoint streaming an endless body would otherwise
// hold the call on a read that answers nothing. Past the cap the connection is
// simply not reused, which is what closing an undrained body did anyway.
const DrainCap = 64 << 10

// DrainAndClose reads out the tail of a body the caller stopped reading and
// then closes it. net/http only returns a connection to the idle pool when its
// body is closed at EOF, so a decode that stops at the end of the JSON value
// has to finish the transfer here, and so does a status line that read a
// bounded snippet of a body it then abandoned.
func DrainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, DrainCap))
	body.Close()
}

// StatusError renders the failure a non-200 engine response carries: the
// request URL, the status, and a bounded snippet of the body when it has one.
// A body that stopped partway is a fragment rather than what the engine said,
// so the cause is wrapped rather than spelled into the text, and a caller can
// still tell a truncated transfer from a bad payload.
func StatusError(url string, resp *http.Response) error {
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 4*core.SnippetCap))
	msg := fmt.Sprintf("%s: http %s", url, core.HTTPStatus(resp.Status))
	if s := core.Snippet(b); s != "" {
		msg += ": " + s
	}
	if rerr != nil {
		return fmt.Errorf("%s: %w", msg, rerr)
	}
	return errors.New(msg)
}

// Package selfreload watches the running executable and signals when it has
// been rebuilt. On Unix, Restart re-execs into the new image so `go build`
// gives an instantly fresh dashboard. Windows cannot replace a running image,
// so Restart prints that and the caller exits.
package selfreload

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/maci0/toktop/internal/logcfg"
)

type identity struct {
	dev, ino   uint64
	size       int64
	mtimeNanos int64
}

// Watch polls the executable's identity and calls onChange once, the first
// time it changes, then returns. It never fires for the initial stat.
//
// A stat that fails leaves nothing to compare, so the loop keeps ticking
// without ever firing: a rebuild that lands in that window is missed and the
// session runs the old image for as long as it lasts. The transition is
// reported rather than dropped, and reported again when it clears, so an
// operator sees that the reload is off instead of a dashboard that just never
// refreshes.
func Watch(ctx context.Context, exePath string, interval time.Duration, onChange func()) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var (
		prev  identity
		first = true
		down  bool
	)
	for {
		if ctx.Err() != nil {
			return
		}
		id, err := statIdentity(exePath)
		switch {
		case err != nil && !down:
			down = true
			audit().Warn("toktop: cannot read the running executable, so a rebuild will not be picked up",
				"path", exePath, "error", err)
		case err == nil && down:
			down = false
			audit().Info("toktop: the running executable is readable again")
		}
		if err == nil {
			if first {
				prev = id
				first = false
			} else if id != prev {
				onChange()
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// audit builds the logger these lines go to. A test swaps auditFn for a
// handler it can read; the swap and the read take the same lock, so a poll
// goroutine from an earlier test cannot reach the next test's handler.
var (
	auditMu sync.Mutex
	auditFn = logcfg.Logger
)

func audit() *slog.Logger {
	auditMu.Lock()
	fn := auditFn
	auditMu.Unlock()
	return fn()
}

func statIdentity(path string) (identity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return identity{}, err
	}
	dev, ino := fileID(path, fi)
	return identity{
		dev:        dev,
		ino:        ino,
		size:       fi.Size(),
		mtimeNanos: fi.ModTime().UnixNano(),
	}, nil
}

// Package selfreload watches the running executable and signals when it has
// been rebuilt. On Unix, Restart re-execs into the new image so `go build`
// gives an instantly fresh dashboard. Windows cannot replace a running image,
// so Restart prints that and the caller exits.
package selfreload

import (
	"context"
	"os"
	"time"
)

type identity struct {
	dev, ino   uint64
	size       int64
	mtimeNanos int64
}

// Watch polls the executable's identity and calls onChange exactly once per
// rebuild. It never fires for the initial stat.
func Watch(ctx context.Context, exePath string, interval time.Duration, onChange func()) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	var prev identity
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		id, err := statIdentity(exePath)
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

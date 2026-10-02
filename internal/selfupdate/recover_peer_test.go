// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The displaced-binary recovery applyTo runs before its download must not race
// a peer install. restoreDisplaced decides what to do by stat-ing the
// installed path: missing means "a killed update left the binary aside, put it
// back". A peer between installDisplacing's two renames has made the installed
// path missing on purpose, and a second run reading that same missing path
// concludes the first one was killed and renames the peer's displaced file
// back over it. The install lock exists so a peer does not replace the binary
// mid-replace, and a recovery that renames the binary is a replace.
//
// The peer here holds the real install lock for the whole of its replace, so
// the second run is not racing an unlocked install: it is doing exactly what
// two `toktop update` runs in two terminals do.
func TestApplyRecoveryWaitsForAPeerHoldingTheInstallLock(t *testing.T) {
	payload := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(payload)
	name := AssetName("9.9.9")
	sums := checksumsArchive(t, hex.EncodeToString(sum[:])+"  "+name+"\n")
	allowTestAssetURLs(t)

	var assetRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		assetRequests.Add(1)
		w.Write(payload)
	})
	mux.HandleFunc("/checksums", func(w http.ResponseWriter, r *http.Request) { w.Write(sums) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &Release{TagName: "v9.9.9"}
	body := `{"tag_name":"v9.9.9","assets":[
		{"name":"` + name + `","browser_download_url":"` + srv.URL + `/asset"},
		{"name":"` + checksumsName("9.9.9") + `","browser_download_url":"` + srv.URL + `/checksums"}]}`
	if err := json.Unmarshal([]byte(body), rel); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	self := filepath.Join(dir, "toktop")
	old := []byte("old binary")
	if err := os.WriteFile(self, old, 0o755); err != nil {
		t.Fatal(err)
	}

	// The peer takes the install lock the way a real install does, then holds
	// it. The installed path is left missing for the duration: that is the
	// state installDisplacing creates between its two renames, and it is what
	// the second run's recovery reads.

	// Set before any goroutine starts: the peer and the second run both read
	// it, so writing it under them would be a race rather than a test.
	wait := installLockWait
	installLockWait = 5 * time.Second
	t.Cleanup(func() { installLockWait = wait })

	releasePeer := make(chan struct{})
	peerDone := make(chan error, 1)
	go func() {
		peerDone <- lockInstall(self, func() error {
			if err := os.Rename(self, self+displacedSuffix); err != nil {
				return err
			}
			<-releasePeer
			return os.Rename(self+displacedSuffix, self)
		})
	}()

	// Wait for the peer to be inside its replace rather than racing it into the
	// lock queue: the second run has to reach its recovery to be the thing
	// under test.
	for range 2000 {
		if _, err := os.Stat(self); err != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(self); err == nil {
		close(releasePeer)
		<-peerDone
		t.Fatal("the peer never reached the state under test")
	}

	applied := make(chan error, 1)
	go func() {
		_, err := applyTo(context.Background(), rel, self)
		applied <- err
	}()

	// The second run has to reach the lock while the peer still holds it, and
	// only then does the peer finish. Unblocking the peer as soon as it starts
	// would let it win the race before the second run ever read the missing
	// path, and the test would pass against the unguarded recovery too.
	time.Sleep(300 * time.Millisecond)
	close(releasePeer)
	if err := <-peerDone; err != nil {
		// The unguarded recovery shows up here: it renames the peer's
		// displaced binary back while the peer is inside its own section, so
		// the peer's own rename has nothing to move.
		t.Fatalf("peer install under a second run: %v", err)
	}
	if err := <-applied; err != nil {
		t.Fatalf("applyTo under a peer install: %v", err)
	}

	// The second run waited its turn and then installed the release over the
	// peer's restored binary, which is what a run that found the release
	// already installed does too. What matters is that it did so by its own
	// install rather than by renaming the peer's displaced file: the peer's
	// replace has to have succeeded for the binary to be here at all.
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("installed binary after the peer finished: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed binary = %q, want the release the second run verified", got)
	}
}

// A recovery that waits on the lock must still run when it is the recovery a
// killed update left behind, which is the state its own comment describes:
// the installed path is missing, the displaced one is the only copy, and
// nothing holds the lock. The wait cannot become a refusal to recover.
func TestApplyRecoveryRestoresWithoutAPeer(t *testing.T) {
	payload := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(payload)
	name := AssetName("9.9.9")
	sums := checksumsArchive(t, hex.EncodeToString(sum[:])+"  "+name+"\n")
	allowTestAssetURLs(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/checksums", func(w http.ResponseWriter, r *http.Request) { w.Write(sums) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &Release{TagName: "v9.9.9"}
	body := `{"tag_name":"v9.9.9","assets":[
		{"name":"` + name + `","browser_download_url":"` + srv.URL + `/asset"},
		{"name":"` + checksumsName("9.9.9") + `","browser_download_url":"` + srv.URL + `/checksums"}]}`
	if err := json.Unmarshal([]byte(body), rel); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	self := filepath.Join(dir, "toktop")
	old := []byte("the only copy")
	if err := os.WriteFile(self+displacedSuffix, old, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := applyTo(context.Background(), rel, self); err != nil {
		t.Fatalf("applyTo recovering a displaced binary: %v", err)
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed binary = %q, want the release", got)
	}
}

// The lock the recovery holds is released on every path out of it, including
// the one where the displaced file is refused. A lock left behind wedges every
// later update, and the install path's release is proved by a different test.
func TestRecoveryReleasesTheInstallLockOnRefusal(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "toktop")
	// A displaced path that is not a regular file is refused rather than
	// promoted, so the recovery returns an error. The lock it was taken under
	// still has to come off the path out.
	if err := os.Mkdir(self+displacedSuffix, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := lockInstall(self, func() error { return restoreDisplaced(self, self+displacedSuffix) }); err == nil {
		t.Fatal("a directory at the displaced path was promoted")
	}

	if _, err := os.Stat(self + installLockSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lock file = %v, want it released", err)
	}
}

// The concurrency the two runs above describe is the one installIfChanged is
// built for on the install path; this pins that the recovery waiting for the
// lock and the install under it compose rather than deadlock, with the peer
// holding the lock through a whole replace.
func TestApplyRecoveryAndInstallSerialize(t *testing.T) {
	payload := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(payload)
	name := AssetName("9.9.9")
	sums := checksumsArchive(t, hex.EncodeToString(sum[:])+"  "+name+"\n")
	allowTestAssetURLs(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
	mux.HandleFunc("/checksums", func(w http.ResponseWriter, r *http.Request) { w.Write(sums) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &Release{TagName: "v9.9.9"}
	body := `{"tag_name":"v9.9.9","assets":[
		{"name":"` + name + `","browser_download_url":"` + srv.URL + `/asset"},
		{"name":"` + checksumsName("9.9.9") + `","browser_download_url":"` + srv.URL + `/checksums"}]}`
	if err := json.Unmarshal([]byte(body), rel); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	self := filepath.Join(dir, "toktop")
	if err := os.WriteFile(self, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var errs [4]error
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = applyTo(context.Background(), rel, self)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed binary = %q, want the release", got)
	}
	if _, err := os.Stat(self + installLockSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lock file = %v, want it released", err)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), updateTempPrefix) {
				t.Errorf("staging file %q left beside the binary", e.Name())
			}
		}
	}
}

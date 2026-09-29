// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxChecksumsDecoded bounds decompressed checksums-archive bytes. The
// compressed fetch is already maxChecksumsArchive; without a decoded cap a gzip
// bomb inside that envelope would expand while the tar walker skipped non-
// matching members.
const maxChecksumsDecoded = 2 << 20

// maxChecksumsListing bounds the checksums.txt member itself, which every
// other member of the archive is skipped past.
const maxChecksumsListing = 1 << 20

// fileChecksum is the hex SHA-256 of path, capped the same way a download is
// so a huge existing file cannot fill memory on the "already current" check.
func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxAssetBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxAssetBytes {
		return "", fmt.Errorf("file %s exceeds %d bytes", path, maxAssetBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ChecksumListing pulls checksums.txt out of the tar.gz the release ships it
// in. Entries are matched by base name, so a wrapper directory around the
// file does not matter; everything else in the archive is skipped.
func ChecksumListing(archive []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxChecksumsDecoded))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("no checksums.txt in the archive")
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(h.Name) != "checksums.txt" {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxChecksumsListing))
		if err != nil {
			return "", err
		}
		return string(body), nil
	}
}

// ChecksumFor finds one file's expected hash in a `sha256sum` style listing
// ("<hex>  <name>", with an optional binary-mode asterisk).
func ChecksumFor(listing, name string) (string, bool) {
	for line := range strings.SplitSeq(listing, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		sum, file := fields[0], strings.TrimPrefix(fields[1], "*")
		if filepath.Base(file) != name || len(sum) != 64 {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		return strings.ToLower(sum), true
	}
	return "", false
}

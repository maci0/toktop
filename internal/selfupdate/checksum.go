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
		// The line ending goes with the line. A listing written on Windows, or
		// by any tool that emits CRLF, carries the carriage return as the last
		// byte of the name, where it is part of no file the release ships and
		// matches no asset.
		sum, file, ok := checksumRecord(strings.TrimSuffix(line, "\r"))
		if !ok {
			continue
		}
		if filepath.Base(file) != name {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		return strings.ToLower(sum), true
	}
	return "", false
}

// checksumRecord reads the hash and the file name off one `sha256sum` line.
//
// The hash field is fixed at 64 characters and the separator that follows it
// is part of the format, not whitespace between two words: `sha256sum` writes
// the hash, one space, then either a second space (text mode) or an asterisk
// (binary mode), then the name. Reading the fields by that shape is what lets
// a name hold a space.
//
// Splitting the line on whitespace instead, as strings.Fields does, breaks on
// every rune Unicode calls whitespace and treats the line as two fields either
// way: a release asset called "toktop 1.2.3_linux_amd64.tar.gz" produced
// three fields, the line was skipped, and ChecksumFor reported no hash for an
// asset the listing names in full. The update was then refused as a checksum
// mismatch against a hash that was sitting in the file. An asset name carrying
// a no-break space or an ideographic space failed the same way.
func checksumRecord(line string) (sum, file string, ok bool) {
	const hexLen = 64
	i := skipBlanks(line, 0)
	start := i
	for i < len(line) && !isBlank(line[i]) {
		i++
	}
	sum = line[start:i]
	if len(sum) != hexLen || i == len(line) {
		return "", "", false
	}
	i++ // the one blank sha256sum writes after the hash, in both modes
	// The separator is the last byte of the line, so there is no name to read
	// and nothing left to inspect the mode from: a listing line that ends
	// right after it is a truncated record, not one with an empty name.
	if i == len(line) {
		return "", "", false
	}
	switch {
	case line[i] == '*': // binary mode
		i++
	case isBlank(line[i]): // text mode's second blank
		i++
	default:
		return "", "", false // the hash is glued to the name; not a record
	}
	// The name is the rest of the line, so it keeps every blank inside it.
	// Leading ones are dropped: a name that starts with a space is
	// indistinguishable from the format's own separator, which is the one
	// ambiguity sha256sum itself does not resolve either.
	name := line[skipBlanks(line, i):]
	if name == "" {
		return "", "", false // a separator with no name is a truncated record
	}
	return sum, name, true
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' || c == '\r' }

func skipBlanks(s string, i int) int {
	for i < len(s) && isBlank(s[i]) {
		i++
	}
	return i
}

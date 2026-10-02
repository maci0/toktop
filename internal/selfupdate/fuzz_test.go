// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"strings"
	"testing"
)

// FuzzChecksumListing drives the self-update verification parsers:
// ChecksumListing unpacks whatever GitHub served as the checksums tar.gz,
// then ChecksumFor picks one file's hash from that listing. A hostile or
// truncated archive must not panic, hang, or return a sum that is not a
// 64-character lowercase digest; a sum that is accepted must still be
// accepted when written back as a sha256sum line.
func FuzzChecksumListing(f *testing.F) {
	const (
		asset    = "toktop_1.2.3_linux_amd64"
		emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	)
	listing := emptySHA + "  " + asset + "\n" +
		strings.ToUpper(emptySHA) + " *" + asset + ".exe\n"

	for _, seed := range []struct {
		data []byte
		name string
	}{
		{gzipTar(f, "checksums.txt", []byte(listing)), asset},
		{gzipTar(f, "toktop_1.2.3_checksums/checksums.txt", []byte(listing)), asset},
		{gzipTarMembers(f, []tarMember{
			{Name: "README", Body: []byte("not the listing\n")},
			{Name: "checksums.txt", Body: []byte(listing)},
		}), asset},
		{gzipTar(f, "checksums.txt", nil), asset},
		{gzipTar(f, "SHA256SUMS", []byte(listing)), asset},
		{[]byte(listing), asset},
		{[]byte(emptySHA + "  " + asset + "\n"), asset},
		{[]byte(emptySHA + " *" + asset + "\n"), asset},
		{[]byte(strings.ToUpper(emptySHA) + "  " + asset + "\n"), asset},
		{[]byte(emptySHA[:63] + "  " + asset + "\n"), asset},
		{[]byte(emptySHA + "x  " + asset + "\n"), asset},
		{[]byte(emptySHA + "  " + asset + " extra\n"), asset},
		{[]byte("not a hash  " + asset + "\n"), asset},
		// A line whose separator is its last byte. The record reader steps
		// past the separator before it inspects the mode, so these are the
		// lines that reach a name that is not there at all; they read the same
		// with a tab as with a space, and the binary-mode marker at the end of
		// a line is the one spelling that still names an empty file.
		{[]byte(emptySHA + " "), asset},
		{[]byte(emptySHA + "\t"), asset},
		{[]byte(emptySHA + " \r"), asset},
		{[]byte(emptySHA + " *"), asset},
		{[]byte(emptySHA + "  "), asset},
		{[]byte(emptySHA), asset},
		{[]byte(""), asset},
		{[]byte("\x1f\x8b"), asset},
		{[]byte("not gzip at all"), "toktop"},
		{[]byte("checksums.txt"), ""},
		{[]byte(emptySHA + "  ../" + asset + "\n"), asset},
		{[]byte("e3b0c44298fc1c149afbf4c8996fb9\x7f\x00\x00\x00a\xee41e4649b934ca495991b7852b855 *" + asset + "\n"), asset},
	} {
		f.Add(seed.data, seed.name)
	}

	f.Fuzz(func(t *testing.T, data []byte, name string) {
		listing, err := ChecksumListing(data)
		if err != nil && listing != "" {
			t.Fatalf("ChecksumListing returned a listing with an error: %q %v", listing, err)
		}
		listing2, err2 := ChecksumListing(data)
		if listing2 != listing || (err2 == nil) != (err == nil) {
			t.Fatal("ChecksumListing is not deterministic")
		}

		for _, text := range []string{string(data), listing} {
			sum, ok := ChecksumFor(text, name)
			if ok {
				if len(sum) != 64 || sum != strings.ToLower(sum) {
					t.Fatalf("ChecksumFor(%q) = %q, want 64 lowercase characters", name, sum)
				}
				if _, err := hex.DecodeString(sum); err != nil {
					t.Fatalf("ChecksumFor(%q) = %q, not hex: %v", name, sum, err)
				}
				if !strings.HasPrefix(name, "*") {
					again, okAgain := ChecksumFor(sum+"  "+name+"\n", name)
					if !okAgain || again != sum {
						t.Fatalf("ChecksumFor dropped a sum it accepted: name=%q sum=%q", name, sum)
					}
				}
			} else if sum != "" {
				t.Fatalf("ChecksumFor rejected but returned %q", sum)
			}
			sum2, ok2 := ChecksumFor(text, name)
			if sum2 != sum || ok2 != ok {
				t.Fatal("ChecksumFor is not deterministic")
			}
		}
	})
}

type tarMember struct {
	Name string
	Body []byte
}

func gzipTar(f *testing.F, name string, body []byte) []byte {
	f.Helper()
	return gzipTarMembers(f, []tarMember{{Name: name, Body: body}})
}

func gzipTarMembers(f *testing.F, members []tarMember) []byte {
	f.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Name: m.Name, Mode: 0o644, Size: int64(len(m.Body))}); err != nil {
			f.Fatal(err)
		}
		if _, err := tw.Write(m.Body); err != nil {
			f.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		f.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		f.Fatal(err)
	}
	return buf.Bytes()
}
